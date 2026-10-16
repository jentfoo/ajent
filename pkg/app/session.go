package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
	tuisink "github.com/jentfoo/ajent/pkg/tui/sink"
)

// snapshot is a transcript's open-time parse: the entries head recovery read,
// Read's per-line warnings, and the size/mtime stamped before that read so a
// rebuild can detect concurrent appends.
type snapshot struct {
	entries []session.Entry
	warns   []string // surfaced as resume notices when the rebuild consumes them
	size    int64    // transcript size at stat time; unchanged means still fresh
	mod     time.Time
}

type sessRec struct {
	store *session.Store
	w     *session.Writer
	rec   *session.Recorder
	// note is a startup caveat from session selection (--continue skipping
	// newer-format sessions), surfaced once by whichever front end opened it.
	note string
	// snapshot is the transcript's open-time parse, shared by the startup hint
	// and resume rebuild. Consumed by restoreState: every later path (rewind,
	// compaction, exit) re-reads live state.
	snapshot snapshot
	// hint is the transcript's root id, cached on first read and stable for the
	// process lifetime.
	hint string
	// onSwitch reports a rebuilt context (rewind, fork or compaction) so read tracking
	// and @ reference ids stop describing what it replaced. Nil until main wires it.
	onSwitch func([]llm.Message)
	// started is the driver's tool-block-committed flag, shared so a rewind seeds
	// its base with the same rule the pump uses. Nil until main wires it.
	started *startedFlag
	// discardStaged drops shell results staged against the branch a rewind leaves,
	// so they never ride the new branch's first prompt. Nil until main wires it.
	discardStaged func()
	// resumeAuto re-arms automatic compaction: a shorter branch may reduce where
	// the one it left could not. Nil until main wires it.
	resumeAuto func()
}

func sessionHint(r *sessRec) string {
	if r == nil || r.w == nil {
		return ""
	}
	if r.hint == "" { // a fresh session caches on its first read, never re-reading after
		if entries, ok := sessionEntries(r); ok {
			r.hint = rootID(entries) // the root id never changes once minted
		}
	}
	return r.hint
}

func sessionLabel(r *sessRec) string {
	entries, ok := sessionEntries(r)
	if !ok {
		return ""
	}
	return labelOf(entries)
}

func (r *sessRec) name() string {
	entries, ok := sessionEntries(r)
	if !ok {
		return ""
	}
	return session.NameOf(entries)
}

func sessionEntries(r *sessRec) ([]session.Entry, bool) {
	if r == nil || r.w == nil {
		return nil, false
	}
	entries, _, err := session.Read(r.w.Path())
	if err != nil {
		return nil, false
	}
	return entries, true
}

func rootID(entries []session.Entry) string {
	i := slices.IndexFunc(entries, func(e session.Entry) bool { return e.Type == session.TypeSession })
	if i < 0 {
		return ""
	}
	return entries[i].ID
}

// abandoned reports whether entries describe a transcript worth dropping at
// exit: unnamed and holding no message.
func abandoned(entries []session.Entry) bool {
	if session.NameOf(entries) != "" {
		return false // --session resumes by name, so a named session is worth keeping
	}
	return !slices.ContainsFunc(entries, func(e session.Entry) bool { return e.Type == session.TypeMessage })
}

// exitLabel ends the run's recording: an abandoned session (unnamed, nothing
// recorded) is removed with its head cursor so it never becomes a dead picker
// row or steals "most recent", reporting "" so no resume hint names it. Any
// other transcript returns the label a `--resume` command takes.
func (r *sessRec) exitLabel() string {
	if r == nil || r.w == nil {
		return ""
	}
	entries, ok := sessionEntries(r)
	if !ok {
		return "" // unreadable: neither dropped nor advertised
	}
	if r.store != nil && abandoned(entries) {
		_ = r.store.Remove(r.w.Path())
		return ""
	}
	return labelOf(entries)
}

// labelOf names a transcript the way --resume takes it back: its recorded
// name, else the root entry's id.
func labelOf(entries []session.Entry) string {
	if name := session.NameOf(entries); name != "" {
		return name
	}
	return rootID(entries)
}

// resumeHint renders the exact line every finished run points home with, so
// interactive and one-shot exits cannot drift apart.
func resumeHint(label string) string {
	return fmt.Sprintf("\nRun `ajent --resume %s` to resume this session.\n", label)
}

// refuseNewer explains why a saved session cannot open here: this build would
// append to a transcript it cannot read. Naming the format (never the binary
// version) keeps the gate about storage, and the hint sends the user to the fix.
func refuseNewer(info session.Info) error {
	return fmt.Errorf("session %s uses a newer session format; update ajent to resume it", info.Label())
}

// skipNewerNote describes the newer-format sessions --continue skipped to reach
// what it resumed (or fresh), so the fallback reads as explained rather than
// vanishing history.
func skipNewerNote(skipped int, newest session.Info) string {
	if skipped == 1 {
		return fmt.Sprintf("skipping %s: newer session format", newest.Label())
	}
	return fmt.Sprintf("skipping %d sessions (newest %s): newer session format", skipped, newest.Label())
}

// CheckSessionTarget verifies that a requested session exists before the TUI
// opens, so an id or name fails fast rather than silently starting fresh. It
// returns the resolution for the open to adopt as a locator; a zero Info
// resolves nothing. A target blocked behind a newer format refuses with the
// version hint; only a true miss reads as "no session matches", while
// ambiguity and listing failures pass through verbatim.
func CheckSessionTarget(mode ResumeMode, target string) (session.Info, error) {
	switch mode {
	case ResumeID, ResumeSessionName:
	default:
		return session.Info{}, nil // nothing requested: implicit modes need no check
	}
	store, err := session.NewStore()
	if err != nil {
		return session.Info{}, err
	}
	cwd := config.Cwd()
	if mode == ResumeSessionName {
		info, found, ferr := store.FindNamed(cwd, target)
		if ferr != nil || !found {
			return session.Info{}, ferr
		} else if info.Blocked {
			return session.Info{}, refuseNewer(info)
		}
		return info, nil
	}
	info, ferr := store.Find(cwd, target)
	if errors.Is(ferr, session.ErrNotFound) {
		return session.Info{}, fmt.Errorf("no session matches %q", target)
	} else if ferr != nil { // ambiguity and listing failures pass through verbatim, as --delete does
		return session.Info{}, ferr
	} else if info.Blocked {
		return session.Info{}, refuseNewer(info)
	}
	return info, nil
}

// newSession opens the run's transcript. pre is the check-time resolution of
// target, adopted as a locator when it still fits the live store. A returned
// error is an open failure the caller must surface, never a fall-back-to-fresh
// signal; implicit modes that legitimately start fresh return a writer with a
// nil error.
func newSession(ui *tui.UI, mode ResumeMode, target, modelKey string, pre session.Info) (*sessRec, error) {
	store, err := session.NewStore()
	if err != nil {
		return nil, err
	}
	cwd := config.Cwd()
	var pick func([]session.Info) (int, error)
	if ui != nil && mode == ResumePick {
		pick = func(list []session.Info) (int, error) { return pickSessionRoot(ui, list) }
	}
	w, snap, note, err := openSession(store, mode, cwd, target, modelKey, pick, pre)
	if err != nil {
		return nil, err
	}
	r := &sessRec{store: store, w: w, rec: session.NewRecorder(w), note: note, snapshot: snap}
	r.hint = rootID(snap.entries)
	return r, nil
}

// openSession resolves mode onto a transcript writer, its open-time snapshot
// (empty for a fresh session) and a startup caveat note. pre is the check-time
// resolution of target for the id and name modes, adopted as a locator when it
// still fits the live store so the workspace is scanned once. A returned error
// is an open failure the caller must surface, never a fall-back-to-fresh
// signal; implicit modes that legitimately start fresh return a writer with a
// nil error. note carries a caveat about what was chosen (a --continue that had
// to skip newer-format sessions) for the front end to announce.
func openSession(store *session.Store, mode ResumeMode, cwd string, target, modelKey string, pick func([]session.Info) (int, error), pre session.Info) (*session.Writer, snapshot, string, error) {
	fresh := func(name string) (*session.Writer, snapshot, string, error) {
		sd := session.SessionData{Version: session.Version(), Workspace: cwd, Name: name}
		if modelKey != "" {
			sd.Model = modelKey // provenance so a resume can stamp assistant origins
		}
		w, err := store.Create(cwd, sd)
		return w, snapshot{}, "", err
	}

	switch mode {
	case ResumeContinue:
		list, lerr := store.List(cwd)
		if lerr != nil {
			return nil, snapshot{}, "", lerr
		} else if len(list) == 0 {
			return fresh("") // nothing saved yet, start one
		}
		// resume the newest session this build can open: a newer-format one is
		// skipped loudly instead of quietly misrouting work into an older one.
		i := slices.IndexFunc(list, func(in session.Info) bool { return !in.Blocked })
		if i < 0 { // every save carries a newer format; still start one
			w, _, _, ferr := fresh("")
			return w, snapshot{}, skipNewerNote(len(list), list[0]), ferr
		}
		w, snap, err := openSnapshot(list[i].Path)
		var note string
		if err == nil && i > 0 {
			note = skipNewerNote(i, list[0]) // rows sorting above the resumed one are all blocked
		}
		return w, snap, note, err
	case ResumeID:
		info, ferr := preTarget(store, cwd, target, pre)
		if ferr != nil {
			return nil, snapshot{}, "", ferr
		} else if info.Blocked { // refused here so Open never touches a newer transcript
			return nil, snapshot{}, "", refuseNewer(info)
		}
		w, snap, err := openSnapshot(info.Path) // resume that exact transcript's leaf
		return w, snap, "", err
	case ResumeSessionName:
		var info session.Info
		var found bool
		var ferr error
		if preAdoptable(store, cwd, pre) {
			info, found = pre, true // the check found it; content still parses fresh
		} else {
			info, found, ferr = store.FindNamed(cwd, target)
		}
		if ferr != nil {
			return nil, snapshot{}, "", ferr
		} else if !found {
			return fresh(target) // first run under this name
		} else if info.Blocked { // the held name is refused, never re-minted as a fresh transcript
			return nil, snapshot{}, "", refuseNewer(info)
		}
		w, snap, err := openSnapshot(info.Path)
		return w, snap, "", err
	case ResumePick:
		list, lerr := store.List(cwd)
		if lerr != nil && !errors.Is(lerr, session.ErrNoSessions) {
			return nil, snapshot{}, "", lerr // a listing failure must not read as nothing to resume
		} else if len(list) == 0 {
			return fresh("") // nothing saved yet, start one
		}
		if pick == nil { // no way to choose: refuse rather than silently start fresh
			return nil, snapshot{}, "", errors.New("bare --resume needs a terminal for the session picker; pass an id or name")
		}
		picked, perr := pick(list)
		if errors.Is(perr, tui.ErrCancelled) || picked < 0 {
			return fresh("") // cancelled the resume, start new rather than stall
		} else if perr != nil {
			return nil, snapshot{}, "", perr
		}
		// the picker only shades a blocked row (Disabled is not enforced), so the
		// invariant that Open never sees one holds at this boundary too
		if list[picked].Blocked {
			return nil, snapshot{}, "", refuseNewer(list[picked])
		}
		w, snap, err := openSnapshot(list[picked].Path) // resume that root's leaf
		return w, snap, "", err
	default: // ResumeNewSession and anything unexpected: always fresh
		return fresh("")
	}
}

// openSnapshot opens a transcript for append and returns its open-time parse
// with the size and mtime stamped before the read, so a later rebuild can
// detect concurrent appends.
func openSnapshot(path string) (*session.Writer, snapshot, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, snapshot{}, err
	}
	w, entries, warns, oerr := session.OpenEntries(path)
	if oerr != nil {
		return nil, snapshot{}, oerr
	}
	return w, snapshot{entries: entries, warns: warns, size: fi.Size(), mod: fi.ModTime()}, nil
}

// preTarget resolves the resume target, adopting pre's path when it still
// belongs to the live store so the workspace parses once.
func preTarget(store *session.Store, cwd, target string, pre session.Info) (session.Info, error) {
	if preAdoptable(store, cwd, pre) {
		return pre, nil
	}
	return store.Find(cwd, target)
}

// preAdoptable reports whether pre may serve as the open's locator: resolved,
// unblocked, and inside the workspace's live session directory (the demo build
// redirects the config home after the check, so a foreign path is dropped).
func preAdoptable(store *session.Store, cwd string, pre session.Info) bool {
	if pre.Path == "" || pre.Blocked {
		return false
	}
	dir, err := store.Dir(cwd)
	if err != nil {
		return false
	}
	return strings.HasPrefix(pre.Path, dir+string(os.PathSeparator))
}

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

func searchItems(prompts []session.Prompt) []tui.SearchItem {
	out := make([]tui.SearchItem, 0, len(prompts))
	for _, p := range prompts {
		var detail string
		if !p.At.IsZero() { // typed-only lines have no transcript timestamp
			detail = p.At.Local().Format("2006-01-02 15:04") // recorded in UTC, rendered in the system local time zone
		}
		out = append(out, tui.SearchItem{Text: p.Text, Detail: detail})
	}
	return out
}

func rewindBody(row session.TreeRow) string {
	var prefix string
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

func pickSessionRoot(ui *tui.UI, list []session.Info) (int, error) {
	items := make([]tui.PickItem, len(list))
	for i, in := range list {
		label := in.First
		if label == "" {
			label = in.Name // a named session with no prompt yet still identifies itself
		}
		if label == "" {
			label = "(empty session)"
		}
		detail := in.Updated.Local().Format("2006-01-02 15:04") // recorded in UTC, rendered in the system local time zone
		if in.Model != "" {
			detail += " · " + in.Model
		}
		if in.Messages > 0 {
			detail += fmt.Sprintf(" · %d msgs", in.Messages)
		}
		// the name renders as the tag and both it and the id stay filterable
		items[i] = tui.PickItem{Label: label, Detail: detail, Tag: in.Name, Terms: []string{in.ID}}
		if in.Blocked {
			// present but unopenable: the row says where the conversation went
			items[i].Label = "newer session format"
			items[i].Disabled = true
		}
	}
	return ui.PickContext(context.Background(), "Resume session", items,
		tui.PickOptions{Placeholder: "filter"})
}

// restoreState rebuilds resumed state into st from the transcript's branch,
// seeding set with recorded session overrides. It returns entries and head for
// replay plus resume warnings; a failed read warns through those rather than
// resuming as an unannounced empty conversation.
func (r *sessRec) restoreState(set *config.Set, reg *llm.Registry, st *agent.State, toolsReg *tools.Registry) ([]session.Entry, string, []string) {
	var entries []session.Entry
	var rwarns []string
	snap := r.snapshot
	r.snapshot = snapshot{} // consumed: later rebuilds re-read live state
	if snap.entries != nil && snapshotFresh(r.w.Path(), snap) {
		entries, rwarns = snap.entries, snap.warns // the open-time parse: startup reads once
	} else {
		var err error
		entries, rwarns, err = session.Read(r.w.Path())
		if err != nil {
			return nil, "", append(rwarns, "transcript unreadable: "+err.Error())
		}
	}
	if len(entries) == 0 {
		return nil, "", nil
	}
	head := resumeHead(r.w.Head(), entries)
	// stamp the threshold before state resolution so the trigger and context bar
	// read one number after resume. permissions.mode is dropped (even when an older
	// session recorded it) so a restart always starts from the configured default.
	overrides := session.SettingOverrides(session.Branch(entries, head))
	delete(overrides, "permissions.mode")
	set.SeedSession(overrides)
	applyImagesBlock(set) // a session saved with images blocked resumes blocked
	resumed := set.Settings()
	reg.SetCompactDefault(resumed.Compaction.Threshold) // models declaring none pick up the session default

	rebuilt, warns := r.stateFor(modelResolver(reg), entries, head)
	if len(rebuilt.Messages) == 0 && rebuilt.Model.ID == "" {
		return nil, "", append(rwarns, warns...) // a brand-new transcript carries no history yet
	}
	st.Messages = rebuilt.Messages
	if rebuilt.Model.ID != "" {
		st.Model = rebuilt.Model
	}
	if _, ok := llm.ParseLevel(resumed.Reasoning.Level); ok || resumed.Reasoning.Retain != "" {
		st.Reasoning = llm.ReasoningFrom(config.Reasoning{
			Level:  resumed.Reasoning.Level,
			Retain: resumed.Reasoning.Retain,
			Hide:   resumed.Reasoning.Hide,
			Budget: resumed.Reasoning.Budget,
		}, st.Model)
	}
	if toolsReg != nil && len(resumed.Tools.Enabled) > 0 {
		toolsReg.SetEnabled(resumed.Tools.Enabled)
	}
	st.Tokens = rebuilt.Tokens // a resumed ledger reflects the branch's recorded usage
	return entries, head, append(rwarns, warns...)
}

// snapshotFresh reports whether the transcript still matches the snapshot's
// stamped size and mtime, so another process's appends force a live re-read.
func snapshotFresh(path string, snap snapshot) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Size() == snap.size && fi.ModTime().Equal(snap.mod)
}

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
	session.Replay(session.Branch(entries, head), replaySink(ui), session.ReplayOptions{})
}

func (r *sessRec) bindRewind(ui *tui.UI, ag *agent.Agent, reg *llm.Registry) {
	if ui == nil || ag == nil {
		return
	}
	ui.SetOnRewind(func() { r.rewind(ui, ag, reg) })
}

func (r *sessRec) stateFor(resolve func(string) (llm.Model, error), entries []session.Entry, head string) (agent.State, []string) {
	return session.State(session.Branch(entries, head), resolve)
}

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
	if r.resumeAuto != nil {
		r.resumeAuto()
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
	if rebuilt.Model.ID == "" && live.ID != "" {
		// the branch named no model of its own: frame what the rebuild measured
		// against the live one rather than leaving a zero window, which would
		// rescale the bar off the compaction threshold onto the raw context size
		ledger.SetWindow(live)
	}
	ledger.SetBase(r.baseEstimate(ag))
	pushSwitchedContext(ui, ledger)
	return nil
}

func (r *sessRec) baseEstimate(ag *agent.Agent) int {
	var committed bool
	if r.started != nil {
		committed = r.started.marked()
	}
	return ag.BaseEstimate(committed)
}

func pushSwitchedContext(ui *tui.UI, t *tokens.Accounting) {
	if ui == nil {
		return
	}
	cs := t.Context()
	ui.SetContext(tui.ContextInfo{
		Used: cs.Used, Window: cs.Window, Reserve: cs.Reserve,
		Compact: cs.Compact, Estimated: cs.Estimated,
	})
}

func (r *sessRec) liveModel(ag *agent.Agent) llm.Model {
	if ag == nil {
		return llm.Model{}
	}
	save := llm.Model{}
	ag.WithState(func(st *agent.State) { save = st.Model })
	return save
}

func syncModelUI(ui *tui.UI, reg *llm.Registry, m llm.Model) {
	reg.SetActive(m)
	if ui == nil {
		return
	}
	ui.SetModel(m.Key(), m.ShortName(), m.ContextWindow)
}

// replaySink builds the sink a rewind or resume replay drives: tool images
// render as text, since the drawings already sit in scrollback above.
func replaySink(ui *tui.UI) agent.Sink {
	s := tuisink.New(ui)
	s.SetImages(false)
	return s
}

func (r *sessRec) restoreForkModel(ui *tui.UI, ag *agent.Agent, reg *llm.Registry, m llm.Model) {
	if m.ID == "" || ag == nil {
		return
	}
	var ledger *tokens.Accounting
	ag.WithState(func(st *agent.State) {
		st.Model = m
		st.Tokens.SetModel(m)
		st.Tokens.Reseed(tokens.EstimateFor(m, st.Reasoning.Retain, st.Messages))
		ledger = st.Tokens
	})
	// this deliberately overwrites what switchState seeded, so the base is measured
	// again here against the fork's model, BaseEstimate takes the lock WithState held
	ledger.SetBase(r.baseEstimate(ag))
	pushSwitchedContext(ui, ledger)
	syncModelUI(ui, reg, m)
}

func resumeHead(live string, entries []session.Entry) string {
	if live != "" && slices.ContainsFunc(entries, func(e session.Entry) bool { return e.ID == live }) {
		return live
	}
	return session.Head(entries)
}

func modelResolver(reg *llm.Registry) func(string) (llm.Model, error) {
	return reg.Resolve
}

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

const rewindDeferThreshold = 300

func (r *sessRec) rewind(ui *tui.UI, ag *agent.Agent, reg *llm.Registry) {
	// a failed re-read is not the same as nothing recorded yet
	entries, _, err := session.Read(r.w.Path())
	if err != nil {
		ui.Notify("rewind: "+err.Error(), tui.LevelWarn)
		return
	}
	if len(entries) == 0 {
		ui.Notify("nothing to rewind onto yet", tui.LevelInfo)
		return
	}
	head := resumeHead(r.w.Head(), entries)
	tree := session.TreeRows(entries, head)
	if len(tree) == 0 {
		ui.Notify("nothing to rewind onto yet", tui.LevelInfo)
		return
	}

	// one payload per row, verbatim from the transcript so a ctrl+x copy is
	// truncation-independent of the labels the picker renders
	rowIDs := make([]string, len(tree))
	for i, row := range tree {
		rowIDs[i] = row.ID
	}
	copyTexts := session.CopyTexts(entries, rowIDs)

	items := make([]tui.PickItem, len(tree))
	for i, row := range tree {
		tag, mark := roleTag(row.Kind)
		// Guide draws the branch ("├──", "└──", continuation bars), but a flat trunk has none.
		// Off shades the rows no longer in context, so an abandoned fork recedes.
		items[i] = tui.PickItem{
			Label: row.Guide + rewindBody(row),
			Tag:   tag,
			Mark:  mark,
			Off:   !row.Active,
			Copy:  copyTexts[i],
		}
	}
	// a large session would repaint every retained line on each arrow press in alt
	// mode, defer that until the message is chosen.
	if len(tree) >= rewindDeferThreshold {
		ui.SetDeferHistory(true)
		defer ui.SetDeferHistory(false)
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
	if r.discardStaged != nil {
		r.discardStaged() // staged against the branch just left, not this one's to carry
	}
	r.restoreForkModel(ui, ag, reg, saveModel)

	// redraw to just the restored context, then drop the picked text into the
	// prompt so it can be edited or re-sent as this branch's first message.
	ui.Reset()
	// mark where restored history begins so it reads clearly in scrollback
	ui.Divider()
	session.Replay(session.Branch(entries, newHead), replaySink(ui), session.ReplayOptions{})
	if fillText != "" {
		ui.SetInput(fillText)
	}
}
