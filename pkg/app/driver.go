package app

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/mcp"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/refs"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/subagent"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
	tuisink "github.com/jentfoo/ajent/pkg/tui/sink"
)

const secretPrefix = "secret:"

// Driver runs the real agent loop: it builds an Agent over the registry and
// drives turns from submitted messages, steering mid-turn input into the running
// turn rather than starting a second one. sessMode decides whether this run starts
// fresh or resumes a saved transcript; sessTarget names it for the id and name modes.
func Driver(ui *tui.UI, set *config.Set, reg *llm.Registry, active llm.Model, sessMode ResumeMode, sessTarget string, args []string) string {
	providers := llm.NewProviders(reg)
	rc := llm.ReasoningFrom(set.Settings().Reasoning, active)
	st := &agent.State{
		Model:     active,
		Reasoning: rc,
		Tokens:    tokens.New(active),
	}

	// every turn is recorded into the workspace transcript so double-Esc while idle
	// can open the context-tree picker and rewind onto an earlier point.
	rec := newSession(ui, sessMode, sessTarget, active.Key())
	if rec == nil {
		ui.Notify("session recording disabled; Esc will not rewind", tui.LevelWarn)
	}

	sink := tuisink.New(ui)

	// build the built-in tool registry and hand it to the loop so the model can
	// read, write, edit and run commands.
	// ask_user rides the TUI's question queue, so it never pre-empts a permission
	// dialog. It stays disabled until a workflow enables it.
	toolsReg, terr := tools.Builtins(tools.Options{SessionID: config.Cwd(), Ask: askUser(ui)})
	if terr != nil {
		ui.Notify("tools disabled: "+terr.Error(), tui.LevelWarn)
	}
	// a configured enabled set replaces the built-in default (read/write/edit/bash),
	// so find/grep/ls come on when config.json lists them.
	if toolsReg != nil && len(set.Settings().Tools.Enabled) > 0 {
		toolsReg.SetEnabled(set.Settings().Tools.Enabled)
	}

	// the compactor is wired lazily so the agent options can close over it before
	// the *Agent it needs exists; it is assigned once, right after agent.New.
	var comp *compactor
	env := agent.DetectEnvironment()
	// user-global instructions layer before the project's, so the more specific
	// cwd file comes later in context; an unresolvable home is skipped silently.
	globalDir, _ := config.Home()
	proj, perr := agent.LoadProjectInstructions(globalDir, env.Cwd)
	if perr != nil {
		ui.Notify("could not read AGENTS.md: "+perr.Error(), tui.LevelWarn)
	}
	opts := agent.Options{
		Sinks:               []agent.Sink{sink},
		Env:                 env,
		ProjectInstructions: proj,
		Tools:               toolsReg,
		Provider: func(m llm.Model) (llm.Provider, error) {
			return providers.ProviderFor(m)
		},
		Compact: func(ctx context.Context, reason agent.CompactReason) (bool, error) {
			if comp == nil {
				return false, nil // recording is off; nothing to compact
			}
			return comp.run(ctx, reason, "")
		},
		MaxSteps:  set.Settings().Agent.MaxSteps, // <= 0 or unset means unlimited
		SessionID: sessionHint(rec),
	}
	if rec != nil {
		opts.Sinks = []agent.Sink{rec.rec.Sink(sink)} // persist notices and fsync at turn end
		opts.OnMessage = []func(agent.MessageInfo){rec.rec.Message}
		rec.rebuild(set, ui, reg, st, toolsReg)
	}
	// a resumed session restores its enabled tool set; unknown names are ignored.
	if toolsReg != nil && len(st.Tools) > 0 {
		toolsReg.SetEnabled(st.Tools)
	}

	// the editor's in-progress text feeds accounting (SetOnEdit below) and clears on
	// submission via settled. editSinks may be empty before a session is set up.
	editSinks := opts.Sinks // may be empty before a session is set up
	pushContext := func() {
		if st.Tokens == nil || len(editSinks) == 0 {
			return
		}
		c := st.Tokens.Context()
		for _, s := range editSinks {
			s.Context(c)
		}
	}
	// once a submitted prompt and everything behind it lands in state, pending owns
	// its tokens; the submit bucket must clear so they are never counted twice.
	settled := func() {
		if st.Tokens != nil && len(editSinks) > 0 {
			st.Tokens.SetSubmit(0)
			pushContext()
		}
	}
	// prompts submitted while a turn runs queue here: they render as dimmed rows,
	// hand over at the next step boundary (or the next turn), and recover to the
	// editor on interrupt or Alt+Up.
	q := newSteerQueue(ui,
		func(est int) { submitPrompt(st, editSinks, est, pushContext) },
		settled,
	)
	// typingGate holds the next step boundary while the user is mid-message, so a
	// prompt they are still typing lands in this step instead of behind it.
	gate := &typingGate{
		idle:    typingIdle,
		handoff: typingHandoff,
		poll:    typingPoll,
		pending: q.pending,
	}
	// the hold publishes a keyed status segment while it waits on a visible draft.
	gate.status = func(text, short string) {
		ui.SetStatusSegment(tui.Segment{Key: "typing", Text: text, Short: short})
	}
	// the editor's in-progress text feeds accounting so the context bar grows as you
	// type or paste, then clears once submitted (the buffer empties); it is also the
	// typing signal the boundary hold reads.
	ui.SetOnEdit(func(text string) {
		gate.edit(text)
		if st.Tokens == nil || len(editSinks) == 0 {
			return
		}
		st.Tokens.SetCompose(tokens.EstimateText(text, tokens.KindProse))
		pushContext()
	})
	var ag *agent.Agent

	// sub-agent investigations fan read-only work into throwaway child agents,
	// each a fresh headless loop whose only return value is a final summary.
	var sag *subagent.Manager
	if toolsReg != nil {
		sag = subagent.New(subagent.Options{
			Provider: func(m llm.Model) (llm.Provider, error) { return providers.ProviderFor(m) },
			Model:    func() llm.Model { return resolveSubAgentModel(set, reg, st) },
			Reasoning: func() llm.ReasoningConfig {
				return st.Reasoning
			},
			Parent:              func() *tokens.Accounting { return st.Tokens },
			Tools:               toolsReg,
			Env:                 env,
			ProjectInstructions: proj,

			Activity: func(key, text string, rank int) {
				if ui != nil {
					// ranked by job number so rows hold their place, oldest first,
					// however the parallel agent_start calls happen to publish
					ui.SetActivityRanked(key, text, rank)
				}
			},
			Notice: func(msg string) { ui.NotifyKeyed("subagent", msg, tui.LevelInfo) },
			Status: func(text, short string) {
				ui.SetStatusSegment(tui.Segment{Key: "subagents", Text: text, Short: short})
			},
			Deliver: func(in agent.Input) bool {
				if ag == nil || !ag.Running() { // never start a turn on an idle parent
					return false
				}
				return ag.Steer(in)
			},
			MaxConcurrent: set.Settings().Subagent.MaxConcurrent,
		})
		for _, t := range sag.Tools() {
			// builtin source so /tools sorts the trio up front with core tools
			toolsReg.RegisterFrom(tools.SourceBuiltin, t, true) // enabled by default; /tools toggles
		}
		// the trio toggles together in /tools under one "subagents" row.
		toolsReg.RegisterGroup(tools.ToolGroup{
			Name:   "subagents",
			Source: tools.SourceBuiltin,
			Tools:  []string{"agent_start", "agent_poll", "agent_list"},
		})
		// agent_* delegate read-only work, so allow-read runs them free.
		toolsReg.MarkReadOnly([]string{"agent_start", "agent_poll", "agent_list"})
		// flush pending completion steers into the running parent turn at start.
		opts.Sinks = append(opts.Sinks, subagentSink{mgr: sag})
	}

	// record each turn's outcome so a turn-boundary hook can tell a clean stop
	// from an abort or a provider error.
	turnRec := &turnRecorder{}
	opts.Sinks = append(opts.Sinks, turnRec)

	// /init's write lands inside a normal turn; this watches for it so the driver
	// can say the new file only applies on the next start.
	initSeen := &initWatch{notify: ui.Notify}
	opts.Sinks = append(opts.Sinks, initSeen)

	// queued mid-turn prompts land at the next step boundary via this hook.
	opts.OnBoundary = q.pull
	// AwaitInput may hold that boundary while the user finishes a message, so a
	// prompt typed during it lands in this same step rather than behind another call.
	opts.AwaitInput = gate.hold
	// OnToolBatch hands each step's calls (in message order) to sub-agent id
	// reservation and permission prefetch. The barrier is built later, so it is
	// reached through a forward reference assigned in its setup block below; nil
	// until then means no classification to prefetch.
	var batchPrefetch func(context.Context, []agent.ToolCall)
	opts.OnToolBatch = func(ctx context.Context, calls []agent.ToolCall) {
		if sag != nil {
			sag.Reserve(calls)
		}
		if batchPrefetch != nil {
			batchPrefetch(ctx, calls)
		}
	}
	if sag != nil {
		// completion steers join the same boundary: membership is decided at the
		// moment the message lands, so ids a poll already claimed are never named.
		queued := opts.OnBoundary
		opts.OnBoundary = func() []agent.Input {
			out := sag.Boundary()
			out = append(out, queued()...)
			return out
		}
	}

	ag = agent.New(st, opts)

	// started is the single answer to "has the tool block been committed?": it gates
	// both whether tool schemas count toward the bar and whether /tools may still
	// narrow the set. A resumed branch with history plainly sent one already.
	started := len(st.Messages) > 0

	// seed the constant request overhead (system + AGENTS.md) so the bar is honest
	// from startup; tool schemas join only once the block is committed, since until
	// then /tools can still take one away. The pump re-seeds at the first prompt,
	// when MCP servers have connected and their schemas are in the registry, and
	// Agent.stream's own SetBase replaces this floor once a turn actually starts.
	var seedToolsOnce sync.Once
	st.Tokens.SetBase(ag.BaseEstimate(started))
	pushContext()

	if rec != nil {
		rec.bindRewind(ui, ag, reg)
		comp = &compactor{
			rec: rec, st: st, ag: ag, reg: reg,
			sink:        opts.Sinks[0], // set above when rec != nil
			notify:      func(msg string, level agent.Level) { ui.Notify(msg, tui.Level(level)) },
			busy:        ui.Busy,
			providerFor: providers.ProviderFor,
			cfg:         func() config.Compaction { return set.Settings().Compaction },
		}
		rec.resumeAuto = comp.resumeAuto
	}

	// the prompt is at rest until a turn starts; double-Esc rewinds from here.
	ui.SetIdle(true)

	showReasoningIndicator(ui, set, st)

	if active.ID == "" {
		ui.Notify("no model configured; use /model to pick one", tui.LevelWarn)
	}

	quit := make(chan struct{})

	// MCP servers bridge their remote tools into the registry and are supervised by
	// a manager. Every server connects in full, eagerly, just before the user's
	// first message.
	servers, mwarns, merr := mcp.LoadConfig(config.Cwd())
	if merr != nil {
		ui.Notify("mcp: "+merr.Error(), tui.LevelWarn)
	}
	for _, w := range mwarns {
		ui.Notify("mcp: "+w, tui.LevelWarn)
	}
	var mgr *mcp.Manager
	if toolsReg != nil && merr == nil {
		mgr = mcp.New(servers, mcp.Options{
			Registrar: registryAdapter{toolsReg},
			Workspace: config.Cwd(),
			Restore:   st.Tools,
			Notice:    func(msg string, warn bool) { ui.Notify(msg, levelOf(warn)) },
			Status:    func(text string) { ui.SetStatusSegment(tui.Segment{Key: "mcp", Text: text}) },
		})
	}

	// the permission barrier gates every tool call through static classification and
	// an approval dialog. Read-only work runs free; writes prompt unless allowed or
	// blocked by mode. It starts from the resolved config default (a resume's session
	// override included, since rebuild seeded it) so a restart restores the mode.
	var barrier *permit.Barrier
	if toolsReg != nil {
		barrier = permit.NewBarrier(toolsReg.ReadOnly)
		if mstr := set.Settings().Permissions.Mode; mstr != "" {
			if m, ok := permit.ParseMode(mstr); ok {
				barrier.SetMode(m)
			}
		}
		showPermissionIndicator(ui, barrier)
		// the prompter and noter adapt tui and agent onto permit's narrow interfaces;
		// note injection steers the running turn without stopping it.
		barrier.SetPrompter(promptAdapter{ui})
		barrier.SetNoter(func(note string) {
			ag.Steer(agent.Input{Text: note, Injected: true}) // system context, not a user prompt
		})
		// auto mode classifies unverifiable shell commands with a fresh-context model
		// call, cached per exact command; the verdict never enters the session.
		// auto+write's writable roots: the gate path-scopes write/edit against them and
		// the classifier prompt names the same two, so both judge by one rule.
		wcwd, wtmp := config.Cwd(), os.TempDir()
		barrier.SetClassifier(permit.NewCachedClassifier(classifierAdapter{
			providerFor: providers.ProviderFor,
			model:       func() llm.Model { return st.Model }, // current model so /model applies
			schema:      toolSchema(toolsReg),
			cwd:         wcwd,
			tmp:         wtmp,
		}.Classify))
		barrier.SetWriteRoots(wcwd, wtmp)
		barrier.SetNotice(func(msg string) { ui.Notify(msg, tui.LevelInfo) })
		// config-declared safe commands (exact MCP tool names or verbatim bash lines)
		// auto-allow as read-only in allow-read/auto; write/edit can never be listed.
		barrier.SetSafeCommands(set.Settings().Permissions.SafeCommands)
		// config-declared denied commands refuse outright without prompting, every mode.
		barrier.SetDeniedCommands(set.Settings().Permissions.DeniedCommands)
		barrier.SetDryRun(toolsReg.DryRun)
		// the full diff is already committed above the dialog by guardedTool.Execute,
		// so the subject names it rather than repeating a truncated copy.
		barrier.SetPreview(func(call agent.ToolCall) string {
			ch, ok := toolsReg.Preview(call)
			if !ok {
				return ""
			}
			return tui.DiffSummary(ch.Path, ch.Before, ch.After)
		})
		toolsReg.AddGuard(barrier.Guard())
		toolsReg.SetAsker(barrier.Asker())
		// a batch's prompt-classified calls are classified concurrently ahead of
		// their dialogs, so later commands in the batch resolve fast; an abort cancels.
		batchPrefetch = barrier.Prefetch
	}

	// the command registry, shell stager and @ expander own the single dispatch path
	// for submitted lines. Commands run inline; shell lines stage and flush ahead of
	// the next prompt; prompts expand @ refs and steer the agent.
	cmds := command.NewRegistry()
	stager := command.NewStager(toolsReg, sink)
	// `!` output is context the next prompt will carry, so the bar counts it from
	// the moment the command finishes rather than waiting for a submission
	stager.SetOnChange(func(est int) {
		if st.Tokens == nil || len(editSinks) == 0 {
			return
		}
		st.Tokens.SetStaged(est)
		pushContext()
	})
	if rec != nil {
		rec.discardStaged = stager.Discard
	}
	pump := make(chan pumpLine, 16)
	console := &uiConsole{
		ui: ui, set: set, reg: reg, st: st, tools: toolsReg, commands: cmds,
		started: &started, quit: quit, permit: barrier,
	}
	console.refreshBase = func() {
		// BaseEstimate reports 0 while a turn owns State; that turn's own SetBase
		// picks a widened tool block up at its next step, so skip rather than zero it
		if est := ag.BaseEstimate(started); est > 0 {
			st.Tokens.SetBase(est)
			pushContext()
		}
	}
	if mgr != nil {
		console.mcp = mcpAdapter{mgr}
	}
	if sag != nil {
		console.agents = agentsAdapter{sag}
	}
	if rec != nil {
		console.rec = rec.rec
		console.sess = rec
		console.comp = comp
	}
	// the plan workflow needs a transcript to branch and a registry to scope;
	// without either it is simply absent and nothing else changes.
	ctl := newPlanController(planDeps{
		rec: rec, ag: ag, reg: reg, st: st, ui: ui, console: console, toolsReg: toolsReg, q: q,
	})
	hooks := planHooksFor(ctl, turnRec)
	// the survey needs the tool registry to run read and agent_* through; without
	// one /init is simply absent, like the plan workflow without a transcript.
	ictl := newInitController(initDeps{
		cwd: config.Cwd(), toolsReg: toolsReg, sink: sink, ag: ag,
		notify: ui.Notify, agents: sag, watch: initSeen, pump: pump,
	})
	command.RegisterBuiltins(cmds, console)
	for _, c := range append(planCommands(ctl), initCommands(ictl)...) {
		cmds.Register(c)
	}
	if ctl != nil && comp != nil {
		// automatic compaction inside a phase keeps its own focus; an explicit
		// /compact <instructions> still wins.
		comp.focus = ctl.Focus
	}
	onModeCycle := func() {
		if barrier == nil {
			return
		}
		m := barrier.Cycle() // re-evaluates any open dialog under the new mode
		showPermissionIndicator(ui, barrier)
		ui.Notify("permissions mode: "+m.String(), tui.LevelInfo)
		// record a session override so Explain and Settings report (session) and a
		// resume restores it; the config file is never rewritten.
		_ = console.SetSessionSetting("permissions.mode", m.String())
	}
	watchControls(ui, ag, q, stager, ictl, quit, onModeCycle)
	expander := refs.NewExpander(toolsReg, sink, tools.PathPolicy{Cwd: config.Cwd()})
	expander.Seed(st.Messages) // a resumed transcript already holds ref ids
	if rec != nil {
		rec.started = &started
		rec.onSwitch = func(msgs []llm.Message) {
			if t := toolsReg.Tracker(); t != nil {
				t.Reset() // reads the new context lacks must re-inject, not dedupe
			}
			expander.Seed(msgs)
		}
	}
	idx := refs.NewIndex(config.Cwd())
	ui.SetCompleter(command.NewCompleter(cmds, console, idx))

	// Ctrl+R and ↑/↓ recall this workspace's typed lines merged with recorded
	// prompts; every submitted line also lands in the editor-history file now.
	var hist *session.EditorHistory
	if store := promptStore(rec); store != nil {
		hist, _ = session.NewEditorHistory(store, config.Cwd(), secretPrefix)
		defer hist.Compact() // bounded rewrite at exit; last writer wins under concurrency
		idx := session.NewRecallIndex(store, config.Cwd(), hist)
		ui.SetHistorySearch(func() []tui.SearchItem { return searchItems(idx.Lines()) })
	}

	if ctl != nil {
		// a manual rewind invalidates the workflow's branch points, so it ends first
		ui.SetOnRewind(func() {
			if ctl.Active() {
				ctl.Stop()
			}
			rec.rewind(ui, ag, reg)
		})
		ctl.Restore() // pick a mid-workflow session back up before the first prompt
	}

	// a first start with no palette chosen picks one before any output exists
	if ui.Mode() != tui.ModePlain {
		if terr := command.ThemeSetup(context.Background(), console); terr != nil {
			ui.Notify("theme: "+terr.Error(), tui.LevelWarn)
		}
	}

	go runPump(pump, ag, console, stager, expander, rec != nil, ui, &started,
		settled, q, gate, st, editSinks, &seedToolsOnce, pushContext, hooks)

	if len(args) > 0 { // an argv prompt is programmatic input, not a typed line
		initial := strings.Join(args, " ")
		hist.AppendHidden(initial) // durable in the workspace store yet excluded from ↑/↓ and Ctrl+R
		// echo and accounting happen in the pump like every other prompt.
		pump <- pumpLine{kind: command.KindPrompt, rest: initial, injected: true}
	}

	// teardown runs on the way out of a quit the user just asked for, so the two
	// independent shutdowns overlap rather than adding up between the keypress and
	// the restored terminal. Each is internally bounded.
	defer func() {
		var wg sync.WaitGroup
		if sag != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sag.Close() // cancel every running investigation and wait briefly
			}()
		}
		if mgr != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				mgr.Close()
			}()
		}
		wg.Wait()
	}()

	// an abandoned session with no conversation is worthless to resume; drop it and
	// return empty so main skips the "to resume this session" hint.
	finish := func(r *sessRec) string {
		if r != nil && r.empty() {
			_ = r.store.Remove(r.w.Path())
			return ""
		}
		return sessionLabel(r)
	}

	for {
		select {
		case msg, ok := <-ui.Messages():
			if !ok {
				close(pump)
				return finish(rec) // UI closed
			}
			line := command.ParseLine(msg)
			if line.Kind == command.KindCommand {
				hist.AppendHidden(msg) // slash commands stay durable yet excluded from ↑/↓ and Ctrl+R
			} else {
				hist.Append(msg) // every prompt and !shell line recorded for recall; nil-safe
			}
			switch line.Kind {
			case command.KindShell:
				stager.Run(line.Rest, line.Excluded)
			case command.KindCommand:
				pump <- pumpLine{kind: command.KindCommand, rest: line.Rest}
			default:
				// arm the handoff here, where the line certainly left the editor: the
				// async edit notification cannot re-arm after the pump resolves it
				gate.submitted()
				pump <- pumpLine{kind: command.KindPrompt, rest: line.Rest}
			}
		case <-quit:
			close(pump)
			return finish(rec)
		}
	}
}

type subagentSink struct {
	agent.NopSink
	mgr *subagent.Manager
}

func (s subagentSink) TurnStart(agent.TurnInfo) { s.mgr.Flush() }

func (s subagentSink) TurnEnd(r agent.TurnResult) {
	if r.Stop == llm.StopAborted {
		s.mgr.Interrupted()
	}
}

func resolveSubAgentModel(set *config.Set, reg *llm.Registry, st *agent.State) llm.Model {
	if m := set.Settings().Subagent.Model; m != "" {
		if r, err := reg.Resolve(m); err == nil {
			return r
		}
	}
	return st.Model
}

const doublePressWindow = 10 * time.Second

func watchControls(ui *tui.UI, ag *agent.Agent, q *steerQueue, stager *command.Stager, initCtl *initController, quit chan struct{}, onModeCycle func()) {
	go controlLoop(ui.Controls(), ui, ag, q, stager, initCtl, quit, onModeCycle)
}

func controlLoop(controls <-chan tui.Control, ui *tui.UI, ag *agent.Agent, q *steerQueue, stager *command.Stager, initCtl *initController, quit chan struct{}, onModeCycle func()) {
	// armed is when the first Ctrl+C landed; quitHint fires to retire the hint
	// that advertises the window, keeping the gesture and the hint the same
	// length. The window is measured from armed, not from the timer, so a press
	// arriving as the timer fires cannot lose the arm to select's coin flip.
	var armed time.Time
	var quitHint <-chan time.Time
	for {
		select {
		case <-quitHint:
			quitHint = nil
			ui.SetStatusSegment(tui.Segment{Key: "hint"}) // empty Text removes it
		case c, ok := <-controls:
			if !ok {
				return // the UI went away
			}
			switch c {
			case tui.ControlEscape:
				switch {
				case ag.Running():
					q.abort() // queued messages return to the editor, joined with newlines
					ag.Interrupt()
				case initCtl.abort(): // a minutes-long /init survey is escapable too
				case stager.Pending():
					stager.Cancel() // Esc cancels an in-flight staged shell command
				}
			case tui.ControlInterrupt:
				if ag.Running() {
					q.abort()
					ag.Interrupt()
					continue
				}
				if initCtl.abort() {
					ui.SetStatusSegment(tui.Segment{Key: "hint", Text: "cancelled project survey"})
					continue
				}
				// a running `!` cancels on the first Ctrl+C instead of quitting
				if stager.Pending() {
					stager.Cancel()
					ui.SetStatusSegment(tui.Segment{Key: "hint", Text: "cancelled shell command"})
					continue
				}
				if !armed.IsZero() && time.Since(armed) < doublePressWindow { // inside the promised window
					// teardown still has to run; say so rather than leaving the
					// "again to quit" hint up, which reads as a press that missed
					ui.SetStatusSegment(tui.Segment{Key: "hint", Text: "quitting…"})
					close(quit)
					return
				}
				armed = time.Now()
				quitHint = time.After(doublePressWindow)
				ui.SetStatusSegment(tui.Segment{Key: "hint", Text: "ctrl+c again to quit"})
			case tui.ControlEOF:
				if ag.Running() {
					continue // ignored while a turn streams, per the key table
				}
				close(quit)
				return
			case tui.ControlRecallQueued:
				q.recall() // Alt+Up: pop the newest queued message back into the editor
			case tui.ControlModeCycle:
				if onModeCycle != nil {
					onModeCycle()
				}
			}
		}
	}
}
