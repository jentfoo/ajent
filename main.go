package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/history"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/refs"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
	tuisink "github.com/jentfoo/ajent/pkg/tui/sink"
)

// secretPrefix marks editor lines excluded from persistent history, so a pasted
// secret never round-trips through ~/.ajent/history.
const secretPrefix = "secret:"

func main() {
	var modelFlag string
	flag.StringVar(&modelFlag, "m", "", "initial model to use")
	flag.StringVar(&modelFlag, "model", "", "initial model to use")
	render := flag.String("render", "auto",
		"paint mode: auto, inline (terminal scrollback, unsupported under tmux or screen), "+
			"alt (own scrollback), plain")
	cont := flag.Bool("continue", false, "resume the most recent session automatically")

	// --resume takes an optional id: bare means pick among saved roots, with a
	// value it reopens that exact transcript. It is parsed out of argv first so
	// flag.Parse does not greedily consume a following positional argument.
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		_, _ = fmt.Fprintf(out, "usage of %s:\n", os.Args[0])
		flag.PrintDefaults()
		_, _ = fmt.Fprintln(out, "  -resume [id]")
		_, _ = fmt.Fprintln(out, "    \tlist saved sessions and resume one; with an id, resume that session directly (new by default)")
	}

	resumeGiven, resumeID, argv := extractResume(os.Args[1:])
	_ = flag.CommandLine.Parse(argv)

	// --resume overrides --continue; neither means a brand-new session.
	sessMode := modeNewSession
	if *cont {
		sessMode = modeContinue
	}
	switch {
	case resumeGiven && resumeID != "":
		sessMode = modeResumeID // reopen that exact saved transcript by id
	case resumeGiven:
		sessMode = modeResumePick // pick among saved roots, then resume its leaf
	}

	// A requested session id must resolve before the TUI opens; otherwise fail fast
	// with a clear message instead of silently starting a fresh transcript.
	if sessMode == modeResumeID {
		sid := cwdOrDot()
		store, err := session.NewStore()
		if err != nil {
			fmt.Fprintf(os.Stderr, "ajent: %v\n", err)
			os.Exit(1)
		}
		if _, ferr := store.Find(sid, resumeID); ferr != nil {
			fmt.Fprintf(os.Stderr, "ajent: no session matches id %q\n", resumeID)
			os.Exit(2)
		}
	}

	mode, ok := tui.ParseMode(*render)
	if !ok {
		fmt.Fprintf(os.Stderr, "ajent: unknown render mode %q\n", *render)
		os.Exit(2)
	}

	file, warnings, err := llm.LoadUserFile()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ajent:", err)
		os.Exit(1)
	}
	reg, regWarnings := llm.NewRegistry(file, llm.LoadUserCache(), llm.RegistryOptions{})
	warnings = append(warnings, regWarnings...)

	active := reg.Active()
	if modelFlag != "" {
		m, err := reg.Resolve(modelFlag)
		if err == nil {
			reg.SetActive(m)
			active = m
		} else {
			warnings = append(warnings, "no model matches "+modelFlag)
		}
	}

	// a model with no reported window renders no context bar rather than one drawn
	// against a fabricated number; parts() already skips the bar when MaxTokens is 0.
	var label string
	if active.ID != "" {
		label = active.Key()
	}

	ui, err := tui.New(tui.Options{
		Mode:      mode,
		Model:     label,
		MaxTokens: active.ContextWindow,
		History:   history.Load(secretPrefix),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ajent:", err)
		os.Exit(1)
	}
	defer ui.Close()
	defer history.Save(ui.History(), secretPrefix)

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

	sess := driver(ui, reg, active, sessMode, resumeID, flag.Args())

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
func driver(ui *tui.UI, reg *llm.Registry, active llm.Model, sessMode resumeMode, resumeID string, args []string) *sessRec {
	providers := llm.NewProviders(reg)
	st := &agent.State{
		Model:     active,
		Reasoning: defaultReasoning(),
		Tokens:    tokens.New(active),
	}

	// phase 06: every turn is recorded into the workspace transcript so double-Esc
	// while idle can open the context-tree picker and rewind onto an earlier point.
	rec := newSession(ui, sessMode, resumeID)
	if rec == nil {
		ui.Notify("session recording disabled; Esc will not rewind", tui.LevelWarn)
	}

	sink := tuisink.New(ui)

	// phase 04: build the built-in tool registry and hand it to the loop so the
	// model can read, write, edit and run commands.
	toolsReg, terr := tools.Builtins(tools.Options{SessionID: cwdOrDot()})
	if terr != nil {
		ui.Notify("tools disabled: "+terr.Error(), tui.LevelWarn)
	}

	// the compactor is wired lazily so the agent options can close over it before
	// the *Agent it needs exists; it is assigned once, right after agent.New.
	var comp *compactor
	opts := agent.Options{
		Sink:  sink,
		Env:   agent.DetectEnvironment(),
		Tools: toolsReg,
		Provider: func(m llm.Model) (llm.Provider, error) {
			return providers.ProviderFor(m)
		},
		Compact: func(ctx context.Context, reason agent.CompactReason) (bool, error) {
			if comp == nil {
				return false, nil // recording is off; nothing to compact
			}
			return comp.run(ctx, reason, "")
		},
	}
	if rec != nil {
		opts.Sink = rec.rec.Sink(sink) // persist notices and fsync at turn end
		opts.OnMessage = rec.rec.Message
		rec.rebuild(ui, reg, st)
	}
	// a resumed session restores its enabled tool set; unknown names are ignored.
	if toolsReg != nil && len(st.Tools) > 0 {
		toolsReg.SetEnabled(st.Tools)
	}

	// feed the editor's in-progress text into accounting so the context bar grows
	// as you type or paste, then clears it once submitted (the buffer empties).
	editSink := opts.Sink // may be nil before a session is set up
	ui.SetOnEdit(func(text string) {
		t := st.Tokens
		if t == nil || editSink == nil {
			return
		}
		t.SetCompose(tokens.EstimateText(text, tokens.KindProse))
		editSink.Context(t.Context())
	})
	ag := agent.New(st, opts)
	if rec != nil {
		rec.bindRewind(ui, ag, reg)
		comp = &compactor{
			rec: rec, st: st, ag: ag, reg: reg, ui: ui,
			sink:        opts.Sink,
			providerFor: providers.ProviderFor,
		}
	}

	// the prompt is at rest until a turn starts; double-Esc rewinds from here.
	ui.SetIdle(true)

	if active.ID == "" {
		ui.Notify("no model configured; use /model to pick one", tui.LevelWarn)
	}

	quit := make(chan struct{})
	started := false

	// phase 05: the command registry, shell stager and @ expander own the single
	// dispatch path for submitted lines. Commands run inline; shell lines stage and
	// flush ahead of the next prompt; prompts expand @ refs and steer the agent.
	cmds := command.NewRegistry()
	stager := command.NewStager(toolsReg, sink)
	console := &uiConsole{
		ui: ui, reg: reg, st: st, tools: toolsReg, commands: cmds,
		started: &started, quit: quit,
	}
	if rec != nil {
		console.rec = rec.rec
		console.comp = comp
	}
	command.RegisterBuiltins(cmds, console)
	watchControls(ui, ag, stager, quit)
	expander := refs.NewExpander(toolsReg, sink, tools.PathPolicy{Cwd: cwdOrDot()})
	idx := refs.NewIndex(cwdOrDot(), tools.PathPolicy{Cwd: cwdOrDot()})
	ui.SetCompleter(command.NewCompleter(cmds, console, idx))

	pump := make(chan pumpLine, 16)
	go runPump(pump, ag, console, stager, expander, rec != nil, ui, &started)

	if len(args) > 0 {
		pump <- pumpLine{kind: command.KindPrompt, rest: strings.Join(args, " ")}
	}

	for {
		select {
		case msg, ok := <-ui.Messages():
			if !ok {
				close(pump)
				return rec // UI closed
			}
			line := command.ParseLine(msg)
			switch line.Kind {
			case command.KindShell:
				stager.Run(line.Rest)
			case command.KindCommand:
				pump <- pumpLine{kind: command.KindCommand, rest: line.Rest}
			default:
				pump <- pumpLine{kind: command.KindPrompt, rest: line.Rest}
			}
		case <-quit:
			close(pump)
			return rec
		}
	}
}

// pumpLine is one classified submitted line handed to the prompt pump.
type pumpLine struct {
	kind command.Kind
	rest string
}

// runPump owns ordering for commands and prompts. Commands run inline (pickers
// block only the pump); prompts flush staged shell results, expand @ refs and
// steer or start a turn. Submissions stay in order and the UI never stalls.
func runPump(pump <-chan pumpLine, ag *agent.Agent, console *uiConsole, stager *command.Stager, expander *refs.Expander, recording bool, ui *tui.UI, started *bool) {
	for line := range pump {
		switch line.kind {
		case command.KindCommand:
			name, arg, ok := command.SplitCommand(line.rest)
			if !ok {
				console.Notify("unknown command /"+line.rest, tui.LevelWarn)
				continue
			}
			cmd, ok := console.commands.Get(name)
			if !ok {
				console.Notify("unknown command /"+name, tui.LevelWarn)
				continue
			}
			_ = cmd.Handler(context.Background(), arg, console)
		case command.KindPrompt:
			if strings.TrimSpace(line.rest) == "" {
				continue
			}
			// flush staged shell results ahead of the message, waiting for any
			// in-flight command to finish first
			before := stager.Flush(context.Background())
			res := expander.Expand(context.Background(), line.rest)
			for _, n := range res.Notices {
				console.Notify(n, tui.LevelWarn)
			}
			input := agent.Input{Text: res.Text, Before: append(before, res.Before...)}
			startPrompt(ui, recording, ag, input, started)
		}
	}
}

// startPrompt echoes a submitted message and runs it to completion in the
// background so the pump keeps receiving input while the model streams.
// recording reports whether sessions are on, which is what lets double-Esc rewind;
// idle flips false for the turn's duration so Esc interrupts instead of rewinding.
func startPrompt(ui *tui.UI, recording bool, ag *agent.Agent, input agent.Input, started *bool) {
	ui.SetIdle(false)
	*started = true
	go func() {
		_ = ag.Prompt(context.Background(), input)
		if recording {
			ui.SetIdle(true)
		}
	}()
}

// defaultReasoning enables thinking for capable models: a moderate level that
// streams to the UI, with whole-turn retention so multi-step turns stay valid.
// Providers without reasoning support ignore it entirely. /reasoning overrides it.
func defaultReasoning() llm.ReasoningConfig {
	return llm.ReasoningConfig{
		Level:  llm.LevelMedium,
		Retain: llm.RetainWholeTurn,
		Show:   true,
	}
}

// sessRec owns the open transcript: the store it lives in, the writer appending
// to it, and its path. bindRewind attaches the double-Esc handler that reads the
// branch back off disk and offers a picker over prior messages.
type sessRec struct {
	store *session.Store
	w     *session.Writer
	rec   *session.Recorder
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
func newSession(ui *tui.UI, mode resumeMode, resumeID string) *sessRec {
	store, err := session.NewStore()
	if err != nil {
		return nil
	}
	cwd := cwdOrDot()
	var pick func([]session.Info) (int, error)
	if ui != nil && mode == modeResumePick {
		pick = func(list []session.Info) (int, error) { return pickSessionRoot(ui, list) }
	}
	w, err := openSession(store, mode, cwd, resumeID, pick)
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
func openSession(store *session.Store, mode resumeMode, cwd string, id string, pick func([]session.Info) (int, error)) (*session.Writer, error) {
	fresh := func() (*session.Writer, error) {
		return store.Create(cwd, session.SessionData{
			Version:   session.Version(),
			Workspace: cwd,
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

// rebuild reconstructs the agent state from the resumed branch and replays it
// onto the UI so a reopened session shows its history. It is best-effort: any
// problem falls back to a fresh empty context.
func (r *sessRec) rebuild(ui *tui.UI, reg *llm.Registry, st *agent.State) {
	entries, _, err := session.Read(r.w.Path())
	if err != nil || len(entries) == 0 {
		return
	}
	rebuilt, warns := r.stateFor(modelResolver(reg), entries, session.Head(entries))
	for _, wmsg := range warns {
		ui.Notify("resume: "+wmsg, tui.LevelWarn)
	}
	if len(rebuilt.Messages) == 0 && rebuilt.Model.ID == "" {
		return // a brand-new transcript carries no history yet
	}
	st.Messages = rebuilt.Messages
	if rebuilt.Model.ID != "" {
		st.Model = rebuilt.Model
	}
	if rebuilt.Tokens != nil {
		st.Tokens = rebuilt.Tokens // a resumed ledger reflects the branch's recorded usage
	}
	session.Replay(session.Branch(entries, session.Head(entries)), tuisink.New(ui), session.ReplayOptions{})
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

// modelResolver adapts the registry's Resolve to a plain key resolver.
func modelResolver(reg *llm.Registry) func(string) (llm.Model, error) {
	return reg.Resolve
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
	head := session.Head(entries)
	tree := session.TreeRows(entries, head)
	if len(tree) == 0 {
		ui.Notify("nothing to rewind onto yet", tui.LevelInfo)
		return
	}

	items := make([]tui.PickItem, len(tree))
	for i, row := range tree {
		lead := "  "
		if row.Active {
			lead = "* " // the chain currently in context
		}
		// Guide draws the branch ("├──", "└──", continuation bars); a flat trunk has none.
		items[i] = tui.PickItem{Label: lead + row.Guide + row.Label}
	}
	picked, err := ui.PickContext(context.Background(), "Rewind to", items,
		tui.PickOptions{Placeholder: "filter", Initial: len(tree) - 1})
	if err != nil {
		return // cancelled
	}

	newHead, fillText, ok := session.RewindTarget(entries, tree[picked].ID)
	if !ok || newHead == "" {
		ui.Notify("cannot rewind onto that entry", tui.LevelWarn)
		return
	}
	r.w.SetHead(newHead)

	rebuilt, warns := r.stateFor(modelResolver(reg), entries, newHead)
	for _, wmsg := range warns {
		ui.Notify("rewind: "+wmsg, tui.LevelWarn)
	}

	// mutate the live state in place so every holder (the console, this rewind
	// handler) sees the restored context; keep the current model and reasoning.
	ag.WithState(func(st *agent.State) {
		st.Messages = rebuilt.Messages
		if rebuilt.Model.ID != "" {
			st.Model = rebuilt.Model
		}
		st.Tokens = rebuilt.Tokens // ledger rebuilt for exactly this branch point
	})

	// redraw to just the restored context, then drop the picked text into the
	// prompt so it can be edited or re-sent as this branch's first message.
	ui.Reset()
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
// terminal.
func watchControls(ui *tui.UI, ag *agent.Agent, stager *command.Stager, quit chan struct{}) {
	go func() {
		var lastInt time.Time
		for c := range ui.Controls() {
			switch c {
			case tui.ControlEscape:
				if ag.Running() {
					ag.Interrupt()
				} else if stager.Pending() {
					stager.Cancel() // Esc cancels an in-flight staged shell command
				}
			case tui.ControlInterrupt:
				if ag.Running() {
					ag.Interrupt()
					continue
				}
				// a running `!` cancels on the first Ctrl+C instead of quitting
				if stager.Pending() {
					stager.Cancel()
					ui.SetStatusSegment("hint", "cancelled shell command")
					continue
				}
				now := time.Now()
				if now.Sub(lastInt) < doublePressWindow {
					close(quit)
					return
				}
				lastInt = now
				ui.SetStatusSegment("hint", "ctrl+c again to quit")
			case tui.ControlEOF:
				if ag.Running() {
					continue // ignored while a turn streams, per the key table
				}
				close(quit)
				return
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
