package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
	tuisink "github.com/jentfoo/ajent/pkg/tui/sink"
)

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
	// discardStaged drops shell results staged against the branch a rewind leaves,
	// so they never ride the new branch's first prompt. Nil until main wires it.
	discardStaged func()
	// resumeAuto re-arms automatic compaction: a shorter branch may reduce where
	// the one it left could not. Nil until main wires it.
	resumeAuto func()
}

func sessionHint(r *sessRec) string {
	entries, ok := sessionEntries(r)
	if !ok {
		return ""
	}
	return rootID(entries)
}

func sessionLabel(r *sessRec) string {
	entries, ok := sessionEntries(r)
	if !ok {
		return ""
	}
	if name := session.NameOf(entries); name != "" {
		return name
	}
	return rootID(entries)
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

func (r *sessRec) empty() bool {
	if r == nil || r.w == nil || r.store == nil {
		return false
	}
	entries, _, err := session.Read(r.w.Path())
	if err != nil {
		return false
	}
	if session.NameOf(entries) != "" {
		return false // --session resumes by name, so a named session is worth keeping
	}
	return !slices.ContainsFunc(entries, func(e session.Entry) bool { return e.Type == session.TypeMessage })
}

// CheckSessionTarget verifies that a requested session exists before the TUI
// opens, so an id or name fails fast rather than silently starting fresh.
func CheckSessionTarget(mode ResumeMode, target string) error {
	store, err := session.NewStore()
	if err != nil {
		return err
	}
	cwd := config.Cwd()
	if mode == ResumeSessionName {
		_, _, ferr := store.FindNamed(cwd, target)
		return ferr
	}
	if _, ferr := store.Find(cwd, target); ferr != nil {
		return fmt.Errorf("no session matches %q", target)
	}
	return nil
}

func newSession(ui *tui.UI, mode ResumeMode, target, modelKey string) *sessRec {
	store, err := session.NewStore()
	if err != nil {
		return nil
	}
	cwd := config.Cwd()
	var pick func([]session.Info) (int, error)
	if ui != nil && mode == ResumePick {
		pick = func(list []session.Info) (int, error) { return pickSessionRoot(ui, list) }
	}
	w, err := openSession(store, mode, cwd, target, modelKey, pick)
	if err != nil {
		return nil
	}
	return &sessRec{store: store, w: w, rec: session.NewRecorder(w)}
}

func openSession(store *session.Store, mode ResumeMode, cwd string, target, modelKey string, pick func([]session.Info) (int, error)) (*session.Writer, error) {
	fresh := func(name string) (*session.Writer, error) {
		return store.Create(cwd, session.SessionData{
			Version:   session.Version(),
			Workspace: cwd,
			Model:     modelKey, // provenance so a resume can stamp assistant origins
			Name:      name,
		})
	}

	switch mode {
	case ResumeContinue:
		info, lerr := store.Latest(cwd)
		if lerr == nil {
			return session.Open(info.Path) // resume the most recent transcript's leaf
		} else if errors.Is(lerr, session.ErrNoSessions) {
			return fresh("")
		}
		return nil, lerr
	case ResumeID:
		info, ferr := store.Find(cwd, target)
		if ferr != nil {
			return nil, ferr
		}
		return session.Open(info.Path) // resume that exact transcript's leaf
	case ResumeSessionName:
		info, found, ferr := store.FindNamed(cwd, target)
		if ferr != nil {
			return nil, ferr
		} else if !found {
			return fresh(target) // first run under this name
		}
		return session.Open(info.Path)
	case ResumePick:
		list, lerr := store.List(cwd)
		if len(list) == 0 || errors.Is(lerr, session.ErrNoSessions) {
			return fresh("") // nothing saved yet; start one
		} else if lerr != nil {
			return nil, lerr
		}
		picked, perr := -1, error(nil)
		if pick != nil { // no UI means we cannot choose; fall back to fresh
			picked, perr = pick(list)
		}
		if errors.Is(perr, tui.ErrCancelled) || picked < 0 {
			return fresh("") // cancelled the resume; start new rather than stall
		} else if perr != nil {
			return nil, perr
		}
		return session.Open(list[picked].Path) // resume that root's leaf
	default: // ResumeNewSession and anything unexpected: always fresh
		return fresh("")
	}
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
	}
	return ui.PickContext(context.Background(), "Resume session", items,
		tui.PickOptions{Placeholder: "filter"})
}

func (r *sessRec) restoreState(set *config.Set, reg *llm.Registry, st *agent.State, toolsReg *tools.Registry) ([]session.Entry, string, []string) {
	entries, _, err := session.Read(r.w.Path())
	if err != nil || len(entries) == 0 {
		return nil, "", nil
	}
	head := resumeHead(r.w.Head(), entries)
	// a session-scoped threshold must be stamped before state resolution so the
	// trigger, the context bar and the band ceiling read one number after resume.
	// The overrides come off the branch, not raw file order: a transcript with
	// forks holds settings from siblings this head never saw.
	set.SeedSession(session.SettingOverrides(session.Branch(entries, head)))
	resumed := set.Settings()
	reg.SetCompactDefault(resumed.Compaction.Threshold) // models declaring none pick up the session default

	rebuilt, warns := r.stateFor(modelResolver(reg), entries, head)
	if len(rebuilt.Messages) == 0 && rebuilt.Model.ID == "" {
		return nil, "", warns // a brand-new transcript carries no history yet
	}
	st.Messages = rebuilt.Messages
	if rebuilt.Model.ID != "" {
		st.Model = rebuilt.Model
	}
	if _, ok := llm.ParseLevel(resumed.Reasoning.Level); ok || resumed.Reasoning.Retain != "" {
		st.Reasoning = llm.ReasoningFrom(config.Reasoning{
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

func (r *sessRec) baseEstimate(ag *agent.Agent) int {
	var committed bool
	if r.started != nil {
		committed = *r.started
	}
	return ag.BaseEstimate(committed)
}

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
	// a large session would repaint every retained line on each arrow press in alt
	// mode; defer that until the message is chosen.
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
		r.discardStaged() // staged against the branch just left; not this one's to carry
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
