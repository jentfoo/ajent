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
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tui"
	tuisink "github.com/jentfoo/ajent/pkg/tui/sink"
)

// defaultMaxTokens is the context window shown when no model reports one.
const defaultMaxTokens = 200_000

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

	maxTokens := defaultMaxTokens
	var label string
	if active.ID != "" {
		label = active.Key()
		if active.ContextWindow > 0 {
			maxTokens = active.ContextWindow
		}
	}

	ui, err := tui.New(tui.Options{
		Mode:      mode,
		Model:     label,
		MaxTokens: maxTokens,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ajent:", err)
		os.Exit(1)
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
// fresh or resumes a saved transcript.
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
	}

	// phase 06: every turn is recorded into the workspace transcript so double-Esc
	// while idle can open the context-tree picker and rewind onto an earlier point.
	rec := newSession(ui, sessMode, resumeID)
	if rec == nil {
		ui.Notify("session recording disabled; Esc will not rewind", tui.LevelWarn)
	}

	sink := tuisink.New(ui)
	opts := agent.Options{
		Sink: sink,
		Env:  agent.DetectEnvironment(),
		Provider: func(m llm.Model) (llm.Provider, error) {
			return providers.ProviderFor(m)
		},
	}
	if rec != nil {
		opts.Sink = rec.rec.Sink(sink) // persist notices and fsync at turn end
		opts.OnMessage = rec.rec.Message
		rec.rebuild(ui, reg, st)
	}
	ag := agent.New(st, opts)
	if rec != nil {
		rec.bindRewind(ui, ag, reg, &st)
	}

	// the prompt is at rest until a turn starts; double-Esc rewinds from here.
	ui.SetIdle(true)

	if active.ID == "" {
		ui.Notify("no model configured; use /model to pick one", tui.LevelWarn)
	}

	quit := watchControls(ui, ag)

	var seed agent.Input
	if len(args) > 0 {
		seed = agent.Input{Text: strings.Join(args, " ")}
		startTurn(ui, rec != nil, ag, seed.Text)
	}

	for {
		select {
		case msg, ok := <-ui.Messages():
			if !ok {
				return rec // UI closed
			}
			if cmd, arg, ok := slashCommand(msg); ok {
				handleCommand(ui, reg, st, cmd, arg)
				continue
			}
			if ag.Steer(agent.Input{Text: msg}) {
				continue // queued into the running turn at its next boundary
			}
			startTurn(ui, rec != nil, ag, msg)
		case <-quit:
			return rec
		}
	}
}

// startTurn echoes a submitted message and runs it to completion in the
// background so the main loop keeps receiving input while the model streams.
// recording reports whether sessions are on, which is what lets double-Esc rewind;
// idle flips false for the turn's duration so Esc interrupts instead of rewinding.
func startTurn(ui *tui.UI, recording bool, ag *agent.Agent, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	ui.SetIdle(false)
	go func() { // the sink echoes the prompt on TurnStart and streams the reply
		_ = ag.Prompt(context.Background(), agent.Input{Text: text})
		if recording {
			ui.SetIdle(true) // back at rest; double-Esc can rewind again
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
		if perr == tui.ErrCancelled || picked < 0 {
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
		detail := in.Started.Local().Format("2006-01-02 15:04")
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
	session.Replay(session.Branch(entries, session.Head(entries)), tuisink.New(ui), session.ReplayOptions{})
}

// bindRewind wires the double-Esc gesture to a picker over this transcript's
// branch. Picking an entry rewinds the writer and agent onto that point.
func (r *sessRec) bindRewind(ui *tui.UI, ag *agent.Agent, reg *llm.Registry, st **agent.State) {
	if ui == nil || ag == nil {
		return
	}
	ui.SetOnRewind(func() { r.rewind(ui, ag, reg, st) })
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
func (r *sessRec) rewind(ui *tui.UI, ag *agent.Agent, reg *llm.Registry, st **agent.State) {
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

	newHead, fillText, ok := rewindToPrior(entries, tree[picked].ID)
	if !ok {
		ui.Notify("cannot rewind onto that entry", tui.LevelWarn)
		return
	}
	r.w.SetHead(newHead)

	rebuilt, warns := r.stateFor(modelResolver(reg), entries, newHead)
	for _, wmsg := range warns {
		ui.Notify("rewind: "+wmsg, tui.LevelWarn)
	}

	nst := &agent.State{
		Messages:  rebuilt.Messages,
		Model:     (**st).Model, // keep the live model; a fork may not carry one
		Reasoning: (**st).Reasoning,
		Tools:     (**st).Tools,
	}
	if rebuilt.Model.ID != "" {
		nst.Model = rebuilt.Model
	}

	// redraw to just the restored context, then drop the picked text into the
	// prompt so it can be edited or re-sent as this branch's first message.
	ui.Reset()
	if ag.ResetState(nst) {
		*st = nst // driver's handle now points at the same rebuilt state
	}
	session.Replay(session.Branch(entries, newHead), tuisink.New(ui), session.ReplayOptions{})
	if fillText != "" {
		ui.SetInput(fillText)
	}
}

// rewindToPrior maps selecting a message onto rewinding *before* it: the head is
// set to its parent so the picked text becomes the start of the new branch, and
// fillText carries the full original prompt for the editor. ok is false when the
// entry has no prior point to rewind onto.
func rewindToPrior(entries []session.Entry, rowID string) (newHead, fillText string, ok bool) {
	for i := range entries {
		e := entries[i]
		if e.ID != rowID || e.Type != session.TypeMessage {
			continue
		}
		if e.ParentID == "" {
			return "", "", false // nothing before this message to rewind onto
		}
		return e.ParentID, session.EntryMessageText(e), true
	}
	return "", "", false
}

// doublePressWindow is how long a second Ctrl+C on an idle editor must arrive
// within to quit, so a single stray interrupt never kills the app.
const doublePressWindow = 2 * time.Second

// watchControls interprets out-of-band keys: Esc and Ctrl+C interrupt a running
// turn, while Ctrl+D or a double Ctrl+C on an idle empty editor quits. Closing
// quit signals driver to return, which lets main's deferred ui.Close restore the
// terminal.
func watchControls(ui *tui.UI, ag *agent.Agent) <-chan struct{} {
	quit := make(chan struct{})
	go func() {
		var lastInt time.Time
		for c := range ui.Controls() {
			switch c {
			case tui.ControlEscape:
				if ag.Running() {
					ag.Interrupt()
				}
			case tui.ControlInterrupt:
				if ag.Running() {
					ag.Interrupt()
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
				close(quit) // emitted only on an empty editor: deliberate exit
				return
			}
		}
	}()
	return quit
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

// slashCommand splits a submitted line into a command and its argument,
// reporting whether it was one at all.
func slashCommand(msg string) (cmd, arg string, ok bool) {
	trimmed := strings.TrimSpace(msg)
	if !strings.HasPrefix(trimmed, "/") {
		return "", "", false
	}
	cmd, arg, _ = strings.Cut(trimmed[1:], " ")
	return strings.ToLower(cmd), strings.TrimSpace(arg), cmd != ""
}

// handleCommand runs a slash command. The registry stays the single source of
// truth for the active model; st.Model follows it so later turns use the new one.
func handleCommand(ui *tui.UI, reg *llm.Registry, st *agent.State, cmd, arg string) {
	switch cmd {
	case "model":
		selectModel(ui, reg, st, arg)
	case "reasoning":
		reasoningCommand(ui, st, arg)
	default:
		ui.Notify("unknown command /"+cmd, tui.LevelWarn)
	}
}

// reasoningCommand sets the session's reasoning level (and whether thinking is
// streamed). With no argument it reports the current choice; with an empty target
// after a picker it leaves things unchanged.
func reasoningCommand(ui *tui.UI, st *agent.State, arg string) {
	if strings.TrimSpace(arg) == "" {
		items := make([]tui.PickItem, len(llm.Levels()))
		for i, lvl := range llm.Levels() {
			name := lvl.String()
			marker := "  "
			if st.Reasoning.Level == lvl {
				marker = "* "
			}
			items[i] = tui.PickItem{Label: marker + name, Terms: []string{name}}
		}
		picked, err := ui.PickContext(context.Background(), "Reasoning", items,
			tui.PickOptions{Placeholder: "filter"})
		if err != nil {
			return // cancelled
		}
		arg = llm.Levels()[picked].String()
	}
	lvl, ok := llm.ParseLevel(arg)
	if !ok {
		ui.Notify("unknown reasoning level "+arg+"; use off|minimal|low|medium|high|xhigh|max", tui.LevelWarn)
		return
	}
	st.Reasoning = llm.ReasoningConfig{
		Level:  lvl,
		Retain: st.Reasoning.Retain, // keep the current retention policy
		Show:   true,
	}
	ui.Notify("reasoning: "+lvl.String(), tui.LevelInfo)
}

// selectModel resolves arg, or opens the picker when it is empty.
func selectModel(ui *tui.UI, reg *llm.Registry, st *agent.State, arg string) {
	models := reg.Models()
	if len(models) == 0 {
		ui.Notify("no models configured; add some to ~/.ajent/"+llm.ModelsFileName, tui.LevelWarn)
		return
	}
	var m llm.Model
	if arg != "" {
		target, err := reg.Resolve(arg)
		if err != nil {
			reportResolveError(ui, arg, err)
			return
		}
		m = target
	} else {
		items := make([]tui.PickItem, len(models))
		activeKey := reg.Active().Key()
		var initial int
		for i, mod := range models {
			if mod.Key() == activeKey {
				initial = i
			}
			items[i] = tui.PickItem{
				Label:  mod.Key(),
				Detail: modelDetail(mod),
				Terms:  append([]string{mod.Name}, mod.Aliases...),
			}
		}
		picked, err := ui.PickContext(context.Background(), "Model", items,
			tui.PickOptions{Placeholder: "filter", Initial: initial})
		if err != nil {
			return // cancelled, nothing to report
		}
		m = models[picked]
	}
	applyModel(ui, reg, st, m)
}

// modelDetail is the dim trailing text on a picker row.
func modelDetail(m llm.Model) string {
	parts := []string{m.Display()}
	if m.ContextWindow > 0 {
		parts = append(parts, formatTokens(m.ContextWindow))
	}
	if m.Caps.Reasoning != llm.ReasoningNone {
		parts = append(parts, "reasoning")
	}
	return strings.Join(parts, " · ")
}

// applyModel makes m active and reflects it in the status line and the agent's
// state so subsequent turns use it.
func applyModel(ui *tui.UI, reg *llm.Registry, st *agent.State, m llm.Model) {
	reg.SetActive(m)
	st.Model = m
	ui.SetModel(m.Key(), m.ContextWindow)
	ui.Notify("model: "+m.Key(), tui.LevelInfo)
}

// reportResolveError explains why a name did not select a model.
func reportResolveError(ui *tui.UI, arg string, err error) {
	var ambiguous *llm.ErrAmbiguousModel
	if errors.As(err, &ambiguous) {
		ui.Notify(arg+" matches "+strings.Join(ambiguous.Candidates, ", "), tui.LevelWarn)
		return
	}
	ui.Notify("no model matches "+arg, tui.LevelWarn)
}

// formatTokens abbreviates a context window, such as 200k or 1.2M.
func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return strings.TrimSuffix(strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64), ".0") + "M"
	case n >= 1_000:
		return strings.TrimSuffix(strconv.FormatFloat(float64(n)/1_000, 'f', 1, 64), ".0") + "k"
	default:
		return strconv.Itoa(n)
	}
}
