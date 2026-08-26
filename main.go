package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/pflag"

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

// secretPrefix marks editor lines excluded from the workspace's persisted line
// history, so a pasted secret never reaches disk.
const secretPrefix = "secret:"

func main() {
	f, err := parseFlags(os.Args[1:])
	if errors.Is(err, pflag.ErrHelp) {
		return
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "ajent: %v\n", err)
		os.Exit(exitUsage)
	}
	if verr := f.validate(); verr != nil {
		fmt.Fprintf(os.Stderr, "ajent: %v\n", verr)
		os.Exit(exitUsage)
	}

	// --resume overrides --continue; neither means a brand-new session.
	sessMode := modeNewSession
	if f.cont {
		sessMode = modeContinue
	}
	switch {
	case f.resume && f.resumeID != "":
		sessMode = modeResumeID // reopen that exact saved transcript by id
	case f.resume:
		sessMode = modeResumePick // pick among saved roots, then resume its leaf
	}

	// A requested session id must resolve before the TUI opens; otherwise fail fast
	// with a clear message instead of silently starting a fresh transcript.
	if sessMode == modeResumeID {
		sid := cwdOrDot()
		store, serr := session.NewStore()
		if serr != nil {
			fmt.Fprintf(os.Stderr, "ajent: %v\n", serr)
			os.Exit(exitUsage)
		}
		if _, ferr := store.Find(sid, f.resumeID); ferr != nil {
			fmt.Fprintf(os.Stderr, "ajent: no session matches id %q\n", f.resumeID)
			os.Exit(exitUsage)
		}
	}

	// the demo build reconfigures itself before config.Load reads AJENT_HOME.
	stop := startDemo()
	defer stop()

	// the flag layer outranks every file layer; -m/--render stop being ad hoc.
	flagLayer := config.Layer{Name: "flag"}
	var ferr error
	if f.model != "" {
		flagLayer.Data, ferr = config.SetKey(flagLayer.Data, "model", f.model)
	}
	if f.render != "auto" && ferr == nil {
		flagLayer.Data, _ = config.SetKey(flagLayer.Data, "ui.render", f.render)
	}

	set, warnings, err := config.Load(config.Options{
		Workspace: cwdOrDot(),
		Flags:     flagLayer,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ajent:", err)
		os.Exit(exitUsage)
	}

	modeName := set.Settings().UI.Render
	mode, ok := tui.ParseMode(modeName)
	if !ok || modeName == "" {
		fmt.Fprintf(os.Stderr, "ajent: unknown render mode %q\n", modeName)
		os.Exit(exitUsage)
	}
	themeName := set.Settings().UI.Theme
	pal, known := tui.LookupPalette(themeName)
	if !known {
		pal = tui.DefaultPalette()
		warnings = append(warnings, fmt.Sprintf("unknown ui.theme %q, using %q", themeName, pal.Name))
	}
	tools.ApplyLimits(toolLimitsFrom(set.Settings().Tools.Limits))

	file, w, err := llm.LoadUserFile()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ajent:", err)
		os.Exit(exitUsage)
	}
	warnings = append(warnings, w...)
	overridden, owarns, oerr := llm.ApplyOverrides(file, set.Settings().Providers, set.Settings().Models)
	if oerr != nil {
		fmt.Fprintln(os.Stderr, "ajent:", oerr)
		os.Exit(exitUsage)
	}
	warnings = append(warnings, owarns...)
	file = overridden
	// a config.json model choice becomes the default before discovery runs.
	if m := set.Settings().Model; m != "" {
		file.DefaultModel = m
	}
	reg, regWarnings := llm.NewRegistry(file, llm.LoadUserCache(), llm.RegistryOptions{})
	warnings = append(warnings, regWarnings...)

	active := reg.Active()
	if f.model != "" {
		// the flag already set config's model; resolve it through the registry.
		if m, rerr := reg.Resolve(f.model); rerr == nil {
			reg.SetActive(m)
			active = m
		}
	}

	// a one-shot run never opens a terminal: it drains the same loop onto stdout
	// and exits with a code a script can branch on.
	if f.prompt != "" {
		code := runHeadless(headlessOptions{
			flags: f, set: set, reg: reg, active: active,
			sessMode: sessMode, resumeID: f.resumeID, warnings: warnings,
		})
		stop() // os.Exit skips the deferred demo teardown
		os.Exit(code)
	}

	// a model with no reported window renders no context bar rather than one drawn
	// against a fabricated number; parts() already skips the bar when MaxTokens is 0.
	var label, short string
	if active.ID != "" {
		label = active.Key()
		short = active.ShortName()
	}

	ui, err := tui.New(tui.Options{
		Mode:       mode,
		Palette:    pal,
		Model:      label,
		ModelShort: short,
		MaxTokens:  active.ContextWindow,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ajent:", err)
		os.Exit(exitUsage)
	}
	defer ui.Close()

	for _, w := range warnings {
		ui.Notify(w, tui.LevelWarn)
	}
	go refreshModels(ui, reg)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sig
		ui.Close()
		os.Exit(0)
	}()

	sess := driver(ui, set, reg, active, sessMode, f.resumeID, f.args)

	// Restore the terminal before printing so the hint is visible after a Ctrl+C /
	// Ctrl+D quit, then tell the user how to get back to this conversation.
	ui.Close()
	if id := sessionHint(sess); id != "" {
		fmt.Printf("\nRun `ajent --resume %s` to resume this session.\n", id)
	}
}

// driver runs the real agent loop: it builds an Agent over the registry and
// drives turns from submitted messages, steering mid-turn input into the running
// turn rather than starting a second one. sessMode decides whether this run starts
// fresh or resumes a saved transcript; resumeID is the id for modeResumeID. It
// returns the open session record (nil when recording could not be set up) so main
// can print its resume hint on exit.
func driver(ui *tui.UI, set *config.Set, reg *llm.Registry, active llm.Model, sessMode resumeMode, resumeID string, args []string) *sessRec {
	providers := llm.NewProviders(reg)
	rc := reasoningFrom(set.Settings().Reasoning, active)
	st := &agent.State{
		Model:     active,
		Reasoning: rc,
		Tokens:    tokens.New(active),
	}

	// every turn is recorded into the workspace transcript so double-Esc while idle
	// can open the context-tree picker and rewind onto an earlier point.
	rec := newSession(ui, sessMode, resumeID, active.Key())
	if rec == nil {
		ui.Notify("session recording disabled; Esc will not rewind", tui.LevelWarn)
	}

	sink := tuisink.New(ui)

	// build the built-in tool registry and hand it to the loop so the model can
	// read, write, edit and run commands.
	// ask_user rides the TUI's question queue, so it never pre-empts a permission
	// dialog. It stays disabled until a workflow enables it.
	toolsReg, terr := tools.Builtins(tools.Options{SessionID: cwdOrDot(), Ask: askUser(ui)})
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

	// feed the editor's in-progress text into accounting so the context bar grows
	// as you type or paste, then clears it once submitted (the buffer empties).
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
	ui.SetOnEdit(func(text string) {
		if st.Tokens == nil || len(editSinks) == 0 {
			return
		}
		st.Tokens.SetCompose(tokens.EstimateText(text, tokens.KindProse))
		pushContext()
	})
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

			Activity: func(key, text string) {
				if ui != nil {
					ui.SetActivity(key, text)
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
			notify:      func(msg string, level agent.Level) { ui.Notify(msg, tuiLevel(level)) },
			busy:        ui.Busy,
			providerFor: providers.ProviderFor,
		}
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
	servers, mwarns, merr := mcp.LoadConfig(cwdOrDot())
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
			Workspace: cwdOrDot(),
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
		barrier.SetClassifier(permit.NewCachedClassifier(classifierAdapter{
			providerFor: providers.ProviderFor,
			model:       func() llm.Model { return st.Model }, // current model so /model applies
			schema:      toolSchema(toolsReg),
		}.Classify))
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
	}

	// the command registry, shell stager and @ expander own the single dispatch path
	// for submitted lines. Commands run inline; shell lines stage and flush ahead of
	// the next prompt; prompts expand @ refs and steer the agent.
	cmds := command.NewRegistry()
	stager := command.NewStager(toolsReg, sink)
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
		cwd: cwdOrDot(), toolsReg: toolsReg, sink: sink, ag: ag,
		notify: ui.Notify, agents: sag, watch: initSeen, pump: pump,
	})
	registerCommands(cmds, console, append(planCommands(ctl), initCommands(ictl)...)...)
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
	watchControls(ui, ag, q, stager, ictl, quit, onModeCycle, func() {
		if sag != nil {
			sag.Interrupted()
		}
	})
	expander := refs.NewExpander(toolsReg, sink, tools.PathPolicy{Cwd: cwdOrDot()})
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
	idx := refs.NewIndex(cwdOrDot(), tools.PathPolicy{Cwd: cwdOrDot()})
	ui.SetCompleter(command.NewCompleter(cmds, console, idx))

	// Ctrl+R and ↑/↓ recall this workspace's typed lines merged with recorded
	// prompts; every submitted line also lands in the editor-history file now.
	var hist *session.EditorHistory
	if store := promptStore(rec); store != nil {
		hist, _ = session.NewEditorHistory(store, cwdOrDot(), secretPrefix)
		defer hist.Compact() // bounded rewrite at exit; last writer wins under concurrency
		idx := session.NewRecallIndex(store, cwdOrDot(), hist)
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
		settled, q, st, editSinks, &seedToolsOnce, pushContext, hooks)

	if len(args) > 0 { // an argv prompt is programmatic input, not a typed line
		initial := strings.Join(args, " ")
		hist.AppendHidden(initial) // durable in the workspace store yet excluded from ↑/↓ and Ctrl+R
		// echo and accounting happen in the pump like every other prompt.
		pump <- pumpLine{kind: command.KindPrompt, rest: initial, injected: true}
	}

	defer func() {
		if sag != nil {
			sag.Close() // cancel every running investigation and wait briefly
		}
		if mgr != nil {
			mgr.Close()
		}
	}()

	// an abandoned session with no conversation is worthless to resume; drop it and
	// return nil so main skips the "to resume this session" hint.
	finish := func(r *sessRec) *sessRec {
		if r != nil && r.empty() {
			_ = r.store.Remove(r.w.Path())
			return nil
		}
		return r
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
				pump <- pumpLine{kind: command.KindPrompt, rest: line.Rest}
			}
		case <-quit:
			close(pump)
			return finish(rec)
		}
	}
}

// planHooks are the workflow seams the pump and drain loop consult. A zero value
// disables both, leaving dispatch exactly as it is with no workflow running.
type planHooks struct {
	// beforePrompt may rewrite a submitted input before it starts a turn. It runs
	// on the pump goroutine with the agent idle, so it may switch branches.
	beforePrompt func(context.Context, agent.Input) (agent.Input, bool)
	// advance runs at every turn boundary on the drain goroutine, errored turns
	// included. A returned input continues the same drain loop as the next turn.
	advance func(context.Context) (agent.Input, bool)
}

// registerCommands wires the built-in commands, then any workflow commands on
// top. Every registration path goes through here so a new feature can never
// displace the built-in set the way a stray edit at the call site could.
func registerCommands(cmds *command.Registry, console *uiConsole, extra ...command.Command) {
	command.RegisterBuiltins(cmds, console)
	for _, c := range extra {
		cmds.Register(c)
	}
}

// pumpLine is one classified submitted line handed to the prompt pump. injected marks
// programmatic input (e.g. an argv bootstrap prompt) so it never enters recall/search.
type pumpLine struct {
	kind     command.Kind
	rest     string
	injected bool // non-typed input, excluded from transcript-derived prompt recall
	// input is an already-assembled prompt (the /init survey and its tool pairs)
	// re-entering the pump; it skips @ expansion and carries rest as a short label.
	input *agent.Input
	// onTurn runs as that input becomes a turn, so the sender can observe the turn
	// its work produced rather than guessing when it lands.
	onTurn func()
}

// runPump owns ordering for commands and prompts. Commands run inline (pickers
// block only the pump); prompts flush staged shell results, expand @ refs and
// steer or start a turn. Submissions stay in order and the UI never stalls.
// submittedEcho returns the prompt text to echo above the input for a submitted
// line, "" when nothing should appear (commands and shell lines are not echoed).
func submittedEcho(msg string) string {
	line := command.ParseLine(msg)
	if line.Kind == command.KindPrompt && strings.TrimSpace(line.Rest) != "" {
		return line.Rest
	}
	return ""
}

// submitPrompt carries the sent text's estimate in the ledger until it lands as a
// message, so the bar does not dip between the editor clearing and appendSteer.
func submitPrompt(st *agent.State, editSinks []agent.Sink, est int, push func()) {
	if st.Tokens == nil || len(editSinks) == 0 {
		return
	}
	st.Tokens.SetSubmit(est)
	push()
}

// runPump owns ordering for commands and prompts. Commands run inline (pickers
// block only the pump); prompts flush staged shell results, expand @ refs then
// queue-or-start a turn. Submissions stay in order and the UI never stalls.
func runPump(pump <-chan pumpLine, ag *agent.Agent, console *uiConsole, stager *command.Stager, expander *refs.Expander, recording bool, ui *tui.UI, started *bool, settled func(), q *steerQueue, st *agent.State, editSinks []agent.Sink, seedToolsOnce *sync.Once, pushContext func(), hooks planHooks) {
	for line := range pump {
		switch line.kind {
		case command.KindCommand:
			name, arg, ok := command.SplitCommand(line.rest)
			if !ok {
				console.Notify("unknown command /"+line.rest, tui.LevelWarn)
				continue
			}
			// MCP servers load eagerly so the pre-first-prompt /tools picker and /mcp
			// list already show them; LoadOnFirstMessage is idempotent (runs once).
			if console.mcp.m != nil && (name == "tools" || name == "mcp") {
				console.mcp.LoadOnFirstMessage(context.Background())
			}
			cmd, ok := console.commands.Get(name)
			if !ok {
				console.Notify("unknown command /"+name, tui.LevelWarn)
				continue
			}
			_ = cmd.Handler(context.Background(), arg, console)
		case command.KindPrompt:
			if line.input == nil && strings.TrimSpace(line.rest) == "" {
				continue
			}
			// connect every MCP server in full, once, so its tools exist before this
			// (the first) turn is assembled; /tools or /mcp changes made up to now hold
			if console.mcp.m != nil {
				console.mcp.LoadOnFirstMessage(context.Background())
			}
			// flush staged shell results ahead of the message, waiting for any
			// in-flight command to finish first
			before := stager.Flush(context.Background())

			in, echo, pending := promptInput(line, before, expander, func(n string) {
				console.Notify(n, tui.LevelWarn)
			})
			est := submitEstimate(in, pending)
			if q.offer(in, echo, est) {
				continue // queued as a dimmed row; the echo lands at delivery
			}
			// only reached with no drain running, so a workflow may branch here
			if hooks.beforePrompt != nil {
				if wrapped, ok := hooks.beforePrompt(context.Background(), in); ok {
					in = wrapped
					est = submitEstimate(in, pending)
				}
			}
			if echo != "" {
				ui.UserEcho(echo)
			}
			seedToolsOnce.Do(func() { st.Tokens.SetBase(ag.BaseEstimate(true)); pushContext() })
			submitPrompt(st, editSinks, est, pushContext)
			in.Settled = settled
			startDrain(ui, recording, ag, q, in, started, hooks)
		}
	}
}

// promptInput turns one pump line into the input to send, the text to echo and
// the estimated tokens its @ reads will add once they land. An already-assembled
// input (the /init survey) keeps its own Before and uses rest as a short label,
// since its instruction is far too long to echo.
func promptInput(line pumpLine, before []llm.Message, expander *refs.Expander, warn func(string)) (agent.Input, string, int) {
	if line.input != nil {
		in := *line.input
		in.Before = append(before, in.Before...)
		if line.onTurn != nil {
			line.onTurn() // this turn writes, not the one running when the sender finished
		}
		return in, line.rest, 0
	}
	res := expander.Expand(line.rest)
	for _, n := range res.Notices {
		warn(n)
	}
	return agent.Input{
		Text:     res.Text,
		Before:   before,
		After:    res.Run,
		Injected: line.injected,
	}, submittedEcho(line.rest), res.Est // "" unless a real prompt; commands and shell lines are not echoed here
}

// submitEstimate sizes what a submission adds to context: its text, any staged
// pairs riding ahead of it, and pending, the @ reads that land behind it.
func submitEstimate(in agent.Input, pending int) int {
	est := tokens.EstimateText(in.Text, tokens.KindProse) + pending
	if len(in.Before) > 0 {
		est += tokens.EstimateMessages(in.Before)
	}
	return est
}

// startDrain runs the submitted prompt to completion in the background, then
// keeps draining the steer queue as further turns while it stays non-empty. The
// pump never spawns a second drain: offer() only hands back false when no drain
// goroutine exists, so exactly one goroutine drives turns at a time.
func startDrain(ui *tui.UI, recording bool, ag *agent.Agent, q *steerQueue, input agent.Input, started *bool, hooks planHooks) {
	ui.SetIdle(false)
	*started = true
	go func() {
		for {
			err := ag.Prompt(context.Background(), input)
			// a workflow owns the next turn when it says so, errored turns included:
			// its executor-retry rule depends on being reached after a failure.
			if hooks.advance != nil {
				if next, ok := hooks.advance(context.Background()); ok {
					input = next
					continue
				}
			}
			if err != nil {
				q.stopDrain() // leave items queued as rows; do not hammer a failing provider
				break
			}
			next, ok := q.take()
			if !ok {
				break
			}
			input = next
		}
		if recording {
			ui.SetIdle(true)
		}
	}()
}

// promptAdapter adapts tui's decision dialogs and free-text questions onto
// permit.Prompter. In plain mode no dialog can be shown, so Open reports ErrNoUI.
type promptAdapter struct{ ui *tui.UI }

func (a promptAdapter) Open(prompt, subject string, options []string) (permit.Dialog, error) {
	if a.ui.Mode() == tui.ModePlain { // nobody to ask in headless/piped mode
		return nil, tui.ErrNoUI
	}
	opts := make([]tui.Option, len(options))
	for i, o := range options {
		opts[i] = tui.Option{Label: o}
	}
	d := a.ui.OpenDecision(tui.DecisionRequest{Prompt: prompt, Context: subject, Options: opts})
	return decisionAdapter{d}, nil
}

func (a promptAdapter) Reason(ctx context.Context, label string) (string, bool) {
	ans, err := a.ui.Ask(ctx, tui.Question{Text: label})
	if err != nil || ans.Declined {
		return "", false
	}
	return ans.Text, true
}

// subagentSink flushes pending completion steers into the running parent turn.
// TurnStart runs once `running` is true, so a steer queues and lands at step 1
// without ever starting an idle agent; NopSink carries every other event.
type subagentSink struct {
	agent.NopSink
	mgr *subagent.Manager
}

func (s subagentSink) TurnStart(agent.TurnInfo) { s.mgr.Flush() }

// turnRecorder keeps the last turn's result so a turn-boundary hook can tell a
// clean stop from an abort or a provider error. Prompt only returns an error, and
// an aborted turn is not one.
type turnRecorder struct {
	agent.NopSink

	mu     sync.Mutex
	result agent.TurnResult
}

func (t *turnRecorder) TurnEnd(r agent.TurnResult) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.result = r
}

// last returns the most recently ended turn's result.
func (t *turnRecorder) last() agent.TurnResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.result
}

// resolveSubAgentModel returns the configured child model resolved through the
// registry, or the session's current model when unset or unresolvable. Resolved
// at spawn so a /settings change applies to the next job.
func resolveSubAgentModel(set *config.Set, reg *llm.Registry, st *agent.State) llm.Model {
	if m := set.Settings().Subagent.Model; m != "" {
		if r, err := reg.Resolve(m); err == nil {
			return r
		}
	}
	return st.Model
}

// classifierAdapter classifies an unverifiable call with a one-shot call to the
// parameters), used to judge MCP/extension calls in auto+mcp mode.
func toolSchema(reg *tools.Registry) func(name string) (llm.ToolSchema, bool) {
	return func(name string) (llm.ToolSchema, bool) {
		t, ok := reg.Get(name)
		if !ok {
			return llm.ToolSchema{}, false
		}
		sch := t.Schema()
		sch.Name = name
		sch.Description = t.Description()
		return sch, true
	}
}

// classifierAdapter classifies an unverifiable call with a one-shot call to the
// session's current model in a fresh context; the verdict never enters the session.
// providerFor resolves the active vendor, model yields the live state so /model
// switches apply, schema fetches tool metadata for non-shell (MCP) calls.
type classifierAdapter struct {
	providerFor func(llm.Model) (llm.Provider, error)
	model       func() llm.Model
	schema      func(name string) (llm.ToolSchema, bool) // nil: no MCP metadata available
}

func (a classifierAdapter) Classify(ctx context.Context, s permit.Subject) permit.Class {
	if a.schema == nil && !s.IsShell() {
		return permit.ClassUnsure // an MCP call needs its tool metadata to be judged
	}
	m := a.model()
	if m.ID == "" { // no model configured; nothing to classify with
		return permit.ClassUnsure
	}
	p, err := a.providerFor(m)
	if err != nil {
		return permit.ClassUnsure
	}
	var sys = permit.ClassifierSystem
	userMsg := s.Args
	if !s.IsShell() { // an MCP/extension tool call is judged with its own framing and metadata
		sch, ok := a.schema(s.Name)
		if !ok {
			return permit.ClassUnsure // unknown tool cannot be evaluated safely
		}
		sys = permit.MCPClassifierSystem(sch.Name, sch.Description, string(sch.Parameters))
		userMsg = s.Args
	}
	req := llm.Request{
		Model:     m,
		System:    llm.BlockList{llm.TextBlock{Text: sys}},
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: llm.BlockList{llm.TextBlock{Text: userMsg}}}},
		MaxTokens: classifyBudget(m),
	}
	req.Reasoning = llm.ReasoningConfig{Level: llm.ClampLevel(m, llm.LevelMinimal)}
	out, _, serr := runSummary(ctx, p, req)
	if serr != nil {
		return permit.ClassUnsure
	}
	return permit.NormalizeClass(out)
}

// classifyBudget sizes the classifier's output cap generously so a reasoning model
// still has room for its thinking block when minimal is clamped away.
func classifyBudget(m llm.Model) int {
	budget := m.MaxOutput
	if budget <= 0 {
		return 4096 // unknown window: modest fallback, enough for one word plus thought
	}
	return min(budget, 20480)
}

// decisionAdapter wraps *tui.Decision onto permit.Dialog.
type decisionAdapter struct{ d *tui.Decision }

func (a decisionAdapter) Wait(ctx context.Context) (int, error) {
	r, err := a.d.Wait(ctx)
	if err != nil {
		// an explicit Esc is a real denial, not the headless no-UI path.
		if errors.Is(err, tui.ErrCancelled) {
			return 0, permit.ErrDenied
		}
		return 0, err
	}
	return r.Index, nil
}
func (a decisionAdapter) Resolve(index int) { a.d.Resolve(index) }
func (a decisionAdapter) Close()            { a.d.Close() }

// showPermissionIndicator shows the live mode in the status bar when it differs
// from the allow-read default, clearing it otherwise. The non-default modes must
// always be visible so nobody forgets the gate is open.
func showPermissionIndicator(ui *tui.UI, b *permit.Barrier) {
	if ui == nil || b == nil {
		return
	}
	m := b.Mode()
	if m != permit.ModeAllowRead { // default: hidden until the user changes it
		ui.SetStatusSegment(tui.Segment{Key: "permissions", Text: m.String(), Short: m.Short()})
		return
	}
	ui.SetStatusSegment(tui.Segment{Key: "permissions"})
}

// showReasoningIndicator shows the reasoning level in the status bar when it
// differs from the resolved default, clearing it otherwise.
func showReasoningIndicator(ui *tui.UI, set *config.Set, st *agent.State) {
	if ui == nil || st == nil {
		return
	}
	dflt := llm.LevelMedium // the compiled-in default level
	if d, _, ok := set.Explain("reasoning.level"); ok && string(d) != `"medium"` {
		ui.SetStatusSegment(tui.Segment{Key: "reasoning", Text: st.Reasoning.Level.String()})
	} else if st.Reasoning.Level != dflt {
		ui.SetStatusSegment(tui.Segment{Key: "reasoning", Text: st.Reasoning.Level.String()})
	} else {
		ui.SetStatusSegment(tui.Segment{Key: "reasoning"})
	}
}

// toolLimitsFrom maps a typed limits block onto tools.Limits for ApplyLimits.
func toolLimitsFrom(l config.ToolLimits) tools.Limits {
	return tools.Limits{
		Bash:      toolsLimit(l.Bash),
		Read:      toolsLimit(l.Read),
		Find:      toolsLimit(l.Find),
		Grep:      toolsLimit(l.Grep),
		Ls:        toolsLimit(l.Ls),
		RefInject: toolsLimit(l.RefInject),
		RefTotal:  toolsLimit(l.RefTotal),
	}
}

func toolsLimit(l config.Limit) tools.Limit {
	return tools.Limit{Lines: l.Lines, Bytes: l.Bytes}
}

// reasoningFrom translates a config reasoning block into the active model's
// configured level and retention, falling back to defaults when unparsable.
func reasoningFrom(r config.Reasoning, m llm.Model) llm.ReasoningConfig {
	lvl := llm.LevelMedium
	if l, ok := llm.ParseLevel(r.Level); ok && r.Level != "" {
		lvl = llm.ClampLevel(m, l)
	}
	retain := llm.RetainWholeTurn
	if p, ok := llm.ParseRetain(r.Retain); ok && r.Retain != "" {
		retain = p
	}
	show := r.Show
	return llm.ReasoningConfig{Level: lvl, Retain: retain, Show: show}
}

// sessRec owns the open transcript: the store it lives in, the writer appending
// to it, and its path. bindRewind attaches the double-Esc handler that reads the
// branch back off disk and offers a picker over prior messages.
type sessRec struct {
	store *session.Store
	w     *session.Writer
	rec   *session.Recorder
	// onSwitch reports a rebuilt context — rewind, fork or compaction — so read
	// tracking and @ reference ids stop describing what it replaced. Nil until
	// main wires it.
	onSwitch func([]llm.Message)
	// started is the driver's tool-block-committed flag, shared so a rewind seeds
	// its base with the same rule the pump uses. Nil until main wires it.
	started *bool
}

// extractResume scans argv for a --resume token and its optional trailing session
// id, removing those tokens from the returned args so flag.Parse sees only the
// other flags. A bare `--resume` means "pick among saved roots"; `--resume <id>`
// reopens that exact transcript directly.
func extractResume(argv []string) (given bool, id string, rest []string) {
	rest = make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--resume" || a == "-resume":
			given = true
			if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				id, i = strings.TrimSpace(argv[i+1]), i+1 // consume the id token
			}
		case strings.HasPrefix(a, "--resume="):
			given = true
			id = a[len("--resume="):]
		default:
			rest = append(rest, a)
		}
	}
	return given, id, rest
}

// cwdOrDot returns the current working directory, or "." when it is unavailable.
func cwdOrDot() string {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		return "."
	}
	return cwd
}

// sessionHint returns the active transcript's root id — the value to pass back via
// `ajent --resume <id>` to rejoin this conversation. Empty when recording is off or
// nothing can be read from disk.
func sessionHint(r *sessRec) string {
	if r == nil || r.w == nil {
		return ""
	}
	entries, _, err := session.Read(r.w.Path())
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.Type == session.TypeSession {
			return e.ID
		}
	}
	return ""
}

// empty reports whether this run recorded no conversation: the transcript holds
// zero message entries. A resumed non-empty session stays, so only a brand-new
// session abandoned before its first prompt is dropped.
func (r *sessRec) empty() bool {
	if r == nil || r.w == nil || r.store == nil {
		return false
	}
	entries, _, err := session.Read(r.w.Path())
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Type == session.TypeMessage {
			return false
		}
	}
	return true
}

// resumeMode says what this run should do with saved sessions.
type resumeMode int

const (
	modeNewSession resumeMode = iota // no flag: always a brand-new transcript
	modeContinue                     // --continue: auto-resume the most recent one
	modeResumePick                   // --resume: picker over session roots, then resume its leaf
	modeResumeID                     // --resume <id>: reopen that exact saved transcript directly
)

// newSession opens (or resumes) the workspace's current transcript per mode, or
// nil when recording cannot be set up so the app keeps running without a rewind tree.
// newSession opens (or resumes) the workspace's current transcript per mode, or
// nil when recording cannot be set up so the app keeps running without a rewind tree.
func newSession(ui *tui.UI, mode resumeMode, resumeID, modelKey string) *sessRec {
	store, err := session.NewStore()
	if err != nil {
		return nil
	}
	cwd := cwdOrDot()
	var pick func([]session.Info) (int, error)
	if ui != nil && mode == modeResumePick {
		pick = func(list []session.Info) (int, error) { return pickSessionRoot(ui, list) }
	}
	w, err := openSession(store, mode, cwd, resumeID, modelKey, pick)
	if err != nil {
		return nil
	}
	return &sessRec{store: store, w: w, rec: session.NewRecorder(w)}
}

// openSession picks the transcript to write per resumeMode. pick is called for
// modeResumePick to choose among saved roots; pass nil when no UI is available.
// openSession picks the transcript to write per resumeMode. id is used by
// modeResumeID to reopen one saved session directly. pick is called for
// modeResumePick to choose among saved roots; pass nil when no UI is available.
func openSession(store *session.Store, mode resumeMode, cwd string, id, modelKey string, pick func([]session.Info) (int, error)) (*session.Writer, error) {
	fresh := func() (*session.Writer, error) {
		return store.Create(cwd, session.SessionData{
			Version:   session.Version(),
			Workspace: cwd,
			Model:     modelKey, // provenance so a resume can stamp assistant origins
		})
	}

	switch mode {
	case modeContinue:
		info, lerr := store.Latest(cwd)
		if lerr == nil {
			return session.Open(info.Path) // resume the most recent transcript's leaf
		} else if errors.Is(lerr, session.ErrNoSessions) {
			return fresh()
		}
		return nil, lerr
	case modeResumeID:
		info, ferr := store.Find(cwd, id)
		if ferr != nil {
			return nil, ferr
		}
		return session.Open(info.Path) // resume that exact transcript's leaf
	case modeResumePick:
		list, lerr := store.List(cwd)
		if len(list) == 0 || errors.Is(lerr, session.ErrNoSessions) {
			return fresh() // nothing saved yet; start one
		} else if lerr != nil {
			return nil, lerr
		}
		picked, perr := -1, error(nil)
		if pick != nil { // no UI means we cannot choose; fall back to fresh
			picked, perr = pick(list)
		}
		if errors.Is(perr, tui.ErrCancelled) || picked < 0 {
			return fresh() // cancelled the resume; start new rather than stall
		} else if perr != nil {
			return nil, perr
		}
		return session.Open(list[picked].Path) // resume that root's leaf
	default: // modeNewSession and anything unexpected: always fresh
		return fresh()
	}
}

// promptStore returns the store backing Ctrl+R history search: the active
// recorder's store, or a fresh one when recording was disabled so prior
// transcripts stay searchable.
func promptStore(rec *sessRec) *session.Store {
	if rec != nil && rec.store != nil {
		return rec.store
	}
	st, err := session.NewStore()
	if err != nil {
		return nil
	}
	return st
}

// searchItems maps recorded prompts onto Ctrl+R candidate rows.
func searchItems(prompts []session.Prompt) []tui.SearchItem {
	out := make([]tui.SearchItem, 0, len(prompts))
	for _, p := range prompts {
		var detail string
		if !p.At.IsZero() { // typed-only lines have no transcript timestamp
			detail = p.At.UTC().Format("2006-01-02 15:04 UTC") // sessions are stored as UTC
		}
		out = append(out, tui.SearchItem{Text: p.Text, Detail: detail})
	}
	return out
}

// rewindBody returns a tree row's label without its leading kind word, which is
// rendered separately as a colored tag so the row kind reads at a glance.
func rewindBody(row session.TreeRow) string {
	prefix := ""
	switch row.Kind {
	case session.RowUser:
		prefix = "user: "
	case session.RowAssistant:
		prefix = "assistant: "
	case session.RowCompaction:
		prefix = "compaction: "
	}
	return strings.TrimPrefix(row.Label, prefix)
}

// roleTag maps a tree row kind to its colored tag and mark. Every kind carries
// one, so the tag column is never blank and the tree guides stay aligned.
func roleTag(kind session.RowKind) (string, tui.ItemMark) {
	switch kind {
	case session.RowUser:
		return "user", tui.MarkUser
	case session.RowAssistant:
		return "agent", tui.MarkAssistant
	case session.RowCompaction:
		return "compact", tui.MarkTool
	default:
		return "tool", tui.MarkTool
	}
}

// pickSessionRoot lists saved sessions (newest first) for the user to resume from.
func pickSessionRoot(ui *tui.UI, list []session.Info) (int, error) {
	items := make([]tui.PickItem, len(list))
	for i, in := range list {
		label := in.First
		if label == "" {
			label = "(empty session)"
		}
		detail := in.Started.UTC().Format("2006-01-02 15:04 UTC") // sessions are stored as UTC
		if in.Model != "" {
			detail += " · " + in.Model
		}
		if in.Messages > 0 {
			detail += fmt.Sprintf(" · %d msgs", in.Messages)
		}
		items[i] = tui.PickItem{Label: label, Detail: detail}
	}
	return ui.PickContext(context.Background(), "Resume session", items,
		tui.PickOptions{Placeholder: "filter"})
}

// restoreState reconstructs the agent state from the resumed branch, folding the
// transcript's setting overrides back into set. It returns the branch entries,
// its head and any warnings; a nil entry slice means there was nothing to
// restore. Best-effort: any problem leaves st untouched.
func (r *sessRec) restoreState(set *config.Set, reg *llm.Registry, st *agent.State, toolsReg *tools.Registry) ([]session.Entry, string, []string) {
	entries, _, err := session.Read(r.w.Path())
	if err != nil || len(entries) == 0 {
		return nil, "", nil
	}
	head := resumeHead(r.w.Head(), entries)
	rebuilt, warns := r.stateFor(modelResolver(reg), entries, head)
	if len(rebuilt.Messages) == 0 && rebuilt.Model.ID == "" {
		return nil, "", warns // a brand-new transcript carries no history yet
	}
	st.Messages = rebuilt.Messages
	if rebuilt.Model.ID != "" {
		st.Model = rebuilt.Model
	}
	// fold resumed setting overrides into the session config layer so Explain and
	// Settings report (session) instead of reverting to defaults. They come off the
	// branch, not raw file order: a transcript with forks holds settings from
	// siblings this head never saw, and the tool set among them shapes the request.
	set.SeedSession(session.SettingOverrides(session.Branch(entries, head)))
	resumed := set.Settings()
	if _, ok := llm.ParseLevel(resumed.Reasoning.Level); ok || resumed.Reasoning.Retain != "" {
		st.Reasoning = reasoningFrom(config.Reasoning{
			Level:  resumed.Reasoning.Level,
			Retain: resumed.Reasoning.Retain,
			Show:   resumed.Reasoning.Show,
		}, st.Model)
	}
	if toolsReg != nil && len(resumed.Tools.Enabled) > 0 {
		toolsReg.SetEnabled(resumed.Tools.Enabled)
	}
	st.Tokens = rebuilt.Tokens // a resumed ledger reflects the branch's recorded usage
	return entries, head, warns
}

// rebuild reconstructs the agent state from the resumed branch and replays it
// onto the UI so a reopened session shows its history. It is best-effort: any
// problem falls back to a fresh empty context.
func (r *sessRec) rebuild(set *config.Set, ui *tui.UI, reg *llm.Registry, st *agent.State, toolsReg *tools.Registry) {
	entries, head, warns := r.restoreState(set, reg, st, toolsReg)
	for _, wmsg := range warns {
		ui.Notify("resume: "+wmsg, tui.LevelWarn)
	}
	if entries == nil {
		return
	}
	// the restored model drives turns and preselects in /model, so the registry
	// and the status line must name it rather than the config default: without
	// this, picking it in /model reads as the no-op it is keyed on and the bar
	// keeps labelling a model the session is not running.
	if st.Model.ID != "" {
		syncModelUI(ui, reg, st.Model)
	}
	// the resumed palette must land before the replay bakes its colors into history
	if pal, ok := tui.LookupPalette(set.Settings().UI.Theme); ok {
		ui.SetTheme(pal)
	}
	session.Replay(session.Branch(entries, head), tuisink.New(ui), session.ReplayOptions{})
}

// bindRewind wires the double-Esc gesture to a picker over this transcript's
// branch. Picking an entry rewinds the writer and agent onto that point.
func (r *sessRec) bindRewind(ui *tui.UI, ag *agent.Agent, reg *llm.Registry) {
	if ui == nil || ag == nil {
		return
	}
	ui.SetOnRewind(func() { r.rewind(ui, ag, reg) })
}

// stateFor rebuilds agent state from a transcript's branch rooted at head.
func (r *sessRec) stateFor(resolve func(string) (llm.Model, error), entries []session.Entry, head string) (agent.State, []string) {
	return session.State(session.Branch(entries, head), resolve)
}

// switchState points the writer and the live agent state at head's branch. An
// empty head starts a new root, leaving an empty context behind. The display is
// untouched: callers that want the screen redrawn do that themselves. Warnings
// are reported under prefix; an unreadable transcript leaves everything as it
// was and reports the error.
func (r *sessRec) switchState(ui *tui.UI, ag *agent.Agent, reg *llm.Registry, head, prefix string) error {
	var rebuilt agent.State
	if head != "" {
		// rebuild before moving HEAD, so a failed read leaves writer and state in step
		entries, _, err := session.Read(r.w.Path())
		if err != nil {
			ui.Notify(prefix+err.Error(), tui.LevelWarn)
			return err
		}
		var warns []string
		rebuilt, warns = r.stateFor(modelResolver(reg), entries, head)
		for _, wmsg := range warns {
			ui.Notify(prefix+wmsg, tui.LevelWarn)
		}
	}
	r.w.SetHead(head)
	if r.onSwitch != nil {
		r.onSwitch(rebuilt.Messages)
	}

	live := r.liveModel(ag) // captured before the swap, for a branch that names no model

	// mutate the live state in place so every holder (the console, this handler)
	// sees the restored context.
	var ledger *tokens.Accounting
	ag.WithState(func(st *agent.State) {
		st.Messages = rebuilt.Messages
		if rebuilt.Model.ID != "" {
			st.Model = rebuilt.Model
		}
		if rebuilt.Tokens != nil {
			st.Tokens = rebuilt.Tokens // ledger rebuilt for exactly this branch point
		} else {
			st.Tokens = tokens.New(st.Model) // a new root starts an empty ledger
		}
		ledger = st.Tokens
	})
	// both of these need the agent lock WithState holds, so they run after it, not
	// inside the closure.
	if ledger != nil {
		if rebuilt.Model.ID == "" && live.ID != "" {
			// the branch named no model of its own: frame what the rebuild measured
			// against the live one rather than leaving a zero window, which would
			// rescale the bar off the compaction threshold onto the raw context size
			ledger.SetWindow(live)
		}
		ledger.SetBase(r.baseEstimate(ag))
	}
	pushSwitchedContext(ui, ledger)
	return nil
}

// baseEstimate sizes the constant request overhead for the branch just restored,
// counting tool schemas only once the tool block has been committed. It must not
// run inside WithState, whose agent lock Agent.BaseEstimate also takes.
func (r *sessRec) baseEstimate(ag *agent.Agent) int {
	var committed bool
	if r.started != nil {
		committed = *r.started
	}
	return ag.BaseEstimate(committed)
}

// pushSwitchedContext republishes the context bar after a ledger swap, which no
// sink event would otherwise report until the next turn. A nil accounting leaves
// the bar untouched.
func pushSwitchedContext(ui *tui.UI, t *tokens.Accounting) {
	if ui == nil || t == nil {
		return
	}
	cs := t.Context()
	ui.SetContext(tui.ContextInfo{
		Used: cs.Used, Window: cs.Window, Reserve: cs.Reserve,
		Compact: cs.Compact, Estimated: cs.Estimated,
	})
}

// liveModel returns the model currently driving turns, or a zero value when none.
func (r *sessRec) liveModel(ag *agent.Agent) llm.Model {
	if ag == nil {
		return llm.Model{}
	}
	save := llm.Model{}
	ag.WithState(func(st *agent.State) { save = st.Model })
	return save
}

// syncModelUI republishes a model switch that did not come through /model: the
// registry's active entry (the /model preselect) and the status line labels must
// name the model the session actually runs.
func syncModelUI(ui *tui.UI, reg *llm.Registry, m llm.Model) {
	reg.SetActive(m)
	if ui == nil {
		return
	}
	ui.SetModel(m.Key(), m.ShortName(), m.ContextWindow)
}

// restoreForkModel reapplies a model captured before switchState so a rewind does
// not silently revert to an earlier branch's model; it is a no-op for the zero one.
func (r *sessRec) restoreForkModel(ui *tui.UI, ag *agent.Agent, reg *llm.Registry, m llm.Model) {
	if m.ID == "" || ag == nil {
		return
	}
	var ledger *tokens.Accounting
	ag.WithState(func(st *agent.State) {
		st.Model = m
		if st.Tokens != nil {
			st.Tokens.SetModel(m)
			st.Tokens.Reseed(tokens.EstimateFor(m, st.Reasoning.Retain, st.Messages))
			ledger = st.Tokens
		}
	})
	// this deliberately overwrites what switchState seeded, so the base is measured
	// again here against the fork's model; BaseEstimate takes the lock WithState held
	if ledger != nil {
		ledger.SetBase(r.baseEstimate(ag))
	}
	pushSwitchedContext(ui, ledger)
	syncModelUI(ui, reg, m)
}

// resumeHead prefers the live head over the file tail; they differ after a
// rewind or a workflow that left HEAD on an earlier branch.
func resumeHead(live string, entries []session.Entry) string {
	if live != "" && slices.ContainsFunc(entries, func(e session.Entry) bool { return e.ID == live }) {
		return live
	}
	return session.Head(entries)
}

// modelResolver adapts the registry's Resolve to a plain key resolver.
func modelResolver(reg *llm.Registry) func(string) (llm.Model, error) {
	return reg.Resolve
}

// initialRow is the tree row the rewind picker opens on: where the context
// currently ends. Head is preferred; when it names an entry with no row of its
// own (a session or tool-only entry) the last row still in context stands in.
func initialRow(tree []session.TreeRow, head string) int {
	if i := slices.IndexFunc(tree, func(r session.TreeRow) bool { return r.ID == head }); i >= 0 {
		return i
	}
	for i := len(tree) - 1; i >= 0; i-- {
		if tree[i].Active {
			return i
		}
	}
	return len(tree) - 1
}

// rewind reads the transcript back off disk and offers a picker over its whole
// context tree, indented by depth so forks read as branches. Picking one of your
// messages rewinds *before* it — head moves to that message's parent — and pre-
// fills the editor with the picked text, ready to edit or re-send as the start of
// a new branch.
func (r *sessRec) rewind(ui *tui.UI, ag *agent.Agent, reg *llm.Registry) {
	entries, _, err := session.Read(r.w.Path())
	if err != nil || len(entries) == 0 {
		ui.Notify("nothing to rewind onto yet", tui.LevelInfo)
		return
	}
	head := resumeHead(r.w.Head(), entries)
	tree := session.TreeRows(entries, head)
	if len(tree) == 0 {
		ui.Notify("nothing to rewind onto yet", tui.LevelInfo)
		return
	}

	items := make([]tui.PickItem, len(tree))
	for i, row := range tree {
		tag, mark := roleTag(row.Kind)
		// Guide draws the branch ("├──", "└──", continuation bars); a flat trunk has none.
		// Off shades the rows no longer in context, so an abandoned fork recedes.
		items[i] = tui.PickItem{
			Label: row.Guide + rewindBody(row),
			Tag:   tag,
			Mark:  mark,
			Off:   !row.Active,
		}
	}
	picked, err := ui.PickContext(context.Background(), "Rewind to", items,
		tui.PickOptions{Placeholder: "filter", Initial: initialRow(tree, head)})
	if err != nil {
		return // cancelled
	}

	newHead, fillText, ok := session.RewindTarget(entries, tree[picked].ID)
	if !ok || newHead == "" {
		ui.Notify("cannot rewind onto that entry", tui.LevelWarn)
		return
	}
	// keep the current model across a fork: rebuilding from an earlier point
	// would otherwise revert to whatever model was active there, silently undoing
	// a /model switch when that prior message is re-sent.
	saveModel := r.liveModel(ag)
	if err := r.switchState(ui, ag, reg, newHead, "rewind: "); err != nil {
		return
	}
	r.restoreForkModel(ui, ag, reg, saveModel)

	// redraw to just the restored context, then drop the picked text into the
	// prompt so it can be edited or re-sent as this branch's first message.
	ui.Reset()
	// mark where restored history begins so it reads clearly in scrollback
	ui.Divider()
	session.Replay(session.Branch(entries, newHead), tuisink.New(ui), session.ReplayOptions{})
	if fillText != "" {
		ui.SetInput(fillText)
	}
}

// doublePressWindow is how long a second Ctrl+C on an idle editor must arrive
// within to quit, so a single stray interrupt never kills the app.
const doublePressWindow = 2 * time.Second

// watchControls interprets out-of-band keys: Esc and Ctrl+C interrupt a running
// turn, while Ctrl+D or a double Ctrl+C on an idle empty editor quits. Closing
// quit signals driver to return, which lets main's deferred ui.Close restore the
// terminal. onModeCycle runs when Shift+Tab is pressed; the front end wires it.
func watchControls(ui *tui.UI, ag *agent.Agent, q *steerQueue, stager *command.Stager, initCtl *initController, quit chan struct{}, onModeCycle, onInterrupt func()) {
	go func() {
		var lastInt time.Time
		for c := range ui.Controls() {
			switch c {
			case tui.ControlEscape:
				switch {
				case ag.Running():
					q.abort() // queued messages return to the editor, joined with newlines
					ag.Interrupt()
					onInterrupt() // queued sub-agent steers were dropped without their confirm
				case initCtl.abort(): // a minutes-long /init survey is escapable too
				case stager.Pending():
					stager.Cancel() // Esc cancels an in-flight staged shell command
				}
			case tui.ControlInterrupt:
				if ag.Running() {
					q.abort()
					ag.Interrupt()
					onInterrupt() // queued sub-agent steers were dropped without their confirm
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
				now := time.Now()
				if now.Sub(lastInt) < doublePressWindow {
					close(quit)
					return
				}
				lastInt = now
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
	}()
}

// discoverTimeout bounds a background model discovery pass, so an unreachable
// endpoint cannot leave it running for the life of the session.
const discoverTimeout = 30 * time.Second

// refreshModels updates the model list from the providers that can list their
// own, in the background so startup never waits on the network. A failure is
// only ever a notice: whatever was cached still works.
func refreshModels(ui *tui.UI, reg *llm.Registry) {
	ctx, cancel := context.WithTimeout(context.Background(), discoverTimeout)
	defer cancel()

	before := len(reg.Models())
	cache, warnings := reg.Refresh(ctx, llm.DiscoverOptions{})
	for _, w := range warnings {
		ui.Notify(w, tui.LevelWarn)
	}
	if err := llm.SaveUserCache(cache); err != nil {
		ui.Notify("could not save the model cache: "+err.Error(), tui.LevelWarn)
	}
	if added := len(reg.Models()) - before; added > 0 {
		ui.NotifyKeyed("models", "discovered "+strconv.Itoa(added)+" more models", tui.LevelInfo)
	}
}
