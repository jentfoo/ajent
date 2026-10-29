package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/go-analyze/bulk"

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
	// onAppend observes each successfully appended transcript entry, for the
	// control server's entry.append events. Nil drops.
	onAppend func(session.Entry)
	// starts is the driver's dispatch state, shared so a rewind seeds its base
	// with the same committed rule the pump uses. Nil until main wires it.
	starts *startState
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

// exitLabel ends the run's recording, reporting "" so no resume hint names a
// dropped transcript. Any other transcript returns the label a `--resume`
// command takes.
func (r *sessRec) exitLabel() string {
	if r == nil || r.w == nil {
		return ""
	}
	entries, ok := sessionEntries(r)
	if !ok {
		return "" // unreadable: neither dropped nor advertised
	}
	if r.store != nil && session.NameOf(entries) == "" &&
		!slices.ContainsFunc(entries, func(e session.Entry) bool { return e.Type == session.TypeMessage }) {
		// unnamed and holding no message: removed with its head cursor so it never
		// becomes a dead picker row or steals "most recent"
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

// refuseDamaged explains why a path-resolved transcript with entries but no
// decodable identity cannot open: the rebuild has nothing to classify, so
// deliberate removal is its way out.
func refuseDamaged(info session.Info) error {
	return fmt.Errorf("transcript %s holds entries this build cannot identify as a session; delete it with --delete %s",
		info.Path, info.Path)
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

// stderrNote carries a store warning on stderr; the CLI paths printing it run
// before any front end exists.
func stderrNote(w string) {
	if w == "" {
		return
	}
	_, _ = fmt.Fprintln(os.Stderr, "ajent: "+w)
}

// CheckSessionTarget verifies that a requested session exists before the TUI
// opens, so an id or name fails fast rather than silently starting fresh. It
// returns the resolution for the open to adopt as a locator; a zero Info
// resolves nothing. A target blocked behind a newer format refuses with the
// version hint and a damaged one with its removal hint; Find's own wording (a
// miss names any damaged transcripts beside it) passes through verbatim.
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
	// a pre-flight lookup runs before any front end exists, so its pin warning
	// surfaces on stderr directly; the open-time store sees a clean mode after.
	stderrNote(store.Note())
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
	if ferr != nil { // ambiguity, listing failures and the miss wording all pass through verbatim
		return session.Info{}, ferr
	} else if info.Blocked {
		return session.Info{}, refuseNewer(info)
	} else if info.Damaged {
		return session.Info{}, refuseDamaged(info)
	}
	return info, nil
}

// newSession opens the run's transcript. pre is the check-time resolution of
// target, adopted as a locator when it still fits the live store. A returned
// error is an open failure the caller must surface, never a fall-back-to-fresh
// signal; implicit modes that legitimately start fresh return a writer with a
// nil error.
func newSession(ui *tui.UI, dialogs *interactionBroker, mode ResumeMode, target, modelKey string, pre session.Info) (*sessRec, error) {
	store, err := session.NewStore()
	if err != nil {
		return nil, err
	}
	cwd := config.Cwd()
	var pick func([]session.Info) (int, error)
	if ui != nil && mode == ResumePick {
		pick = func(list []session.Info) (int, error) { return pickSessionRoot(dialogs, list) }
	}
	w, snap, note, err := openSession(store, mode, cwd, target, modelKey, pick, pre)
	if err != nil {
		return nil, err
	}
	// startup caveats join into the one line sessRec.note carries
	note = strings.Join(bulk.SliceFilter(func(s string) bool { return s != "" },
		[]string{store.Note(), note}), "; ")
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
			w, _, _, ferr := fresh("")
			// an empty listing can still hide transcripts this build cannot identify;
			// the note keeps them from reading as absent. Best effort: a scan failure
			// never blocks starting fresh.
			var note string
			if infos, derr := store.Damaged(cwd); derr == nil && len(infos) > 0 {
				note = "no resumable session remains, but " + session.DescribeDamaged(infos)
			}
			return w, snapshot{}, note, ferr
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
		var info session.Info
		var ferr error
		if preAdoptable(store, cwd, pre) {
			info = pre // adopt the live-store hit so the workspace parses once
		} else {
			info, ferr = store.Find(cwd, target)
		}
		if ferr != nil {
			return nil, snapshot{}, "", ferr
		} else if info.Blocked { // refused here so Open never touches a newer transcript
			return nil, snapshot{}, "", refuseNewer(info)
		} else if info.Damaged { // identity-less entries give the rebuild nothing to restore
			return nil, snapshot{}, "", refuseDamaged(info)
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

// preAdoptable reports whether pre may serve as the open's locator: resolved,
// openable here (not blocked or damaged), and inside the workspace's live
// session directory (the demo build redirects the config home after the check,
// so a foreign path is dropped).
func preAdoptable(store *session.Store, cwd string, pre session.Info) bool {
	if pre.Path == "" || pre.Blocked || pre.Damaged {
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

func pickSessionRoot(dialogs *interactionBroker, list []session.Info) (int, error) {
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
	return dialogs.Pick(context.Background(), "Resume session", items,
		tui.PickOptions{Placeholder: "filter"})
}

// restoreState rebuilds resumed state into st from the transcript's branch,
// seeding set with recorded session overrides. It returns that branch, for
// replay, plus resume warnings; a failed read warns through those rather than
// resuming as an unannounced empty conversation.
func (r *sessRec) restoreState(set *config.Set, reg *llm.Registry, st *agent.State, toolsReg *tools.Registry) ([]session.Entry, []string) {
	var entries []session.Entry
	var rwarns []string
	snap := r.snapshot
	r.snapshot = snapshot{}         // consumed: later rebuilds re-read live state
	fi, serr := os.Stat(r.w.Path()) // another process's appends force a live re-read
	if snap.entries != nil && serr == nil && fi.Size() == snap.size && fi.ModTime().Equal(snap.mod) {
		entries, rwarns = snap.entries, snap.warns // the open-time parse: startup reads once
	} else {
		var err error
		entries, rwarns, err = session.Read(r.w.Path())
		if err != nil {
			return nil, append(rwarns, "transcript unreadable: "+err.Error())
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}
	head := resumeHead(r.w.Head(), entries)
	branch := session.Branch(entries, head)
	// stamp the threshold before state resolution so the trigger and context bar
	// read one number after resume. permissions.mode is dropped (even when an older
	// session recorded it) so a restart always starts from the configured default.
	overrides := session.SettingOverrides(branch)
	delete(overrides, "permissions.mode")
	rwarns = append(rwarns, set.SeedSession(overrides)...)
	applyImagesBlock(set) // a session saved with images blocked resumes blocked
	resumed := set.Settings()
	reg.SetCompactDefault(resumed.Compaction.Threshold) // models declaring none pick up the session default

	rebuilt, warns := session.State(branch, modelResolver(reg))
	if len(rebuilt.Messages) == 0 && rebuilt.Model.ID == "" {
		return nil, append(rwarns, warns...) // a brand-new transcript carries no history yet
	}
	st.Messages = rebuilt.Messages
	if rebuilt.Model.ID != "" {
		st.Model = rebuilt.Model
	}
	// the recorded set feeds the driver's post-registration re-apply and the MCP
	// manager's connect-time restore, so the resumed frame's tools survive
	st.Tools = rebuilt.Tools
	if _, ok := llm.ParseLevel(resumed.Reasoning.Level); ok || resumed.Reasoning.Retain != "" {
		st.Reasoning = llm.ReasoningFrom(config.Reasoning{
			Level:  resumed.Reasoning.Level,
			Retain: resumed.Reasoning.Retain,
			Hide:   resumed.Reasoning.Hide,
			Budget: resumed.Reasoning.Budget,
		}, st.Model)
	}
	// a recorded empty set disables everything registered; unrecorded keeps the
	// config default. Tri-state survives the transcript: JSON [] unmarshals non-nil.
	if toolsReg != nil && resumed.Tools.Enabled != nil {
		toolsReg.SetEnabled(resumed.Tools.Enabled)
	}
	st.Tokens = rebuilt.Tokens // a resumed ledger reflects the branch's recorded usage
	return branch, append(rwarns, warns...)
}

func (r *sessRec) rebuild(set *config.Set, ui *hubUI, reg *llm.Registry, st *agent.State, toolsReg *tools.Registry) {
	branch, warns := r.restoreState(set, reg, st, toolsReg)
	for _, wmsg := range warns {
		ui.Notify("resume: "+wmsg, tui.LevelWarn)
	}
	if branch == nil {
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
	session.Replay(branch, replaySink(ui), session.ReplayOptions{})
}

// appendEntry appends one transcript entry, announcing it to any observer,
// so direct writers (the compaction checkpoint) surface like recorder ones.
func (r *sessRec) appendEntry(typ session.Type, data any) (session.Entry, error) {
	e, err := r.w.Append(typ, data)
	if err == nil && r.onAppend != nil {
		r.onAppend(e)
	}
	return e, err
}

// errTurnRunning reports a refused state swap: a turn claimed the loop between
// the caller's idle check and the swap attempt.
var errTurnRunning = errors.New("a turn is running")

// switchState points the writer and live agent state at head's branch, an empty
// head starting a new root, firing the switch callbacks once both moved. It
// reports errTurnRunning when a turn claims the loop first, with nothing moved.
func (r *sessRec) switchState(ui *hubUI, ag *agent.Agent, reg *llm.Registry, head, prefix string) error {
	var rebuilt agent.State
	if head != "" {
		// rebuild before moving HEAD, so a failed read leaves writer and state in step
		entries, _, err := session.Read(r.w.Path())
		if err != nil {
			ui.Notify(prefix+err.Error(), tui.LevelWarn)
			return err
		}
		var warns []string
		rebuilt, warns = session.State(session.Branch(entries, head), modelResolver(reg))
		for _, wmsg := range warns {
			ui.Notify(prefix+wmsg, tui.LevelWarn)
		}
	}

	// mutate the live state in place so every holder (the console, this handler)
	// sees the restored context. The swap runs before the head move and callbacks:
	// a refused swap must leave all of them in step with the untouched state.
	var live llm.Model
	var ledger *tokens.Accounting
	if !ag.WithState(func(st *agent.State) {
		live = st.Model // read before the overwrite below, for a branch that names no model
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
	}) {
		ui.Notify(prefix+errTurnRunning.Error(), tui.LevelWarn)
		return errTurnRunning
	}
	r.w.SetHead(head)
	if r.onSwitch != nil {
		r.onSwitch(rebuilt.Messages)
	}
	if r.resumeAuto != nil {
		r.resumeAuto()
	}
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
	if r.starts != nil {
		committed = r.starts.committed.marked()
	}
	return ag.BaseEstimate(committed)
}

func pushSwitchedContext(ui *hubUI, t *tokens.Accounting) {
	if ui == nil {
		return
	}
	ui.SetContext(tuisink.ContextInfo(t.Context()))
}

// liveModel captures the live model for a switch to keep across a rebuild. ok
// is false when a turn claims the loop, so nothing was captured.
func (r *sessRec) liveModel(ag *agent.Agent) (llm.Model, bool) {
	if ag == nil {
		return llm.Model{}, true
	}
	var save llm.Model
	if !ag.WithState(func(st *agent.State) { save = st.Model }) {
		return llm.Model{}, false
	}
	return save, true
}

func syncModelUI(ui *hubUI, reg *llm.Registry, m llm.Model) {
	reg.SetActive(m)
	if ui == nil {
		return
	}
	ui.SetModelOf(m)
}

// replaySink builds the sink a rewind or resume replay drives: tool images
// render as text, since the drawings already sit in scrollback above. No
// terminal (serve mode) replays nowhere: the transcript is the client's view.
func replaySink(ui *hubUI) agent.Sink {
	if ui.UI == nil {
		return agent.NopSink{}
	}
	s := tuisink.New(ui.UI)
	s.SetImages(false)
	return s
}

// restoreForkModel re-applies m as the live model over the switched context,
// reseeding the ledger against it. It reports errTurnRunning when a turn claims
// the loop first, leaving the model as the switch left it. The caller owns the
// refusal notice.
func (r *sessRec) restoreForkModel(ui *hubUI, ag *agent.Agent, reg *llm.Registry, m llm.Model) error {
	if m.ID == "" || ag == nil {
		return nil
	}
	var ledger *tokens.Accounting
	if !ag.WithState(func(st *agent.State) {
		st.Model = m
		st.Tokens.SetModel(m)
		st.Tokens.Reseed(tokens.EstimateFor(m, st.Reasoning.Retain, st.Messages))
		ledger = st.Tokens
	}) {
		return errTurnRunning
	}
	// this deliberately overwrites what switchState seeded, so the base is measured
	// again here against the fork's model, BaseEstimate takes the lock WithState held
	ledger.SetBase(r.baseEstimate(ag))
	pushSwitchedContext(ui, ledger)
	syncModelUI(ui, reg, m)
	return nil
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

// pickRewind opens the context-tree picker and reports the chosen entry id,
// false when cancelled or nothing is rewindable.
func (r *sessRec) pickRewind(ui *hubUI) (string, bool) {
	// a failed re-read is not the same as nothing recorded yet
	entries, _, err := session.Read(r.w.Path())
	if err != nil {
		ui.Notify("rewind: "+err.Error(), tui.LevelWarn)
		return "", false
	}
	if len(entries) == 0 {
		ui.Notify("nothing to rewind onto yet", tui.LevelInfo)
		return "", false
	}
	head := resumeHead(r.w.Head(), entries)
	tree := session.TreeRows(entries, head)
	if len(tree) == 0 {
		ui.Notify("nothing to rewind onto yet", tui.LevelInfo)
		return "", false
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
	picked, err := ui.dialogs.Pick(context.Background(), "Rewind to", items,
		tui.PickOptions{Placeholder: "filter", Initial: initialRow(tree, head)})
	if err != nil {
		return "", false // cancelled
	}
	return tree[picked].ID, true
}

// errUnknownHead names a rewind target absent from the transcript, so callers
// can answer it as not-found rather than a generic failure.
var errUnknownHead = errors.New("unknown head")

// rewindTo switches onto head's branch, the double-Esc operation minus its
// picker: rebuild state, move the writer, keep the live model, discard staged
// output meant for the branch left behind. An interactive run redraws to the
// restored context; a serve run has nothing to redraw. It reports the new head
// entry id. Notifications for mid-operation failures stay inside (they render
// locally and reach observers as notice events); the returned error is the
// caller's machine-readable copy.
func (r *sessRec) rewindTo(ui *hubUI, ag *agent.Agent, reg *llm.Registry, head string) (string, error) {
	entries, _, err := session.Read(r.w.Path())
	if err != nil {
		return "", err
	}
	newHead, fillText, ok := session.RewindTarget(entries, head)
	if !ok || newHead == "" {
		if !slices.ContainsFunc(entries, func(e session.Entry) bool { return e.ID == head }) {
			return "", fmt.Errorf("%w %q", errUnknownHead, head)
		}
		return "", errors.New("cannot rewind onto that entry")
	}
	// keep the current model across a fork: rebuilding from an earlier point
	// would otherwise revert to whatever model was active there, silently undoing
	// a /model switch when that prior message is re-sent.
	saveModel, ok := r.liveModel(ag)
	if !ok {
		// a refused capture means a turn owns the loop, nothing may move
		ui.Notify("rewind: "+errTurnRunning.Error(), tui.LevelWarn)
		return "", errTurnRunning
	}
	if err := r.switchState(ui, ag, reg, newHead, "rewind: "); err != nil {
		return "", err
	}
	if r.discardStaged != nil {
		r.discardStaged() // staged against the branch just left, not this one's to carry
	}
	// a turn claiming the loop here runs against the switched state, so the redraw
	// below would wipe its live output: leave the screen to it
	if err := r.restoreForkModel(ui, ag, reg, saveModel); err != nil {
		// never force the saved model onto a running turn, name what was not kept instead
		ui.Notify(fmt.Sprintf(
			"rewind: branch switched but a turn is running, model %s not kept, /model re-applies it",
			saveModel.Key()), tui.LevelWarn)
		return newHead, err
	}

	// redraw to just the restored context, then drop the picked text into the
	// prompt so it can be edited or re-sent as this branch's first message.
	if ui.Interactive() {
		ui.Reset()
		// mark where restored history begins so it reads clearly in scrollback
		ui.Divider()
		session.Replay(session.Branch(entries, newHead), replaySink(ui), session.ReplayOptions{})
		if fillText != "" {
			ui.SetInput(fillText)
		}
	}
	return newHead, nil
}
