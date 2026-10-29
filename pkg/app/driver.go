package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
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

const (
	secretPrefix = "secret:"

	subAgentToolStart = "agent_start"
	subAgentToolPoll  = "agent_poll"
	subAgentToolList  = "agent_list"
)

// shutdownDrainWait bounds the exit wait for the drain goroutine's abort fill.
// Local work only: buffered file writes survive process exit and a cancelled
// provider read unblocks on context, so this is the worst-case bound.
const shutdownDrainWait = 100 * time.Millisecond

// Driver runs the real agent loop: it builds an Agent over the registry and
// drives turns from submitted messages, steering mid-turn input into the running
// turn rather than starting a second one. sessMode decides whether this run starts
// fresh or resumes a saved transcript, and sessTarget names it for the id and name
// modes. sessResolved carries CheckSessionTarget's resolution of sessTarget
// for the open to adopt directly. A nil term runs serve mode: the control
// socket is the only front end and a startup failure returns a non-nil error.
// Returns the transcript label for an exit-time `--resume` hint, empty when
// there is nothing to resume.
func Driver(term *tui.UI, set *config.Set, reg *llm.Registry, active llm.Model, sessMode ResumeMode, sessTarget string, sessResolved SessionResolution, args []string, scope ToolScope) (string, error) {
	serve := term == nil
	providers := llm.NewProviders(reg)
	rc := llm.ReasoningFrom(set.Settings().Reasoning, active)
	st := &agent.State{
		Model:     active,
		Reasoning: rc,
		Tokens:    tokens.New(active),
	}

	// the broker fronts every dialog before one can open (the resume picker
	// below is the first), and the hub makes every later surface change and
	// control entry cross one seam. Both exist before the first notice.
	dialogs := newInteractionBroker(term, nil, nil)
	env := agent.DetectEnvironment()
	hub := newSessionHub(hubDeps{
		ui: term, dialogs: dialogs, active: active,
		workspace: config.Cwd(), cwd: env.Cwd, acct: st.Tokens,
	}, hubEmitters{}) // a control server attaches its own observers later

	// every turn is recorded into the workspace transcript so double-Esc while idle
	// can open the context-tree picker and rewind onto an earlier point.
	var modelKey string
	if active.ID != "" {
		modelKey = active.Key() // a modelless start stays unstamped, "/" would warn on resume
	}
	rec, serr := newSession(term, dialogs, sessMode, sessTarget, modelKey, sessResolved)
	hub.bindSession(rec)
	ui := hub.ui // every consumer below sees the wrapper, never the raw TUI
	if serve && rec == nil {
		// every serve session is recorded: a transcript that cannot open is a
		// failed start, not an unrecorded run
		var reason string
		if serr != nil {
			reason = serr.Error()
		} else {
			reason = "recording is unavailable"
		}
		return "", fmt.Errorf("serve needs a recorded session: %s", reason)
	}
	if rec == nil {
		msg := "session recording disabled; Esc will not rewind"
		if serr != nil {
			// an explicit resume target that cannot open must not read as a
			// routine no-recording run
			msg = "session open failed: " + serr.Error() + "; recording disabled, Esc will not rewind"
		}
		ui.Notify(msg, tui.LevelWarn)
	} else if rec.note != "" {
		ui.Notify(rec.note, tui.LevelWarn) // e.g. --continue skipped newer-format sessions
	}

	// one app-lifetime root ties every derived operation (the prompt pump, staged
	// shell runs, setup probes) to process shutdown. cancel fires when quit does,
	// and again on any return path via the defer, so tool work dies with the app.
	// Signal handling installs here, ahead of the control socket binding below,
	// so a socket that exists proves a signal will be caught.
	appRoot, stopApp := context.WithCancel(context.Background())
	defer stopApp()

	quit := make(chan struct{})

	go func() {
		select {
		case <-quit:
			stopApp()
		case <-appRoot.Done():
		}
	}()

	// quitNow ends the run from any path, idempotent so a signal racing a user
	// quit is dropped
	var quitOnce sync.Once
	quitNow := func() {
		quitOnce.Do(func() { close(quit) })
	}

	// shutdownDone closes when teardown finishes, retiring the force-exit path
	shutdownDone := make(chan struct{})

	// signals quit through the driver so teardown runs, plain mode included
	// where Ctrl+C arrives as SIGINT with no keyboard controls to catch it.
	// A second signal while teardown is still running force-exits, restoring the
	// terminal first since the process dies without unwinding further.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	go func() {
		select {
		case <-sig:
			quitNow()
		case <-quit:
			return
		}
		select {
		case <-sig:
			if term != nil {
				ui.Close()
			}
			os.Exit(ExitTurn)
		case <-shutdownDone:
		}
	}()

	// the control socket binds before any loop starts, so a controller never
	// races creation. Serve mode always has one (its only front end); an
	// interactive run needs the deliberate config opt-in.
	var ctlSrv *controlServer
	if serve || set.Settings().Control.Enabled {
		srv, warns, cerr := startControl(hub, dialogs, rec, serve)
		for _, warn := range warns {
			// serve mode has no terminal: the supervisor's stderr is the surface
			if serve {
				_, _ = fmt.Fprintln(os.Stderr, "ajent: control: "+warn)
			} else {
				ui.Notify("control: "+warn, tui.LevelWarn)
			}
		}
		ctlSrv = srv
		if cerr != nil {
			if serve {
				return "", fmt.Errorf("control socket: %w", cerr)
			}
			ui.Notify("control: "+cerr.Error(), tui.LevelWarn) // alive peer names the session, run continues
		}
	}

	// the terminal sink renders turns locally; serve mode runs hub-only, its
	// sink (fanout plus spinner tracking) standing in as the front end
	var sink agent.Sink
	if term != nil {
		sink = tuisink.New(term)
	} else {
		sink = hub.sink()
	}

	// build the built-in tool registry and hand it to the loop so the model can
	// read, write, edit and run commands.
	// ask_user rides the TUI's question queue, so it never pre-empts a permission
	// dialog. It stays disabled until a workflow enables it.
	toolsReg, terr := builtinTools(set, reg, askUser(dialogs), func(msg string) { ui.Notify(msg, tui.LevelWarn) })
	if terr != nil {
		ui.Notify("tools disabled: "+terr.Error(), tui.LevelWarn)
	}
	hub.tools = toolsReg
	// a configured enabled set replaces the built-in default (read/write/edit/bash),
	// so find/grep/ls come on when config.json lists them.
	if toolsReg != nil && len(set.Settings().Tools.Enabled) > 0 {
		toolsReg.SetEnabled(set.Settings().Tools.Enabled)
	}

	// the compactor is wired lazily so the agent options can close over it before
	// the *Agent it needs exists, and it is assigned once, right after agent.New.
	var comp *compactor
	// user-global instructions layer before the project's, so the more specific
	// cwd file comes later in context. An unresolvable home is skipped silently.
	globalDir, _ := config.Home()
	proj, perr := agent.LoadProjectInstructions(globalDir, env.Cwd)
	if perr != nil {
		ui.Notify("could not read AGENTS.md: "+perr.Error(), tui.LevelWarn)
	}
	opts := agent.Options{
		Sinks:               []agent.Sink{sink},
		Env:                 env,
		ProjectInstructions: proj,
		SystemPrompt:        set.Settings().Agent.SystemPrompt, // config or --system, replaces default guidance
		Tools:               toolsReg,
		Provider: func(m llm.Model) (llm.Provider, error) {
			return providers.ProviderFor(m)
		},
		Compact: func(ctx context.Context, reason agent.CompactReason) (bool, error) {
			if comp == nil {
				return false, nil // recording is off, nothing to compact
			}
			return comp.run(ctx, reason, "")
		},
		TurnBoundary: func() {
			if comp != nil {
				comp.endTurn()
			}
		},
		MaxSteps:    set.Settings().Agent.MaxSteps, // <= 0 or unset means unlimited
		TurnRetries: set.Settings().Agent.TurnRetries,
		SessionID:   sessionHint(rec),
	}
	if term != nil {
		opts.Sinks = append(opts.Sinks, hub.sink()) // hub fans out after the renderer
	}
	if rec != nil {
		// recorder first, hub after: the order 34a's clients rely on. Serve mode
		// wraps the hub sink directly, no renderer existing
		opts.Sinks = []agent.Sink{rec.rec.Sink(sink)}
		if term != nil {
			opts.Sinks = append(opts.Sinks, hub.sink())
		}
		opts.OnMessage = []func(agent.MessageInfo){rec.rec.Message}
		rec.rebuild(set, ui, reg, st, toolsReg)
	}

	// the editor's in-progress text feeds accounting (SetOnEdit below) and clears on
	// submission via settled. editSinks may be empty before a session is set up.
	editSinks := opts.Sinks // may be empty before a session is set up
	pushContext := func() {
		if len(editSinks) == 0 {
			return
		}
		c := st.Tokens.Context()
		for _, s := range editSinks {
			s.Context(c)
		}
	}
	// once a submitted prompt and everything behind it lands in state, pending owns
	// its tokens. The submit bucket must clear so they are never counted twice.
	settled := func() {
		if len(editSinks) > 0 {
			st.Tokens.SetSubmit(0)
			pushContext()
		}
	}

	// the stager owns `!` shell lines: they run immediately through the real bash
	// tool and stage their output for delivery to the model. Built before the
	// steer queue, whose delivery paths flush staged output into the batch.
	stager := command.NewStager(appRoot, toolsReg, sink)
	hub.stager = stager // prompts submitted while a turn runs queue here: they render as dimmed rows,
	// hand over at the next step boundary (or the next turn), and recover to the
	// editor on interrupt or Alt+Up. Staged shell output rides whichever prompt
	// delivers, never a submission that might be recalled or aborted.
	q := newSteerQueue(ui,
		func(est int) { submitPrompt(st, editSinks, est, pushContext) },
		settled,
		stager.WaitReady,
		stager.TakeReady,
	)
	hub.q = q
	// /copy reads the transcript as of the last turn end while a turn streams,
	// never the live list the loop goroutine is appending to
	snap := &turnSnapshot{st: st}
	snap.seed(st.Messages)
	// hints arbitrates the one hint status line across its sources: the typing hold and
	// the control loop's quit/cancel notices
	hints := uiHintBoard(ui)
	// typingGate holds the next step boundary while the user is mid-message, so a
	// prompt they are still typing lands in this step instead of behind it.
	searching := func() (bool, time.Time) { return false, time.Time{} } // serve: no editor, no search
	if term != nil {
		searching = ui.Searching
	}
	gate := &typingGate{
		idle:      typingIdle,
		handoff:   typingHandoff,
		poll:      typingPoll,
		pending:   q.pending,
		searching: searching,
	}
	// the hold speaks through the shared hint board, so a quit countdown can
	// take the line and hand it back to a still-held boundary when it releases.
	gate.status = hints.Line().Set
	// the editor's in-progress text feeds accounting so the context bar grows as you
	// type or paste, then clears once submitted (the buffer empties), and it is also the
	// typing signal the boundary hold reads. Serve mode has no editor: drafts arrive
	// only through host fills on the hubUI wrapper.
	if term != nil {
		ui.SetOnEdit(func(text string) {
			gate.edit(text)
			hub.draft(text) // coalesced, secret-suppressed, droppable
			if len(editSinks) == 0 {
				return
			}
			st.Tokens.SetCompose(tokens.EstimateText(text, tokens.KindProse))
			pushContext()
		})
	}
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
			Tools:               childToolSource{reg: toolsReg},
			Env:                 env,
			ProjectInstructions: proj,
			Root:                appRoot, // job contexts die with the app root, not only at Close

			Activity: func(key, text string, rank int) {
				if ui != nil {
					// ranked by job number so rows hold their place, oldest first,
					// however the parallel agent_start calls happen to publish
					ui.SetActivityRanked(key, text, rank)
				}
			},
			Notice: func(msg string) { ui.NotifyKeyed("subagent", msg, tui.LevelInfo) },
			Status: func(text, short string) { ui.SetStatusSegment(segment(segSubagents, text, short)) },
			Deliver: func(in agent.Input) bool {
				if ag == nil || !ag.Running() { // never start a turn on an idle parent
					return false
				}
				return ag.Steer(in)
			},
			MaxConcurrent: set.Settings().Subagent.MaxConcurrent,
			PollTimeout: subagentPollWait(set, func(msg string) {
				ui.Notify(msg, tui.LevelWarn)
			}),
			MaxSteps: set.Settings().Subagent.MaxSteps,
		})
		for _, t := range sag.Tools() {
			// builtin source so /tools sorts the trio up front with core tools
			// SelfBounded: a poll result is the deliverable, never spilled to a file
			toolsReg.RegisterFrom(tools.SourceBuiltin, tools.SelfBounded(t), true) // enabled by default, /tools toggles
		}
		// the trio toggles together in /tools under one "subagents" row.
		toolsReg.RegisterGroup(tools.ToolGroup{
			Name:   "subagents",
			Source: tools.SourceBuiltin,
			Tools:  []string{subAgentToolStart, subAgentToolPoll, subAgentToolList},
		})
		// agent_* delegate read-only work, so allow-read runs them free.
		toolsReg.MarkReadOnly([]string{subAgentToolStart, subAgentToolPoll, subAgentToolList})
		// a resumed session's recorded set re-applies after every synchronous
		// registration (the trio above) so late registrants honor the resumed frame.
		// Unknown names are ignored; MCP tools take the set from Restore on connect.
		// An empty recorded set means the session ran with everything off, not
		// unrecorded: disable all rather than fall back to config defaults.
		if st.Tools != nil {
			toolsReg.SetEnabled(st.Tools)
		}
		// flush pending completion steers into the running parent turn at start.
		opts.Sinks = append(opts.Sinks, subagentSink{mgr: sag})
	}

	// record each turn's outcome so a turn-boundary hook can tell a clean stop
	// from an abort or a provider error.
	turnRec := &turnRecorder{}
	opts.Sinks = append(opts.Sinks, turnRec, snap)

	// /init's write lands inside a normal turn, and this watches for it so the driver
	// can say the new file only applies on the next start.
	initSeen := &initWatch{notify: ui.Notify}
	opts.Sinks = append(opts.Sinks, initSeen)

	// queued mid-turn prompts land at the next step boundary via this hook.
	opts.OnBoundary = q.pull
	// AwaitInput may hold that boundary while the user finishes a message, so a
	// prompt typed during it lands in this same step rather than behind another call.
	opts.AwaitInput = gate.hold

	// the permission barrier gates every tool call through static classification and
	// an approval dialog. Read-only work runs free, but writes prompt unless allowed or
	// blocked by mode. It starts from the resolved config default so a restart always
	// uses the configured (or CLI-flagged) mode. In-session cycling never persists.
	// It must exist before agent.New: Options is copied there, so the batch hooks
	// (sub-agent id reservation, classification prefetch) wire directly, not through
	// a forward reference.
	idx := refs.NewIndex(config.Cwd()) // one path index feeds the prompt completer and reason @ refs
	var barrier *permit.Barrier
	if toolsReg != nil {
		var po permit.Options
		if mstr := set.Settings().Permissions.Mode; mstr != "" {
			if m, ok := permit.ParseMode(mstr); ok {
				po.Mode, po.ModeSet = m, true
			}
		}
		// a CLI permission flag overrides the configured default for this invocation
		// only. It never persists and is not stored in any session.
		if m, ok := scope.barrierMode(); ok {
			po.Mode, po.ModeSet = m, true
		}
		// the ask-a-person paths install as one unit: dialogs open through the
		// broker, note injection steers the running turn without stopping
		// it, and Hold runs the typing gate's dialog hold so an approval dialog
		// never steals a draft's focus.
		po.Dialogs = &permit.Dialogs{
			Prompter: promptAdapter{ui: ui, hold: gate.holdDialog, path: command.NewPathCompleter(idx)},
			Noter: func(note string) {
				// system context, not a user prompt, echoed where it lands
				ag.Steer(agent.Input{Text: note, Echo: note, Injected: true})
			},
			// the full diff is already committed above the dialog by guardedTool.Execute,
			// so the subject names it rather than repeating a truncated copy.
			Preview: func(call agent.ToolCall) string {
				ch, ok := toolsReg.Preview(call)
				if !ok {
					return ""
				}
				return tui.DiffSummary(ch.Path, ch.Before, ch.After)
			},
		}
		bd := barrierOptions{
			reg:         toolsReg,
			providerFor: providers.ProviderFor,
			model:       func() llm.Model { return st.Model },
			session:     sessionHint(rec),
			notify:      func(msg string, level agent.Level) { ui.Notify(msg, tui.Level(level)) },
			safe:        set.Settings().Permissions.SafeCommands,
			denied:      set.Settings().Permissions.DeniedCommands,
		}
		barrier = permit.NewBarrier(toolsReg.ReadOnly, bd.options(po))
		bd.install(barrier)
		showPermissionIndicator(ui, barrier)
		hub.barrier = barrier

		// OnToolBatch hands each step's calls (in message order) to sub-agent id
		// reservation and permission prefetch, wired before agent.New copies Options.
		wireBatchHooks(&opts, sag, barrier)
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

	// steered, follow-up and host-supplied inputs expand through the same @
	// pipeline a submitted prompt gets, via the agent's append-point seam.
	// vision reads the registry's active model, the same source the read
	// tool's gate pulls from, so @ and read can never disagree.
	expander := refs.NewExpander(toolsReg, sink, tools.PathPolicy{Cwd: config.Cwd()},
		func() bool { return reg.Active().Caps.Images })
	opts.NormalizeInput = func(in agent.Input) agent.Input {
		return refs.Normalize(expander, in, func(n string) { ui.Notify(n, tui.LevelWarn) })
	}

	ag = agent.New(st, opts)
	hub.ag = ag
	hub.syncLedger() // a resume rebuilt the ledger snapshots read

	// starts answers the run's two dispatch questions: committed gates whether
	// tool schemas count toward the bar (a resumed branch with history already
	// sent one), dispatched gates whether /tools may still reshape the set.
	starts := newStartState(len(st.Messages) > 0)

	// seed the constant request overhead (system + AGENTS.md) so the bar is honest
	// from startup. Tool schemas join only once the block is committed, since until
	// then /tools can still take one away. The pump re-seeds at the first prompt,
	// when MCP servers have connected and their schemas are in the registry, and
	// Agent.stream's own SetBase replaces this floor once a turn actually starts.
	var seedToolsOnce sync.Once
	st.Tokens.SetBase(ag.BaseEstimate(starts.committed.marked()))
	pushContext()

	if rec != nil {
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

	// the prompt is at rest until a turn starts, double-Esc rewinds from here
	ui.SetIdle(true)

	showReasoningIndicator(ui, set, st)

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
			Status:    func(text, short string) { ui.SetStatusSegment(segment(segMCP, text, short)) },
		})
		// dial every server in the background now so spawn + discovery hide behind
		// typing. LoadOnFirstMessage waits for these before a prompt is built.
		mgr.Preload()
	}

	// the command registry, shell stager and @ expander own the single dispatch path
	// for submitted lines. Commands run inline. Shell lines stage and flush ahead of
	// the next prompt. Prompts expand @ refs and steer the agent.
	cmds := command.NewRegistry()
	// `!` output is context the next prompt will carry, so the bar counts it from
	// the moment the command finishes rather than waiting for a submission
	stager.SetOnChange(func(est int) {
		if len(editSinks) == 0 {
			return
		}
		st.Tokens.SetStaged(est)
		pushContext()
	})
	if rec != nil {
		rec.discardStaged = stager.Discard
	}
	pump := make(chan pumpLine, 16)
	// send queues a line for the pump. The pump is never closed: runPump exits via
	// appRoot, so the arm drops the line once shutdown cancels it instead of blocking
	// on a full channel no one drains.
	send := func(line pumpLine) {
		select {
		case pump <- line:
		case <-appRoot.Done():
		}
	}
	console := &uiConsole{
		ui: ui, set: set, reg: reg, st: st, tools: toolsReg, commands: cmds,
		starts: starts, quit: quitNow, permit: barrier,
		q: q, snap: snap, hub: hub, dialogs: dialogs,
	}
	console.refreshBase = func() {
		// BaseEstimate reports 0 while a turn owns State, and that turn's own SetBase
		// picks a widened tool block up at its next step, so skip rather than zero it
		if est := ag.BaseEstimate(starts.committed.marked()); est > 0 {
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
	// the plan workflow needs a transcript to branch and a registry to scope,
	// without either it is simply absent and nothing else changes.
	ctl := newPlanController(planDeps{
		rec: rec, ag: ag, reg: reg, st: st, ui: ui, console: console, toolsReg: toolsReg, q: q,
	})
	hooks := planHooksFor(ctl, turnRec)
	// the survey needs the tool registry to run read and agent_* through. Without
	// one /init is simply absent, like the plan workflow without a transcript.
	ictl := newInitController(initDeps{
		cwd: config.Cwd(), toolsReg: toolsReg, sink: sink, ag: ag,
		notify: ui.Notify, agents: sag, watch: initSeen, pump: pump,
	})
	hub.initCtl = ictl
	if ictl != nil {
		ictl.runner.Seed(st.Messages) // a resumed transcript already holds /init ids
	}
	command.RegisterBuiltins(cmds, console)
	for _, c := range append(planCommands(ctl), initCommands(ictl)...) {
		cmds.Register(c)
	}
	if ctl != nil && comp != nil {
		// automatic compaction inside a phase keeps its own focus, but an explicit
		// /compact <instructions> still wins.
		comp.focus = ctl.Focus
	}
	expander.Seed(st.Messages) // a resumed transcript already holds ref ids
	if rec != nil {
		rec.starts = starts
		rec.onSwitch = func(msgs []llm.Message) {
			hub.syncLedger() // a switch replaced the ledger snapshots read
			hub.setHead(rec.w.Head())
			if t := toolsReg.Tracker(); t != nil {
				t.Reset() // reads the new context lacks must re-inject, not dedupe
			}
			expander.Seed(msgs)
			if ictl != nil {
				ictl.runner.Seed(msgs) // a rewound or forked transcript holds /init ids too
			}
			snap.seed(msgs) // mid-turn /copy reflects the switched context, not the old branch
		}
	}
	if term != nil {
		ui.SetCompleter(command.NewCompleter(cmds, console, idx))
	}

	// Ctrl+R searches this workspace's typed lines merged with recorded prompts
	// across sessions; plain ↑/↓ recall only the open session's own tree, falling
	// back to that search scope until the session has prompts of its own. Every
	// submitted line also lands in the editor-history file now. Serve mode keeps
	// no recall store: there is no editor to recall into.
	var hist *session.EditorHistory
	if term != nil {
		if store := promptStore(rec); store != nil {
			hist, _ = session.NewEditorHistory(store, config.Cwd(), secretPrefix)
			defer hist.Compact() // bounded rewrite at exit, last writer wins under concurrency
			idx := session.NewRecallIndex(store, config.Cwd(), secretPrefix, hist)
			ui.SetHistorySearch(func() []tui.SearchItem { return searchItems(idx.Lines()) })
			if rec != nil && rec.w != nil {
				sess := session.NewTranscriptIndex(rec.w.Path(), secretPrefix)
				ui.SetPromptHistory(func() []string { return session.PromptTexts(sess.Prompts()) })
			}
		}
	}

	// shared dispatch for every submitted line, so a second source routes unchanged.
	// Wired before the control loop starts, so every hub entry is live from the
	// first keypress.
	router := &lineRouter{ui: ui, send: send, stager: stager, gate: gate, hist: hist}
	hub.setSubmitLine(router.submit)
	if term != nil {
		go controlLoop(appRoot, ui.Controls(), hints, hub, quitNow)
	}

	// the rewind entry both fronts share: a manual double-Esc and a remote
	// session.rewind end an active plan workflow first, then switch branches.
	// Wired after the plan controller exists; a control request that races the
	// wiring sees the unavailable error, never a partial rewind.
	if rec != nil {
		rewindTo := func(head string) (string, error) {
			if ctl != nil && ctl.Active() {
				ctl.Stop()
			}
			return rec.rewindTo(ui, ag, reg, head)
		}
		hub.setRewindTo(rewindTo)
		if term != nil {
			ui.SetOnRewind(func() {
				if head, ok := rec.pickRewind(ui); ok {
					_, _ = rewindTo(head)
				}
			})
		}
	}
	if ctl != nil {
		ctl.Restore() // pick a mid-workflow session back up before the first prompt
	}
	if ctlSrv != nil {
		// wiring is complete, so every hub entry and snapshot source reads
		// consistent: connection loops may start dispatching against it now
		ctlSrv.start()
	}

	// a first start with no palette chosen picks one before any output exists;
	// serve mode renders nothing, so no palette applies
	if term != nil && ui.Mode() != tui.ModePlain {
		if terr := command.ThemeSetup(appRoot, console); terr != nil {
			ui.Notify("theme: "+terr.Error(), tui.LevelWarn)
		}
	}

	// a first start with nothing configured walks through the first provider
	// (the probe and prompts abandon when the user quits mid-setup, so the exit is
	// never held up by a discovery pass). It rides appRoot, which cancels on quit.
	if len(reg.ProviderNames()) == 0 {
		if err := command.ProviderSetup(appRoot, console); err != nil {
			if errors.Is(err, tui.ErrCancelled) {
				ui.Notify("provider setup skipped; add one to ~/.ajent/"+llm.ModelsFileName, tui.LevelInfo)
			} else {
				ui.Notify("setup: "+err.Error(), tui.LevelWarn)
			}
		}
		if m := reg.Active(); m.ID != "" && m.Key() != st.Model.Key() {
			console.SetModel(m) // applies the model, the bar and the session record
			st.Tokens.SetBase(ag.BaseEstimate(starts.committed.marked()))
			pushContext()
		}
	}
	if reg.Active().ID == "" {
		ui.Notify("no model configured; use /model to pick one", tui.LevelWarn)
	}

	var drain drainWaiter
	// a drain exit wakes the pump so queued state writes land without input
	drain.wake = func() { send(pumpLine{wake: true}) }
	go runPump(appRoot,
		pump, ag, console, stager, expander, rec != nil, ui, starts,
		settled, q, gate, st, editSinks, &seedToolsOnce, pushContext, hooks, &drain)

	// an argv prompt is the user's own input, recalled like a typed line. It carries no
	// editor escape, so Append sees the very text the transcript records and its secret
	// exclusion cannot diverge from the transcript-derived indexes.
	if initial := argvPrompt(args); initial != "" {
		hist.Append(initial) // durable in the workspace store for ↑/↓ and Ctrl+R
		// echo and accounting happen in the pump like every other prompt.
		send(pumpLine{kind: command.KindPrompt, rest: initial})
	}

	// teardown order is load-bearing: cancel the root, interrupt, bounded drain
	// wait, component closes. The root cancel stops the running turn and bars
	// new ones (a boundary compaction included), so the wait only covers the
	// abort fill landing. The UI closes after Driver returns, never onto a
	// restored terminal.
	//
	// Run explicitly at each loop return before finish reads the transcript, and
	// again from a defer as panic safety. The once makes the pair idempotent.
	var shutdownOnce sync.Once
	shutdown := func() {
		shutdownOnce.Do(func() {
			stopApp()
			ag.Interrupt()
			drain.wait(shutdownDrainWait)

			var wg sync.WaitGroup
			if sag != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					sag.Close() // cancel every running investigation and wait briefly
				}()
			}
			if toolsReg != nil {
				toolsReg.Close() // kill background commands so none outlives the run
			}
			if mgr != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					mgr.Close()
				}()
			}
			wg.Wait()
			if ctlSrv != nil {
				// session.exit flushes to every observer, then the socket unlinks
				ctlSrv.Close("")
			}
			close(shutdownDone)
		})
	}
	defer shutdown()

	// serve mode reads no editor lines: the nil channel blocks this case out of
	// the select and the run ends on quit alone
	var editorLines <-chan string
	if term != nil {
		editorLines = ui.Messages()
	}
	for {
		select {
		case msg, ok := <-editorLines:
			if !ok {
				// UI went away (stdin EOF): same ordered teardown as a user quit
				shutdown()
				return rec.exitLabel(), nil // drop an abandoned session, else the resume hint label
			}
			router.route(msg, true)
		case <-quit:
			shutdown()
			return rec.exitLabel(), nil // drop an abandoned session, else the resume hint label
		}
	}
}

// appendHistory records one submitted editor line on the workspace recall store;
// a nil hist is a no-op. The pasted-secret exclusion is decided here, once per
// submission and always on line.Rest (the exact text reaching the transcript), so
// a raw line's escape space cannot land an excluded value in the durable file.
func appendHistory(hist *session.EditorHistory, prefix string, msg string, line command.Line) {
	switch {
	case line.Kind == command.KindCommand:
		hist.AppendHidden(msg) // slash commands stay durable yet excluded from ↑/↓ and Ctrl+R
	case line.Kind == command.KindPrompt && session.HasSecretPrefix(prefix, line.Rest):
		// skipped from history exactly as the transcript-derived indexes drop it
	default:
		hist.Append(msg) // every prompt and !shell line recorded for recall
	}
}

// lineRouter dispatches every submitted line: shell lines stage at once,
// commands and prompts queue for the pump. Editor-coupled side effects (recall
// history, clipboard image tokens) stay behind fromEditor.
type lineRouter struct {
	ui     *hubUI
	send   func(pumpLine)
	stager *command.Stager
	gate   *typingGate
	hist   *session.EditorHistory // nil when no recall store exists
}

// route parses one submitted editor line and hands it to its dispatcher.
// fromEditor marks a line that left the prompt editor. Other sources pass
// false and carry no origin.
func (r *lineRouter) route(msg string, fromEditor bool) {
	r.dispatch(msg, fromEditor, "")
}

// submit routes one line from a non-editor source: identical classification,
// editor-coupled side effects excluded, origin recorded where the line lands
// on the transcript.
func (r *lineRouter) submit(line, origin string) {
	r.dispatch(line, false, origin)
}

func (r *lineRouter) dispatch(msg string, fromEditor bool, origin string) {
	line := command.ParseLine(msg)
	if line.Multiline {
		r.ui.Notify("multiline input sent as a prompt; /commands must be one line", tui.LevelWarn)
	}
	var blocks []llm.Block
	if fromEditor && line.Kind == command.KindPrompt {
		// image tokens become blocks, and the recorded line keeps them, so
		// recall restores the text an image rode in on. Orphaned tokens
		// (slot consumed earlier) drop here with a notice.
		var notices []string
		line.Rest, blocks, notices = pendingImages.take(line.Rest)
		for _, n := range notices {
			r.ui.Notify(n, tui.LevelWarn)
		}
	}
	if fromEditor {
		appendHistory(r.hist, secretPrefix, msg, line)
	}
	switch line.Kind {
	case command.KindShell:
		r.stager.Run(line.Rest, line.Excluded)
	case command.KindCommand:
		r.send(pumpLine{kind: command.KindCommand, rest: line.Rest})
	default:
		// arm the handoff at submit time. A remote prompt arms too so a held
		// boundary treats it like a just-typed line
		r.gate.submitted()
		r.send(pumpLine{kind: command.KindPrompt, rest: line.Rest, origin: origin, blocks: blocks})
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

// subagentPollWait resolves subagent.maxPollWait for the manager's poll
// timeout; zero falls back to pkg/subagent's built-in default.
func subagentPollWait(set *config.Set, warn func(string)) time.Duration {
	raw := set.Settings().Subagent.MaxPollWait
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		warn(fmt.Sprintf("unknown subagent.maxPollWait %q, using the built-in default", raw))
		return 0
	}
	return d
}

const (
	doublePressWindow = 10 * time.Second
	// hintNoticeTTL is how long a one-shot control notice (a cancelled survey or staged
	// command) holds the hint line before releasing it to whatever claimed it before.
	hintNoticeTTL = 6 * time.Second
)

// abortRunning stops a running turn Esc-style: queued prompts return to the
// editor, the stream cancels, any staged shell beside it dies too. Reports
// whether a turn ran so callers can fall through to other cancel targets.
func abortRunning(ag *agent.Agent, q *steerQueue, stager *command.Stager) bool {
	if !ag.Running() {
		return false
	}
	q.abort()
	ag.Interrupt()
	if stager.Pending() {
		stager.Cancel() // a staged shell runs beside the turn, stop it too
	}
	return true
}

// controlLoop serves the terminal's out-of-band keys until the UI goes away,
// routing each through the same hub methods an out-of-process client calls.
func controlLoop(ctx context.Context, controls <-chan tui.Control, hints *hintBoard, hub *sessionHub, quitNow func()) {
	ui := hub.ui
	// armed is when the first Ctrl+C landed, and quitHint fires to retire the hint
	// that advertises the window, keeping the gesture and the hint the same
	// length. The window is measured from armed, not from the timer, so a press
	// arriving as the timer fires cannot lose the arm to select's coin flip.
	var armed time.Time
	var quitHint <-chan time.Time
	// arm holds the hint line for as long as the double-press window is open, so an
	// older claim (a typing hold) reclaims it the moment the window closes.
	arm := hints.Line()
	for {
		select {
		case <-quitHint:
			quitHint = nil
			arm.Set("", "") // the window closed unanswered: free the line
		case c, ok := <-controls:
			if !ok {
				return // the UI went away
			}
			switch c {
			case tui.ControlClipboardImage:
				// off the control loop: a slow backend must not delay interrupts
				go func() {
					token, notice := captureClipboardImage(ctx)
					switch {
					case notice != "":
						ui.Notify(notice, tui.LevelWarn)
					case token != "":
						ui.Insert(token)
					}
				}()
			case tui.ControlCopySelection:
				// Resolve which row is highlighted here, on the loop and under lock,
				// so the payload matches what was pressed even if navigation moves the
				// cursor before the slow clipboard write finishes. Only that write runs
				// off-loop, like the image probe.
				text, ok := ui.CopySelection()
				if !ok {
					continue // a stale ctrl+x outlived its picker: silent no-op
				}
				go func() {
					notice, level, ok := copyText(ctx, ui, text)
					if ok {
						ui.Notify(notice, level)
					}
				}()
			case tui.ControlEscape:
				hub.Interrupt()
			case tui.ControlInterrupt:
				switch hub.Interrupt() {
				case stopTurn:
					continue
				case stopSurvey:
					hints.ShowFor("cancelled project survey", "", hintNoticeTTL)
					continue
				case stopShell:
					hints.ShowFor("cancelled shell command", "", hintNoticeTTL)
					continue
				}
				if !armed.IsZero() && time.Since(armed) < doublePressWindow { // inside the promised window
					// teardown still has to run, say so rather than leaving the
					// "again to quit" hint up, which reads as a press that missed
					hints.Request("quitting…", "")
					quitNow()
					return
				}
				armed = time.Now()
				quitHint = time.After(doublePressWindow)
				arm.Set("ctrl+c again to quit", "")
			case tui.ControlEOF:
				if hub.ag != nil && hub.ag.Running() {
					continue // ignored while a turn streams, per the key table
				}
				quitNow()
				return
			case tui.ControlRecallQueued:
				hub.q.recall() // Alt+Up: pop the newest queued message back into the editor
			case tui.ControlModeCycle:
				hub.ModeCycle(false)
			case tui.ControlModeCycleBack:
				hub.ModeCycle(true)
			}
		}
	}
}
