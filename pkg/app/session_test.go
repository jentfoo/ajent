package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-analyze/bulk"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/refs"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

// singleToolSet backs an agent with one tool, so a scripted turn can call it.
type singleToolSet struct{ tool agent.Tool }

func (s singleToolSet) Get(name string) (agent.Tool, bool) { return s.tool, name == tools.ToolBash }
func (s singleToolSet) Schemas() []llm.ToolSchema          { return []llm.ToolSchema{s.tool.Schema()} }
func (s singleToolSet) Names() []string                    { return []string{tools.ToolBash} }

// noopRewindTool executes instantly so the loop never blocks.
type noopRewindTool struct{}

func (noopRewindTool) Name() string                { return tools.ToolBash }
func (noopRewindTool) Label(agent.ToolCall) string { return "bash: ..." }
func (noopRewindTool) Description() string         { return "test tool" }
func (noopRewindTool) Schema() llm.ToolSchema      { return llm.ToolSchema{Name: tools.ToolBash} }
func (noopRewindTool) Mode() agent.ExecutionMode {
	return agent.ModeSerial
}
func (noopRewindTool) Close() {}
func (noopRewindTool) Execute(_ context.Context, _ agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
}

// textTurnRewind frames one assistant turn that emits a single text reply.
func textTurnRewind(text string) []llm.Event {
	return []llm.Event{
		{Type: llm.EventMessageStart},
		{Type: llm.EventTextStart, Index: 0},
		{Type: llm.EventTextDelta, Index: 0, Text: text},
		{Type: llm.EventTextEnd, Index: 0},
		{Type: llm.EventDone, StopReason: llm.StopEndTurn},
	}
}

func readEntriesRewind(t *testing.T, p string) []session.Entry {
	t.Helper()

	e, _, err := session.Read(p)
	require.NoError(t, err)
	return e
}

func TestRewindStateRebuild(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	p := filepath.Join(dir, "2026-01-02T03-04-05Z-test.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	r := &sessRec{
		store: session.StoreAt(dir),
		w:     w,
		rec:   session.NewRecorder(w),
	}

	// grow a chain: user "one" -> assistant reply
	set := singleToolSet{tool: noopRewindTool{}}
	a := agent.New(&agent.State{Model: llm.Model{ID: "p/m"}, Tools: []string{tools.ToolBash}, Tokens: tokens.New(llm.Model{ID: "p/m"})}, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) {
			return &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textTurnRewind("reply one")}}}, nil
		},
		Sinks:     []agent.Sink{r.rec.Sink(agent.NopSink{})},
		Tools:     set,
		Env:       agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
		OnMessage: []func(agent.MessageInfo){r.rec.Message},
	})
	require.NoError(t, a.Prompt(t.Context(), agent.Input{Text: "one"}))

	entries := readEntriesRewind(t, p)
	branch := session.Branch(entries, w.Head())
	assert.Len(t, branch, 3) // session + user "one" + assistant reply

	// rewind onto the *first user message* (index 1). Context is just that
	// prompt, because rewinding drops everything after the pick.
	st, _ := session.State(session.Branch(entries, branch[1].ID), rewindResolve())
	require.Len(t, st.Messages, 1)
	assert.Equal(t, llm.RoleUser, st.Messages[0].Role)
	assert.Equal(t, "one", textOf(st.Messages[0]))

	// rewinding onto the tip keeps the full chain.
	stTip, _ := session.State(session.Branch(entries, branch[len(branch)-1].ID), rewindResolve())
	require.Len(t, stTip.Messages, 2) // "one" + its assistant reply
	assert.Equal(t, llm.RoleAssistant, stTip.Messages[1].Role)

	// rewind the writer onto "one" and grow a new submission there: this forks
	// off the old reply, so TreeRows must show both chains as branches.
	tipOld := branch[len(branch)-1].ID // assistant "reply one" stays an abandoned tip
	w.SetHead(branch[1].ID)
	forkA, err := w.Append(session.TypeMessage,
		session.MessageData{Message: llm.Text(llm.RoleUser, "rewind + resubmit")})
	require.NoError(t, err)

	entries = readEntriesRewind(t, p)
	tree := session.TreeRows(entries, forkA.ID) // active head is the new submission

	// both branches are present: the old tip and the new one
	ids := make([]string, len(tree))
	for i, tr := range tree {
		ids[i] = tr.ID
	}
	assert.Contains(t, ids, tipOld)
	assert.Contains(t, ids, forkA.ID)

	// only the new chain is active, the abandoned old reply is not
	assert.False(t, slices.ContainsFunc(tree, func(tr session.TreeRow) bool {
		return tr.ID == tipOld && tr.Active
	}))
}

func TestRewindClosesOpenPicker(t *testing.T) {
	t.Parallel()

	reg, warnings := llm.NewRegistry(llm.File{
		Providers: map[string]llm.ProviderConfig{
			"p": {Models: []llm.ModelConfig{{ID: "m"}}},
		},
	}, nil, llm.RegistryOptions{})
	require.Empty(t, warnings)

	dir := t.TempDir()
	p := filepath.Join(dir, "2026-01-02T03-04-05Z-pick.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version(), Model: "p/m"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	r := &sessRec{w: w, rec: session.NewRecorder(w)}
	u1, err := w.Append(session.TypeMessage,
		session.MessageData{Message: llm.Text(llm.RoleUser, "hello")})
	require.NoError(t, err)

	// nothing armed: a rewind closes nothing and posts no notice
	assert.False(t, r.closePick())

	ag := agent.New(&agent.State{Model: llm.Model{ID: "p/m"}, Tokens: tokens.New(llm.Model{ID: "p/m"})}, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) {
			return &llm.ScriptedProvider{}, nil
		},
		Env: agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
	})

	var opened atomic.Bool
	b := newInteractionBroker(nil, func(interactionInfo) { opened.Store(true) }, nil)
	ui := newSessionHub(hubDeps{dialogs: b}).ui
	var log emitLog
	log.attach(ui.hub)
	// the driver wires branch.changed onto the switch callback; mirror that here
	r.onSwitch = func(_ []llm.Message, cause string) { ui.hub.setHead(w.Head(), cause) }

	var ok, returned atomic.Bool
	go func() {
		_, got := r.pickRewind(ui)
		ok.Store(got)
		returned.Store(true)
	}()
	require.Eventually(t, opened.Load, ctlWait, 5*time.Millisecond)

	// a rewind from the other surface: one transaction closes the armed picker
	head, err := r.rewindTo(ui, ag, reg, u1.ID, rewindCauseRewind)
	require.NoError(t, err)
	assert.Equal(t, readEntriesRewind(t, p)[0].ID, head) // rewound onto the prompt's parent
	require.Eventually(t, returned.Load, ctlWait, 5*time.Millisecond)
	assert.False(t, ok.Load()) // the closed picker answers its surface as a cancellation

	// the closing notice rides the transient family, beside the branch change
	require.Len(t, log.noticesSeen(), 1) // and it is the only notice this rewind posts
	assert.Contains(t, log.noticesSeen(),
		"|rewind picker closed: the session moved under it")
	branches := log.named(ctl.EventBranchChanged)
	require.Len(t, branches, 1)

	// disarmed once returned: the next rewind closes nothing
	assert.False(t, r.closePick())
}

func TestPickRewindDisarmsOnAnswer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	p := filepath.Join(dir, "2026-01-02T03-04-05Z-pick.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	r := &sessRec{w: w, rec: session.NewRecorder(w)}
	_, err = w.Append(session.TypeMessage,
		session.MessageData{Message: llm.Text(llm.RoleUser, "hello")})
	require.NoError(t, err)

	local := &scriptedLocal{picks: []int{0}}
	b := newInteractionBroker(local, nil, nil)
	ui := newSessionHub(hubDeps{dialogs: b}).ui

	id, ok := r.pickRewind(ui)
	require.True(t, ok)
	assert.NotEmpty(t, id)
	assert.False(t, r.closePick()) // the answer disarmed the cancel
}

func TestInitialRow(t *testing.T) {
	t.Parallel()

	tree := []session.TreeRow{
		{ID: "u1", Active: true},
		{ID: "a1", Active: true},  // head sits here after rewinding onto u2
		{ID: "u2", Active: false}, // an abandoned fork below the head
		{ID: "a2", Active: false},
	}

	t.Run("head_row", func(t *testing.T) {
		assert.Equal(t, 1, initialRow(tree, "a1"))
	})

	t.Run("head_without_row", func(t *testing.T) {
		// a session or tool-only entry has no row: the last row still in context stands in
		assert.Equal(t, 1, initialRow(tree, "no-row"))
	})

	t.Run("nothing_active", func(t *testing.T) {
		stale := make([]session.TreeRow, 0, len(tree))
		for _, r := range tree {
			stale = append(stale, session.TreeRow{ID: r.ID})
		}
		assert.Equal(t, len(stale)-1, initialRow(stale, ""))
	})

	t.Run("single_row", func(t *testing.T) {
		assert.Equal(t, 0, initialRow(tree[:1], "u1"))
	})
}

func TestRewindRowLabels(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		row     session.TreeRow
		wantTag string
		want    string
	}{
		{"user", session.TreeRow{Kind: session.RowUser, Label: "user: hi"}, "user", "hi"},
		{"assistant", session.TreeRow{Kind: session.RowAssistant, Label: "assistant: hello"}, "agent", "hello"},
		{"tool", session.TreeRow{Kind: session.RowTool, Label: "[ls] docs/"}, "tool", "[ls] docs/"},
		{"compaction", session.TreeRow{Kind: session.RowCompaction, Label: "compaction: 12k → 4k"}, "compact", "12k → 4k"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tag, mark := roleTag(c.row.Kind)
			assert.Equal(t, c.wantTag, tag)
			assert.NotEqual(t, tui.MarkNone, mark)
			assert.Equal(t, c.want, rewindBody(c.row))
		})
	}
}

func TestRewindKeepsCurrentModel(t *testing.T) {
	t.Parallel()

	reg, warnings := llm.NewRegistry(llm.File{
		Providers: map[string]llm.ProviderConfig{
			"p": {Models: []llm.ModelConfig{{ID: "a"}, {ID: "b"}}},
		},
	}, nil, llm.RegistryOptions{})
	require.Empty(t, warnings)

	dir := t.TempDir()
	p := filepath.Join(dir, "2026-01-02T03-04-05Z-model.jsonl")
	w, err := session.Create(p, session.SessionData{
		Version: session.Version(), Model: "p/a",
	})
	require.NoError(t, err)
	r := &sessRec{w: w, rec: session.NewRecorder(w)}

	// grow a chain on model a
	a := agent.New(&agent.State{Model: llm.Model{ID: "a"}, Tools: []string{tools.ToolBash}, Tokens: tokens.New(llm.Model{ID: "a"})}, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) {
			return &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textTurnRewind("reply one")}}}, nil
		},
		Tools:     singleToolSet{tool: noopRewindTool{}},
		Env:       agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
		OnMessage: []func(agent.MessageInfo){r.rec.Message},
	})
	require.NoError(t, a.Prompt(t.Context(), agent.Input{Text: "one"}))

	entries := readEntriesRewind(t, p)
	branch := session.Branch(entries, w.Head())

	// switch to b live and record it, as /model does
	bModel, err := reg.Resolve("p/b")
	require.NoError(t, err)
	a.WithState(func(st *agent.State) { st.Model = bModel })
	r.rec.ModelChange(bModel, "command")

	// rewind onto the first user message and restore the fork model: it must stay b
	save, ok := r.liveModel(a)
	require.True(t, ok)
	require.NoError(t, r.switchState(nil, a, reg, branch[1].ID, rewindCauseRewind, "rewind: "))
	require.NoError(t, r.restoreForkModel(nil, a, reg, save))

	var got llm.Model
	a.WithState(func(st *agent.State) { got = st.Model })
	assert.Equal(t, "b", got.ID)
	assert.NotEqual(t, "a", got.ID)
}

func TestRewindTarget(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version()})
	require.NoError(t, err)

	u1, err := w.Append(session.TypeMessage,
		session.MessageData{Message: llm.Text(llm.RoleUser, "hello world")})
	require.NoError(t, err)
	a2, err := w.Append(session.TypeMessage,
		session.MessageData{Message: llm.Text(llm.RoleAssistant, "hi there")})
	require.NoError(t, err)

	entries := readEntriesRewind(t, p)

	// picking the assistant reply keeps that message as its own head with no pre-fill
	head, fill, ok := session.RewindTarget(entries, a2.ID)
	assert.True(t, ok)
	assert.Equal(t, a2.ID, head)
	assert.Empty(t, fill)

	// picking the first user message rewinds to its parent (the session entry),
	// so only that text sits in the editor.
	head2, fill2, ok := session.RewindTarget(entries, u1.ID)
	assert.True(t, ok)
	assert.Equal(t, entries[0].ID, head2) // the session entry
	assert.Equal(t, "hello world", fill2)
}

func TestOpenSessionModes(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	store := session.StoreAt(filepath.Join(ws, "root"))

	// seed one saved transcript so resume/continue have something to reuse
	seed, err := store.Create(ws, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	firstPath := seed.Path()

	// capture the seed's id while it is still the only session, a later
	// newest-first listing cannot be trusted once other sessions share its
	// second-granularity timestamp.
	seedList, err := store.List(ws)
	require.NoError(t, err)
	require.Len(t, seedList, 1)
	seedID := seedList[0].ID

	pickFirst := func(list []session.Info) (int, error) {
		require.NotEmpty(t, list)
		return 0, nil // select the newest root
	}

	// The seed-relative assertions below run while the seed is still the only
	// session, ResumeNewSession and the cancelled resume each create a newer file,
	// so they come last to keep "latest" pointing at the seed above.

	// --continue: reuse the most recent transcript (the seed, still alone)
	wCont, _, _, err := openSession(store, ResumeContinue, ws, "", "", pickFirst, session.Info{})
	require.NoError(t, err)
	assert.Equal(t, firstPath, wCont.Path())

	// --resume with a selection: reuse that root's transcript
	wPick, _, _, err := openSession(store, ResumePick, ws, "", "", pickFirst, session.Info{})
	require.NoError(t, err)
	assert.Equal(t, firstPath, wPick.Path())

	// --resume <id>: reuse that exact saved transcript directly
	wID, _, _, err := openSession(store, ResumeID, ws, seedID, "", pickFirst, session.Info{})
	require.NoError(t, err)
	assert.Equal(t, firstPath, wID.Path())

	// no flag: always a fresh file, never reuses
	wNew, _, _, err := openSession(store, ResumeNewSession, ws, "", "", pickFirst, session.Info{})
	require.NoError(t, err)
	assert.NotEqual(t, firstPath, wNew.Path())

	// --resume cancelled (ErrCancelled): start fresh rather than stall
	wCancel, _, _, err := openSession(store, ResumePick, ws, "", "",
		func([]session.Info) (int, error) { return 0, tui.ErrCancelled }, session.Info{})
	require.NoError(t, err)
	assert.NotEqual(t, firstPath, wCancel.Path())
}

func TestOpenSessionResumePickHeadless(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	store := session.StoreAt(filepath.Join(ws, "root"))

	// an empty workspace needs no choice: fresh matches the picker's own
	// empty-list rule, so there is nothing to refuse yet
	w, _, _, err := openSession(store, ResumePick, ws, "", "", nil, session.Info{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	// now a saved session exists but headless has no way to choose it
	_, _, _, err = openSession(store, ResumePick, ws, "", "", nil, session.Info{})
	assert.ErrorContains(t, err, "bare --resume")
}

func TestOpenSessionContinueRewound(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	store := session.StoreAt(filepath.Join(ws, "root"))

	// session A ends on a rewind, so its branch head is a1 while its file tail is a3
	wa, err := store.Create(ws, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	a1, err := wa.Append(session.TypeMessage,
		session.MessageData{Message: llm.Text(llm.RoleUser, "one")})
	require.NoError(t, err)
	_, err = wa.Append(session.TypeMessage,
		session.MessageData{Message: llm.Text(llm.RoleUser, "two")})
	require.NoError(t, err)
	a3, err := wa.Append(session.TypeMessage,
		session.MessageData{Message: llm.Text(llm.RoleUser, "three")})
	require.NoError(t, err)
	wa.SetHead(a1.ID)
	require.NoError(t, wa.Close())

	// session B is started later and synced, which must not disturb A's cursor
	wb, err := store.Create(ws, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	_, err = wb.Append(session.TypeMessage,
		session.MessageData{Message: llm.Text(llm.RoleUser, "other")})
	require.NoError(t, err)
	require.NoError(t, wb.Sync())
	require.NoError(t, wb.Close())

	// A was worked in most recently even though B was started later
	used := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(wa.Path(), used, used))

	w, _, _, err := openSession(store, ResumeContinue, ws, "", "", nil, session.Info{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	assert.Equal(t, wa.Path(), w.Path())
	assert.Equal(t, a1.ID, w.Head())

	// the abandoned tail stays on disk but off the resumed branch
	entries, _, rerr := session.Read(w.Path())
	require.NoError(t, rerr)
	branch := session.Branch(entries, w.Head())
	assert.False(t, slices.ContainsFunc(branch, func(e session.Entry) bool { return e.ID == a3.ID }))
}

func TestOpenSessionByName(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	store := session.StoreAt(filepath.Join(ws, "root"))

	created, _, _, err := openSession(store, ResumeSessionName, ws, "fix-parser", "p/a", nil, session.Info{})
	require.NoError(t, err)
	require.NoError(t, created.Close())

	entries, _, rerr := session.Read(created.Path())
	require.NoError(t, rerr)
	assert.Equal(t, "fix-parser", session.NameOf(entries))

	// the same name reopens it, a different one starts its own transcript
	resumed, _, _, err := openSession(store, ResumeSessionName, ws, "fix-parser", "p/a", nil, session.Info{})
	require.NoError(t, err)
	assert.Equal(t, created.Path(), resumed.Path())

	other, _, _, err := openSession(store, ResumeSessionName, ws, "other", "p/a", nil, session.Info{})
	require.NoError(t, err)
	assert.NotEqual(t, created.Path(), other.Path())

	// a name is reachable through --resume too
	byResume, _, _, err := openSession(store, ResumeID, ws, "fix-parser", "", nil, session.Info{})
	require.NoError(t, err)
	assert.Equal(t, created.Path(), byResume.Path())

	// a name that only reaches a session by id is refused rather than resuming one
	// the user never named, the guard lives in openSession, not just its caller
	list, lerr := store.List(ws)
	require.NoError(t, lerr)
	require.NotEmpty(t, list)
	_, _, _, err = openSession(store, ResumeSessionName, ws, list[0].ID, "p/a", nil, session.Info{})
	assert.ErrorContains(t, err, "matches session id")
}

func TestResumeByID(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	store := session.StoreAt(filepath.Join(ws, "root"))

	// two distinct saved sessions, resume the older one by id
	_, err := store.Create(ws, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	older, err := store.Create(ws, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	list, err := store.List(ws)
	require.NoError(t, err)
	require.Len(t, list, 2)

	// resolve the saved info for that older file so we can address it by id
	var want session.Info
	for _, in := range list {
		if in.Path == older.Path() {
			want = in
		}
	}
	require.NotEmpty(t, want.ID)

	// the picker's chosen index maps to that exact root among several
	for i, in := range list {
		if in.Path != older.Path() {
			continue
		}

		wPick, _, _, err := openSession(store, ResumePick, ws, "", "",
			func([]session.Info) (int, error) { return i, nil }, session.Info{})
		require.NoError(t, err)
		assert.Equal(t, older.Path(), wPick.Path())
	}

	w, _, _, err := openSession(store, ResumeID, ws, want.ID, "", nil, session.Info{})
	require.NoError(t, err)
	assert.Equal(t, older.Path(), w.Path())

	// a bogus id resolves to ErrNotFound.
	_, _, _, err = openSession(store, ResumeID, ws, "NO-SUCH-ID", "", nil, session.Info{})
	assert.ErrorIs(t, err, session.ErrNotFound)
}

// TestResumeSyncsActiveModel pins the resume fix: a transcript naming p/b must
// make the registry's active entry follow, so /model preselects and names the
// model the session runs instead of the config default. Before the sync a
// restored session kept Active on the default while state ran p/b, so picking
// p/b read as a silent no-op.
func TestResumeSyncsActiveModel(t *testing.T) {
	t.Parallel()

	reg, warnings := llm.NewRegistry(llm.File{
		Providers: map[string]llm.ProviderConfig{
			"p": {Models: []llm.ModelConfig{{ID: "a"}, {ID: "b"}}},
		},
	}, nil, llm.RegistryOptions{})
	require.Empty(t, warnings)
	require.Equal(t, "p/a", reg.Active().Key()) // registry default before any resume

	dir := t.TempDir()
	p := filepath.Join(dir, "2026-01-02T03-04-05Z-model.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version(), Model: "p/b"})
	require.NoError(t, err)

	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
	})
	ui, err := tui.New(tui.Options{In: inR, Out: outW, Mode: tui.ModePlain})
	require.NoError(t, err)
	t.Cleanup(ui.Close)

	set, _, err := config.Load(config.Options{
		Workspace: t.TempDir(),
		Env:       func(string) string { return "" },
	})
	require.NoError(t, err)

	st := &agent.State{Model: reg.Active(), Tokens: tokens.New(reg.Active())}
	(&sessRec{w: w}).rebuild(set, wrapUI(t, ui), reg, st, nil)

	assert.Equal(t, "p/b", st.Model.Key())
	assert.Equal(t, "p/b", reg.Active().Key()) // the /model preselect follows the resume
}

// A session-scoped compaction.threshold must survive --resume: it is stamped into
// the registry before state resolution so the trigger, the context bar and the band
// ceiling read one number. Before the fix the resumed ledger kept the global 0.8.
func TestResumeAppliesSessionCompactionThreshold(t *testing.T) {
	t.Parallel()

	win := 200000
	reg, warnings := llm.NewRegistry(llm.File{
		Providers: map[string]llm.ProviderConfig{
			"p": {Models: []llm.ModelConfig{{
				ID:            "a",
				ContextWindow: &win,
			}}},
		},
	}, nil, llm.RegistryOptions{})
	require.Empty(t, warnings)

	p := filepath.Join(t.TempDir(), "2026-01-02T03-04-05Z-model.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version(), Model: "p/a"})
	require.NoError(t, err)

	// a couple of messages so the branch is non-empty, then a threshold override
	_, _ = w.Append(session.TypeMessage, session.MessageData{
		Message: llm.Text(llm.RoleUser, "do the work"),
	})
	raw := json.RawMessage(`0.5`)
	_, err = w.Append(session.TypeSettingChange, session.SettingData{Key: "compaction.threshold", Value: raw})
	require.NoError(t, err)

	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
	})
	ui, err := tui.New(tui.Options{In: inR, Out: outW, Mode: tui.ModePlain})
	require.NoError(t, err)
	t.Cleanup(ui.Close)

	set, _, err := config.Load(config.Options{
		Workspace: t.TempDir(),
		Env:       func(string) string { return "" },
	})
	require.NoError(t, err)

	st := &agent.State{Model: reg.Active(), Tokens: tokens.New(reg.Active())}
	(&sessRec{w: w}).rebuild(set, wrapUI(t, ui), reg, st, nil)

	// the session override 0.5 survives resume onto a model with no declared one,
	// so the trigger and the bar read it rather than the global default.
	require.NotNil(t, st.Tokens)
	cs := st.Tokens.Context()
	at := tokens.CompactAt(st.Model)
	assert.InDelta(t, 0.5, st.Model.CompactThreshold, 1e-9)
	assert.Equal(t, at, cs.Compact) // the ledger's compact term follows the resumed threshold
}

// A recorded tools.enabled must survive resume: the rebuilt state carries it (so
// the driver's post-registration re-apply and the MCP restore set see it) and the
// live registry reflects it. Subagents were off in the recorded frame, so the
// trio registering after the rebuild must not resurrect them.
func TestResumeRestoresRecordedToolSet(t *testing.T) {
	t.Parallel()

	reg, warnings := llm.NewRegistry(llm.File{
		Providers: map[string]llm.ProviderConfig{
			"p": {Models: []llm.ModelConfig{{ID: "a"}}},
		},
	}, nil, llm.RegistryOptions{})
	require.Empty(t, warnings)

	p := filepath.Join(t.TempDir(), "2026-01-02T03-04-05Z-model.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version(), Model: "p/a"})
	require.NoError(t, err)

	_, err = w.Append(session.TypeMessage, session.MessageData{
		Message: llm.Text(llm.RoleUser, "do the work"),
	})
	require.NoError(t, err)
	// the session enabled the readers and dropped bash; subagents never came on
	raw := json.RawMessage(`["read","write","edit","grep","find","ls"]`)
	_, err = w.Append(session.TypeSettingChange, session.SettingData{Key: "tools.enabled", Value: raw})
	require.NoError(t, err)

	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
	})
	ui, err := tui.New(tui.Options{In: inR, Out: outW, Mode: tui.ModePlain})
	require.NoError(t, err)
	t.Cleanup(ui.Close)

	set, _, err := config.Load(config.Options{
		Workspace: t.TempDir(),
		Env:       func(string) string { return "" },
	})
	require.NoError(t, err)

	toolsReg, terr := builtinTools(set, reg, nil, nil)
	require.NoError(t, terr)

	st := &agent.State{Model: reg.Active(), Tokens: tokens.New(reg.Active())}
	(&sessRec{w: w}).rebuild(set, wrapUI(t, ui), reg, st, toolsReg)

	// the recorded frame lands in state, feeding the MCP restore set
	assert.Equal(t, []string{"read", "write", "edit", "grep", "find", "ls"}, st.Tools)
	// builtins restored through the seeded session layer, in declaration order:
	// readers on, bash off
	assert.Equal(t, []string{"read", "write", "edit", "find", "grep", "ls"}, toolsReg.Names())

	// the trio registers after the rebuild (driver order), enabled by default
	for _, name := range []string{subAgentToolStart, subAgentToolPoll, subAgentToolList} {
		toolsReg.RegisterFrom(tools.SourceBuiltin, &stubTool{name: name}, true)
	}
	// the driver's re-apply of the recorded set must then switch them back off
	toolsReg.SetEnabled(st.Tools)
	for _, name := range []string{subAgentToolStart, subAgentToolPoll, subAgentToolList} {
		assert.False(t, toolsReg.IsEnabled(name))
	}
	assert.False(t, toolsReg.IsEnabled(tools.ToolBash))
	assert.True(t, toolsReg.IsEnabled(tools.ToolGrep))
}

// A recorded empty set means the session ran with every tool off: resume comes
// back all-off rather than the config default, and a late registration stays off.
func TestResumeRestoresEmptyToolSet(t *testing.T) {
	t.Parallel()

	reg, warnings := llm.NewRegistry(llm.File{
		Providers: map[string]llm.ProviderConfig{
			"p": {Models: []llm.ModelConfig{{ID: "a"}}},
		},
	}, nil, llm.RegistryOptions{})
	require.Empty(t, warnings)

	p := filepath.Join(t.TempDir(), "2026-01-02T03-04-05Z-model.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version(), Model: "p/a"})
	require.NoError(t, err)

	_, err = w.Append(session.TypeMessage, session.MessageData{
		Message: llm.Text(llm.RoleUser, "do the work"),
	})
	require.NoError(t, err)
	// an explicitly empty enabled set, distinct from never recording one
	raw := json.RawMessage(`[]`)
	_, err = w.Append(session.TypeSettingChange, session.SettingData{Key: "tools.enabled", Value: raw})
	require.NoError(t, err)

	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
	})
	ui, err := tui.New(tui.Options{In: inR, Out: outW, Mode: tui.ModePlain})
	require.NoError(t, err)
	t.Cleanup(ui.Close)

	set, _, err := config.Load(config.Options{
		Workspace: t.TempDir(),
		Env:       func(string) string { return "" },
	})
	require.NoError(t, err)

	toolsReg, terr := builtinTools(set, reg, nil, nil)
	require.NoError(t, terr)

	st := &agent.State{Model: reg.Active(), Tokens: tokens.New(reg.Active())}
	(&sessRec{w: w}).rebuild(set, wrapUI(t, ui), reg, st, toolsReg)

	// the empty frame survives as empty, not nil, feeding every restore path
	assert.NotNil(t, st.Tools)
	assert.Empty(t, st.Tools)
	assert.Empty(t, toolsReg.Names()) // builtins restored off through the seeded session layer

	// the trio registers after the rebuild (driver order), then the re-apply of
	// the empty set must keep them off instead of falling back to defaults
	for _, name := range []string{subAgentToolStart, subAgentToolPoll, subAgentToolList} {
		toolsReg.RegisterFrom(tools.SourceBuiltin, &stubTool{name: name}, true)
	}
	toolsReg.SetEnabled(st.Tools)
	assert.False(t, toolsReg.IsEnabled(subAgentToolStart))
	assert.False(t, toolsReg.IsEnabled(tools.ToolRead))
}

// writeFutureSession plants a transcript stamped by a newer session format in the
// workspace store, named when name is set: exactly what a downgrade finds.
func writeFutureSession(t *testing.T, store *session.Store, ws, name string) {
	t.Helper()

	dir, err := store.Dir(ws)
	require.NoError(t, err)
	line := fmt.Sprintf(`{"id":"future-root","type":"session","ts":%d,"data":{"version":999,"name":%q}}`,
		time.Now().Add(time.Hour).UnixMilli(), name)
	p := filepath.Join(dir, "2099-01-01T00-00-00Z-future.jsonl")
	require.NoError(t, os.WriteFile(p, []byte(line+"\n"), 0o600))
}

// writeLostIdentity plants a transcript whose session entry cannot decode into
// SessionData, what a torn or hand-edited identity line leaves behind.
func writeLostIdentity(t *testing.T, store *session.Store, ws string) string {
	t.Helper()

	dir, err := store.Dir(ws)
	require.NoError(t, err)
	line := fmt.Sprintf(`{"id":"lost-root","type":"session","ts":%d,"data":"gone"}`, time.Now().Add(time.Hour).UnixMilli())
	p := filepath.Join(dir, "2099-01-01T00-00-02Z-lost.jsonl")
	require.NoError(t, os.WriteFile(p, []byte(line+"\n"), 0o600))
	return p
}

func TestOpenSessionContinueSkipsNewer(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	store := session.StoreAt(filepath.Join(ws, "root"))

	older, err := store.Create(ws, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = older.Close() })
	writeFutureSession(t, store, ws, "future-work")

	w, _, note, err := openSession(store, ResumeContinue, ws, "", "", nil, session.Info{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	assert.Equal(t, older.Path(), w.Path()) // the newest readable one resumes
	assert.Contains(t, note, "future-work")
	assert.Contains(t, note, "newer session format")

	// with every saved session blocked there is nothing to resume: fresh, still loudly
	require.NoError(t, os.Remove(older.Path()))
	w2, _, note2, err := openSession(store, ResumeContinue, ws, "", "", nil, session.Info{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w2.Close() })
	assert.NotEqual(t, older.Path(), w2.Path())
	assert.Contains(t, note2, "newer session format")
}

func TestOpenSessionRefusesNewerFormat(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	store := session.StoreAt(filepath.Join(ws, "root"))
	writeFutureSession(t, store, ws, "future-work")

	_, _, _, err := openSession(store, ResumeID, ws, "future-work", "", nil, session.Info{})
	require.ErrorContains(t, err, "newer session format")

	// --session refuses the held name instead of re-minting it as a fresh
	// transcript that would make a future upgrade's --resume ambiguous
	_, _, _, err = openSession(store, ResumeSessionName, ws, "future-work", "", nil, session.Info{})
	require.ErrorContains(t, err, "newer session format")

	// the picker only shades a blocked row, so a picked one refuses too
	_, _, _, err = openSession(store, ResumePick, ws, "", "",
		func([]session.Info) (int, error) { return 0, nil }, session.Info{})
	require.ErrorContains(t, err, "newer session format")

	list, lerr := store.List(ws)
	require.NoError(t, lerr)
	assert.Len(t, list, 1) // no transcript was minted or appended anywhere
}

func TestCheckSessionTargetRefusesNewer(t *testing.T) {
	// not parallel: t.Chdir and AJENT_HOME pin process state
	store, ws := deleteWorkspace(t)
	writeFutureSession(t, store, ws, "future-work")

	_, err := CheckSessionTarget(ResumeID, "future-work")
	require.ErrorContains(t, err, "newer session format")
	_, err = CheckSessionTarget(ResumeSessionName, "FUTURE-WORK")
	assert.ErrorContains(t, err, "newer session format")
}

// a damaged transcript never lists, so its full path is the only handle: resume
// refuses it with the removal hint while a readable file opens by its path too
func TestOpenSessionDamagedTranscript(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	store := session.StoreAt(filepath.Join(ws, "root"))
	lost := writeLostIdentity(t, store, ws)

	_, _, _, err := openSession(store, ResumeID, ws, lost, "", nil, session.Info{})
	require.ErrorContains(t, err, "--delete") // refused with the way out

	// a healthy transcript resolves through the same path fallback and opens
	healthy, err := store.Create(ws, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = healthy.Close() })
	w, _, _, err := openSession(store, ResumeID, ws, healthy.Path(), "", nil, session.Info{})
	require.NoError(t, err)
	assert.Equal(t, healthy.Path(), w.Path())
}

// --continue over a listing that is empty only because the directory's remaining
// transcripts are damaged starts fresh, loudly about what it left behind
func TestOpenSessionContinueOverDamaged(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	store := session.StoreAt(filepath.Join(ws, "root"))
	lost := writeLostIdentity(t, store, ws)

	w, _, note, err := openSession(store, ResumeContinue, ws, "", "", nil, session.Info{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	assert.NotEqual(t, lost, w.Path()) // fresh transcript, not a silent loss
	assert.Contains(t, note, lost)     // naming the file it cannot resume
}

func TestCheckSessionTargetRefusesDamaged(t *testing.T) {
	// not parallel: t.Chdir and AJENT_HOME pin process state
	store, ws := deleteWorkspace(t)
	lost := writeLostIdentity(t, store, ws)

	_, err := CheckSessionTarget(ResumeID, lost)
	require.ErrorContains(t, err, "--delete")
}

func TestCheckSessionTargetErrorMapping(t *testing.T) {
	// not parallel: t.Chdir and AJENT_HOME pin process state

	t.Run("unknown_says_no_match", func(t *testing.T) {
		deleteWorkspace(t)

		_, err := CheckSessionTarget(ResumeID, "no-such")
		require.ErrorContains(t, err, `no session matches "no-such"`)
	})

	// two sessions answering to one target are ambiguous, never absent: the
	// fail-fast check keeps Find's words like --delete does instead of masking them
	t.Run("ambiguous_not_masked", func(t *testing.T) {
		store, ws := deleteWorkspace(t)
		writeDeletableSession(t, store, ws, "dupe")
		writeDeletableSession(t, store, ws, "dupe")

		_, err := CheckSessionTarget(ResumeID, "dupe")
		require.ErrorContains(t, err, `ambiguous session name "dupe"`)
		assert.NotContains(t, err.Error(), "no session matches")

		_, err = CheckSessionTarget(ResumeSessionName, "dupe")
		assert.ErrorContains(t, err, `ambiguous session name "dupe"`)
	})

	// a store that cannot be listed must not read as nothing to resume either
	t.Run("listing_failure_not_masked", func(t *testing.T) {
		store, ws := deleteWorkspace(t)
		dir, err := store.Dir(ws)
		require.NoError(t, err)
		require.NoError(t, os.Remove(dir))
		// a file where the workspace's session directory belongs fails List
		require.NoError(t, os.WriteFile(dir, nil, 0o600))

		_, err = CheckSessionTarget(ResumeID, "whatever")
		require.ErrorContains(t, err, dir) // Find's failure passes through verbatim
		assert.NotContains(t, err.Error(), "no session matches")
	})
}

// sessionLabel names a transcript the way --resume takes it back.
func sessionLabel(r *sessRec) string {
	entries, ok := sessionEntries(r)
	if !ok {
		return ""
	}
	return labelOf(entries)
}

func TestSessionHint(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version()})
	require.NoError(t, err)

	r := &sessRec{w: w}
	hint := sessionHint(r)
	assert.NotEmpty(t, hint)

	// nil / unwired records yield no hint
	assert.Empty(t, sessionHint(nil))
}

func TestSessionLabel(t *testing.T) {
	t.Parallel()

	t.Run("unnamed_returns_id", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := session.Create(p, session.SessionData{Version: session.Version()})
		require.NoError(t, err)

		assert.Equal(t, sessionHint(&sessRec{w: w}), sessionLabel(&sessRec{w: w}))
	})

	t.Run("named_returns_name", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := session.Create(p,
			session.SessionData{Version: session.Version(), Name: "fix-parser"})
		require.NoError(t, err)

		assert.Equal(t, "fix-parser", sessionLabel(&sessRec{w: w}))
	})

	t.Run("rename_wins", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := session.Create(p,
			session.SessionData{Version: session.Version(), Name: "before"})
		require.NoError(t, err)
		_, aerr := w.Append(session.TypeSessionName, session.NameData{Name: "after"})
		require.NoError(t, aerr)

		assert.Equal(t, "after", sessionLabel(&sessRec{w: w}))
	})

	t.Run("nil_rec_returns_empty", func(t *testing.T) {
		assert.Empty(t, sessionLabel(nil))
	})
}

func TestExitLabel(t *testing.T) {
	t.Parallel()

	t.Run("empty_removed", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := session.Create(p, session.SessionData{Version: session.Version()})
		require.NoError(t, err)
		r := &sessRec{w: w, store: session.StoreAt(filepath.Dir(p))}

		assert.Empty(t, r.exitLabel()) // no hint may point at the dropped transcript
		_, serr := os.Stat(p)
		assert.True(t, os.IsNotExist(serr))
	})

	t.Run("named_empty_kept", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := session.Create(p, session.SessionData{Version: session.Version(), Name: "keep-me"})
		require.NoError(t, err)
		r := &sessRec{w: w, store: session.StoreAt(filepath.Dir(p))}

		assert.Equal(t, "keep-me", r.exitLabel())
	})

	t.Run("with_conversation_kept", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := session.Create(p, session.SessionData{Version: session.Version()})
		require.NoError(t, err)
		_, aerr := w.Append(session.TypeMessage, session.MessageData{
			Message: llm.Text(llm.RoleUser, "hello"),
		})
		require.NoError(t, aerr)
		r := &sessRec{w: w, store: session.StoreAt(filepath.Dir(p))}

		assert.Equal(t, sessionLabel(r), r.exitLabel())
	})

	t.Run("nil_rec_no_label", func(t *testing.T) {
		var r *sessRec
		assert.Empty(t, r.exitLabel())
	})

	t.Run("nil_store_never_dropped", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := session.Create(p, session.SessionData{Version: session.Version()})
		require.NoError(t, err)
		r := &sessRec{w: w}

		assert.Equal(t, rootID(mustEntries(t, w)), r.exitLabel())
		_, serr := os.Stat(p)
		assert.False(t, os.IsNotExist(serr))
	})
}

func TestSearchItems(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	got := searchItems([]session.Prompt{
		{Text: "fix retry", At: at},
		{Text: "line one\nline two"},
	})

	require.Len(t, got, 2)
	assert.Equal(t, "fix retry", got[0].Text)
	// recorded in UTC, rendered in the system local time zone
	assert.Equal(t, at.Local().Format("2006-01-02 15:04"), got[0].Detail)
	assert.Equal(t, "line one\nline two", got[1].Text) // multi-line prompts arrive intact
}

// rewindResolve resolves the test model key.
func rewindResolve() func(string) (llm.Model, error) {
	return func(key string) (llm.Model, error) { return llm.Model{Provider: "p", ID: "m"}, nil }
}

// textOf extracts the first text block of a message entry.
func textOf(m llm.Message) string {
	for _, b := range m.Content {
		if tb, ok := b.(llm.TextBlock); ok && tb.Text != "" {
			return tb.Text
		}
	}
	return ""
}

// TestSwitchStateSeedsBase covers a ledger built by session.State or minted for a
// new root: neither carries the constant request overhead, so without seeding it
// here a rewind drops the system prompt and tool block out of the bar entirely,
// and it stays dropped until the next turn starts.
func TestSwitchStateSeedsBase(t *testing.T) {
	t.Parallel()

	reg, warnings := llm.NewRegistry(llm.File{
		Providers: map[string]llm.ProviderConfig{"p": {Models: []llm.ModelConfig{{ID: "a"}}}},
	}, nil, llm.RegistryOptions{})
	require.Empty(t, warnings)

	p := filepath.Join(t.TempDir(), "2026-01-02T03-04-05Z-model.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version(), Model: "p/a"})
	require.NoError(t, err)

	model := llm.Model{ID: "a", Provider: "p", ContextWindow: 100000}
	st := &agent.State{Model: model, Tools: []string{tools.ToolBash}, Tokens: tokens.New(model)}
	ag := agent.New(st, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) {
			return &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textTurnRewind("hi")}}}, nil
		},
		Tools: singleToolSet{tool: noopRewindTool{}},
		Env:   agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
	})

	committed := newStartState(true)
	r := &sessRec{w: w, rec: session.NewRecorder(w), starts: committed}
	wantBase := ag.BaseEstimate(true)
	require.NotZero(t, wantBase) // the system prompt alone is never free

	// an empty head starts a new root: no messages, but the next request still
	// carries the system prompt and the committed tool block
	require.NoError(t, r.switchState(nil, ag, reg, "", rewindCauseRewind, "rewind: "))
	cs := st.Tokens.Context()
	assert.Equal(t, wantBase, cs.Used)
	assert.True(t, cs.Estimated) // a base is an estimate, so the bar says so
	assert.Equal(t, 100000, cs.Window)
}

// TestSwitchStateKeepsWindowWithoutModel covers a branch that names no model of
// its own: the rebuilt ledger has a zero window, which would rescale the bar off
// the compaction threshold onto the raw context size.
func TestSwitchStateKeepsWindowWithoutModel(t *testing.T) {
	t.Parallel()

	reg, warnings := llm.NewRegistry(llm.File{
		Providers: map[string]llm.ProviderConfig{"p": {Models: []llm.ModelConfig{{ID: "a"}}}},
	}, nil, llm.RegistryOptions{})
	require.Empty(t, warnings)

	p := filepath.Join(t.TempDir(), "2026-01-02T03-04-05Z-model.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version()}) // no model key
	require.NoError(t, err)

	model := llm.Model{ID: "a", Provider: "p", ContextWindow: 100000}
	st := &agent.State{Model: model, Tokens: tokens.New(model)}
	ag := agent.New(st, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) {
			return &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textTurnRewind("hi")}}}, nil
		},
		Env: agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
	})
	committed := newStartState(false)
	r := &sessRec{w: w, rec: session.NewRecorder(w), starts: committed}
	r.rec.Message(agent.MessageInfo{Message: llm.Text(llm.RoleUser, "hello there")})

	entries := readEntriesRewind(t, p)
	require.NoError(t, r.switchState(nil, ag, reg, entries[len(entries)-1].ID, rewindCauseRewind, "rewind: "))

	cs := st.Tokens.Context()
	assert.Equal(t, 100000, cs.Window) // framed by the live model, not left at zero
	assert.Equal(t, tokens.CompactAt(model), cs.Compact)
	assert.NotZero(t, cs.Used)
}

// heldTurnAgent builds an agent whose turns block mid-stream until their
// context ends. start runs one held turn and returns once it claims the loop.
// release ends it and is a no-op when start never ran.
func heldTurnAgent(t *testing.T) (*agent.Agent, *agent.State, func(), func()) {
	t.Helper()

	bp := &blockingProvider{
		turn:     []llm.Event{{Type: llm.EventTextDelta, Text: "held"}},
		onCreate: make(chan *blockingStream, 1),
	}
	model := llm.Model{Provider: "p", ID: "m", ContextWindow: 100000}
	st := &agent.State{Model: model, Tokens: tokens.New(model)}
	ag := agent.New(st, agent.Options{
		Sinks:    []agent.Sink{agent.NopSink{}},
		Provider: func(llm.Model) (llm.Provider, error) { return bp, nil },
		Env:      agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
	})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel) // the held turn always ends, even on a failed assertion
	errCh := make(chan error, 1)
	var began atomic.Bool
	start := func() {
		began.Store(true)
		go func() { errCh <- ag.Prompt(ctx, agent.Input{Text: "held"}) }()
		// start may run off the test goroutine (a switch hook): a stall fails
		// via the test timeout rather than t.Fatal on a foreign goroutine
		<-bp.onCreate
		assert.Eventually(t, func() bool { return ag.Running() }, 5*time.Second, 5*time.Millisecond)
	}
	release := func() {
		if !began.Load() {
			return
		}
		cancel()
		require.NoError(t, <-errCh)
	}
	return ag, st, start, release
}

// holdTurn starts a blocked turn on a fresh agent, holding it mid-stream so
// WithState refuses until the returned release ends it.
func holdTurn(t *testing.T) (*agent.Agent, *agent.State, func()) {
	t.Helper()

	ag, st, start, release := heldTurnAgent(t)
	start()
	return ag, st, release
}

func TestSwitchStateRefusedMidTurn(t *testing.T) {
	t.Parallel()

	reg, warnings := llm.NewRegistry(llm.File{
		Providers: map[string]llm.ProviderConfig{"p": {Models: []llm.ModelConfig{{ID: "a"}}}},
	}, nil, llm.RegistryOptions{})
	require.Empty(t, warnings)

	ui, _, out := plainUI(t)
	p := filepath.Join(t.TempDir(), "2026-01-02T03-04-05Z-model.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	_, err = w.Append(session.TypeMessage,
		session.MessageData{Message: llm.Text(llm.RoleUser, "hello")})
	require.NoError(t, err)

	var switched bool
	var resumed bool
	r := &sessRec{
		w: w, rec: session.NewRecorder(w),
		onSwitch:   func([]llm.Message, string) { switched = true },
		resumeAuto: func() { resumed = true },
	}

	entries := readEntriesRewind(t, p)
	ag, st, release := holdTurn(t)

	err = r.switchState(wrapUI(t, ui), ag, reg, entries[len(entries)-1].ID, rewindCauseRewind, "rewind: ")

	require.ErrorIs(t, err, errTurnRunning)
	assert.Equal(t, entries[len(entries)-1].ID, w.Head()) // the head never moved
	assert.False(t, switched)                             // callbacks never fired
	assert.False(t, resumed)
	assert.False(t, slices.ContainsFunc(st.Messages, func(m llm.Message) bool {
		return textOf(m) == "hello" // the branch context never landed
	}))
	require.Eventually(t, func() bool {
		return strings.Contains(out(), "rewind: a turn is running")
	}, 2*time.Second, 5*time.Millisecond)

	release()
	require.NoError(t, r.switchState(wrapUI(t, ui), ag, reg, entries[len(entries)-1].ID, rewindCauseRewind, "rewind: "))
	assert.True(t, switched) // the retry fires what the refusal held back
	assert.True(t, resumed)
	assert.Equal(t, entries[len(entries)-1].ID, w.Head())
}

func TestRestoreForkModelRefusedMidTurn(t *testing.T) {
	t.Parallel()

	reg, warnings := llm.NewRegistry(llm.File{
		Providers: map[string]llm.ProviderConfig{"p": {Models: []llm.ModelConfig{{ID: "a"}, {ID: "b"}}}},
	}, nil, llm.RegistryOptions{})
	require.Empty(t, warnings)

	ui, _, _ := plainUI(t)
	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	r := &sessRec{w: w, rec: session.NewRecorder(w)}

	m, err := reg.Resolve("p/b")
	require.NoError(t, err)
	ag, st, release := holdTurn(t)

	err = r.restoreForkModel(wrapUI(t, ui), ag, reg, m)
	require.ErrorIs(t, err, errTurnRunning)
	assert.Equal(t, "m", st.Model.ID) // the live model is untouched

	release()
	require.NoError(t, r.restoreForkModel(wrapUI(t, ui), ag, reg, m))
	assert.Equal(t, "b", st.Model.ID)
}

func TestLiveModelRefusedMidTurn(t *testing.T) {
	t.Parallel()

	ag, _, release := holdTurn(t)
	r := &sessRec{}

	m, ok := r.liveModel(ag)
	release()

	assert.False(t, ok)
	assert.Zero(t, m)
}

func TestRewindCaptureRefusedMidTurn(t *testing.T) {
	t.Parallel()

	reg, warnings := llm.NewRegistry(llm.File{
		Providers: map[string]llm.ProviderConfig{"p": {Models: []llm.ModelConfig{{ID: "a"}}}},
	}, nil, llm.RegistryOptions{})
	require.Empty(t, warnings)

	ui, inW, out := plainUI(t)
	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version(), Model: "p/a"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	r := &sessRec{w: w, rec: session.NewRecorder(w)}
	_, err = w.Append(session.TypeMessage,
		session.MessageData{Message: llm.Text(llm.RoleUser, "one")})
	require.NoError(t, err)
	head := w.Head()
	before := out()

	ag, _, start, release := heldTurnAgent(t)
	start() // claim the loop first, so the answered picker always refuses at capture
	rewound := make(chan struct{})
	go func() {
		defer close(rewound)
		rewindPicked(t, r, wrapUI(t, ui), ag, reg)
	}()
	// one exclusive filter answers the picker; readLines queues it until the
	// picker opens, so ordering against start above stays deterministic
	_, err = io.WriteString(inW, "one\n")
	require.NoError(t, err)
	<-rewound
	release()

	assert.Equal(t, head, w.Head()) // nothing moved
	require.Eventually(t, func() bool {
		return strings.Contains(out(), "rewind: a turn is running")
	}, 2*time.Second, 5*time.Millisecond)
	assert.True(t, strings.HasSuffix(out(), before)) // the screen never redrew
}

func TestRewindRefusedAfterSwitch(t *testing.T) {
	t.Parallel()

	reg, warnings := llm.NewRegistry(llm.File{
		Providers: map[string]llm.ProviderConfig{"p": {Models: []llm.ModelConfig{{ID: "a"}, {ID: "b"}}}},
	}, nil, llm.RegistryOptions{})
	require.Empty(t, warnings)

	ui, inW, out := plainUI(t)
	p := filepath.Join(t.TempDir(), "2026-01-02T03-04-05Z-model.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version(), Model: "p/a"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	_, err = w.Append(session.TypeMessage,
		session.MessageData{Message: llm.Text(llm.RoleUser, "one")})
	require.NoError(t, err)
	entries := readEntriesRewind(t, p)
	sessionHead := entries[0].ID // rewinding onto "one" sets the head before it

	b, err := reg.Resolve("p/b")
	require.NoError(t, err)

	var discarded bool
	ag, st, start, release := heldTurnAgent(t)
	ag.WithState(func(s *agent.State) { s.Model = b }) // the live switch the rewind keeps
	// discardStaged fires between the head move and the restore: land the turn there
	land := start
	r := &sessRec{w: w, rec: session.NewRecorder(w), discardStaged: func() {
		discarded = true
		land()
	}}
	_, err = io.WriteString(inW, "one\n")
	require.NoError(t, err)
	rewound := make(chan struct{})
	go func() {
		defer close(rewound)
		rewindPicked(t, r, wrapUI(t, ui), ag, reg)
	}()
	<-rewound

	assert.Equal(t, sessionHead, w.Head()) // the move stands
	assert.True(t, discarded)              // the abandoned branch's staging never rides
	require.Eventually(t, func() bool {
		return strings.Contains(out(), "rewind: branch switched but a turn is running, model p/b not kept")
	}, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, "a", st.Model.ID) // the kept branch's model, the b restore never landed

	release()
	// a later rewind lands once the loop is free: the restore re-applies b, which
	// only it can set after the switch rebuilt the branch's own a
	land = func() {} // the loop stays free for the retry
	ag.WithState(func(s *agent.State) { s.Model = b })
	_, err = io.WriteString(inW, "one\n")
	require.NoError(t, err)
	rewound = make(chan struct{})
	go func() {
		defer close(rewound)
		rewindPicked(t, r, wrapUI(t, ui), ag, reg)
	}()
	<-rewound
	assert.Equal(t, "b", st.Model.ID) // the restore landed on the retry
}

// rewindPicked runs the interactive rewind composition: answer the picker,
// switch onto its choice. The driver's SetOnRewind body, shared with tests.
func rewindPicked(t *testing.T, r *sessRec, ui *hubUI, ag *agent.Agent, reg *llm.Registry) {
	t.Helper()
	head, ok := r.pickRewind(ui)
	if !ok {
		return
	}
	_, _ = r.rewindTo(ui, ag, reg, head, rewindCauseRewind)
}

// rewindRig wires the driver's dispatch shape over one real transcript: a
// recorder-landed agent, a steer queue, and a pump dispatching through a state
// lane beside the rewind that holds it. prompts is how many scripted replies
// the provider carries.
type rewindRig struct {
	rec  *sessRec
	lane *stateLane
	ag   *agent.Agent
	st   *agent.State
	reg  *llm.Registry
	q    *steerQueue
	pump chan pumpLine
	// send queues one stamped line, the driver's own sender
	send func(pumpLine)
	prov *llm.ScriptedProvider
	ui   *hubUI
	log  *emitLog
}

func newRewindRig(t *testing.T, prompts int) *rewindRig {
	t.Helper()

	reg, warnings := llm.NewRegistry(llm.File{
		Providers: map[string]llm.ProviderConfig{"p": {Models: []llm.ModelConfig{{ID: "a"}}}},
	}, nil, llm.RegistryOptions{})
	require.Empty(t, warnings)

	term := newPumpTestUI(t)
	ui := wrapUI(t, term)
	log := &emitLog{}
	log.attach(ui.hub)

	turns := make([]llm.ScriptedTurn, prompts)
	for i := range turns {
		turns[i] = llm.ScriptedTurn{Events: textTurnRewind("reply " + strconv.Itoa(i))}
	}
	prov := &llm.ScriptedProvider{Turns: turns}

	w, err := session.Create(filepath.Join(t.TempDir(), "2026-01-02T03-04-05Z-race.jsonl"),
		session.SessionData{Version: session.Version(), Model: "p/a"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	rec := &sessRec{w: w, rec: session.NewRecorder(w), lane: newStateLane()}
	rec.onSwitch = func([]llm.Message, string) {
		ui.hub.setHead(w.Head(), rewindCauseRewind) // the driver's wiring
	}

	model, err := reg.Resolve("p/a")
	require.NoError(t, err)
	st := &agent.State{Model: model, Tokens: tokens.New(model)}
	ag := agent.New(st, agent.Options{
		Provider:  func(llm.Model) (llm.Provider, error) { return prov, nil },
		Sinks:     []agent.Sink{rec.rec.Sink(agent.NopSink{})},
		Env:       agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
		OnMessage: []func(agent.MessageInfo){rec.rec.Message},
	})

	fake := &fakeQueueUI{}
	waitReady, takeReady := stagePair(t)
	q := newSteerQueue(fake, func(int) {}, func() {}, waitReady, takeReady, nil)

	pump := make(chan pumpLine, 4)
	lane := rec.lane
	send := func(line pumpLine) {
		line.gen = lane.stamp()
		pump <- line
	}
	console := &uiConsole{ui: ui, commands: command.NewRegistry()}
	expander := refs.NewExpander(nil, agent.NopSink{}, tools.PathPolicy{Cwd: t.TempDir()}, nil)
	starts := newStartState(false)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var drain drainWaiter
	go runPump(ctx, pump, ag, console, command.NewStager(ctx, nil, nil), expander,
		true, ui, starts, func() {}, q, lane, nil, st, nil, &sync.Once{}, func() {}, planHooks{}, &drain)

	// a seed exchange the rewinds fork away from, present in live state too
	// the way a resumed session's rebuild leaves it
	writeMessage(t, rec, "one")
	writeMessage(t, rec, "reply one")
	seedEntries := mustEntries(t, w)
	seedState, _ := session.State(session.Branch(seedEntries, w.Head()), modelResolver(reg))
	st.Messages = seedState.Messages

	rig := &rewindRig{rec: rec, lane: lane, ag: ag, st: st, reg: reg, q: q,
		pump: pump, send: send, prov: prov, ui: ui, log: log}
	rec.busy = func() bool { return ag.Running() || q.drainLive() }
	return rig
}

// branchTexts lists the current branch's user and assistant message texts, so
// assertions can speak in prompt terms rather than entry ids.
func branchTexts(t *testing.T, w *session.Writer) []string {
	t.Helper()

	entries, _, err := session.Read(w.Path())
	require.NoError(t, err)
	var out []string
	for _, e := range session.Branch(entries, w.Head()) {
		if e.Type != session.TypeMessage {
			continue
		}
		var md session.MessageData
		require.NoError(t, json.Unmarshal(e.Data, &md))
		out = append(out, textOf(md.Message))
	}
	return out
}

// TestRewindHoldsLaneAgainstDispatch pins the closed swap-then-SetHead window:
// while a rewind holds the state lane mid-transaction, a concurrent dispatch
// cannot start a turn, and the line it tried to dispatch — queued against the
// branch the rewind forked away from — comes back instead of landing.
func TestRewindHoldsLaneAgainstDispatch(t *testing.T) {
	t.Parallel()

	rig := newRewindRig(t, 4)
	entries := mustEntries(t, rig.rec.w)
	branch := session.Branch(entries, rig.rec.w.Head())
	target := branch[1].ID // the seed user prompt
	before := rig.rec.w.Head()

	// the busy hook is the deterministic stall: the rewind parks inside the
	// lane at its refuse check until the test releases it
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	rig.rec.busy = func() bool {
		once.Do(func() { close(entered) })
		<-release
		return false
	}

	rewound := make(chan struct{})
	go func() {
		defer close(rewound)
		_, _ = rig.rec.rewindTo(rig.ui, rig.ag, rig.reg, target, rewindCauseRewind)
	}()
	<-entered // the lane is held mid-transaction, nothing has moved

	rig.send(pumpLine{kind: command.KindPrompt, rest: "racing prompt", queueID: "stale-row"})
	// the dispatch decision waits on the lane: no model request may leave, so
	// no entry can append from the head the rewind is about to move
	require.Never(t, func() bool { return len(rig.prov.Requests()) > 0 },
		150*time.Millisecond, 10*time.Millisecond)

	close(release)
	<-rewound
	assert.NotEqual(t, before, rig.rec.w.Head()) // the switch landed

	// the rewind bumped the generation under the line's stamp, so the racing
	// prompt recovers instead of running against the switched state. Rewinding
	// onto the seed prompt drops it from context (it becomes the editor prefill)
	require.Eventually(t, func() bool {
		return len(rig.log.named(ctl.EventQueueDropped)) > 0
	}, 5*time.Second, 5*time.Millisecond)
	var qd ctl.QueueDroppedData
	require.NoError(t, rig.log.named(ctl.EventQueueDropped)[0].Decode(&qd))
	assert.Equal(t, rewindCauseRewind, qd.Cause)
	assert.Equal(t, []ctl.QueueRow{{ID: "stale-row", Text: "racing prompt"}}, qd.Rows)
	assert.Empty(t, rig.prov.Requests())
	assert.Empty(t, branchTexts(t, rig.rec.w))

	// a prompt queued after the rewind carries the fresh generation and runs
	// as the new branch's own turn, appending from its head
	rig.send(pumpLine{kind: command.KindPrompt, rest: "fresh after rewind"})
	require.Eventually(t, func() bool { return len(rig.prov.Requests()) == 1 },
		5*time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool {
		return slices.Contains(branchTexts(t, rig.rec.w), "fresh after rewind")
	}, 5*time.Second, 5*time.Millisecond)
}

// TestStaleLineWithoutCorrelationPublishesNothing pins the stale-line gate: a
// typed line stuck in the pump buffer never entered the queue, so no surface
// announced it as a row and aging it out publishes no drop event to flash.
func TestStaleLineWithoutCorrelationPublishesNothing(t *testing.T) {
	t.Parallel()

	rig := newRewindRig(t, 4)
	entries := mustEntries(t, rig.rec.w)
	target := session.Branch(entries, rig.rec.w.Head())[1].ID
	_, _ = rig.rec.rewindTo(rig.ui, rig.ag, rig.reg, target, rewindCauseRewind)

	// a line stamped before the rewind is stale at dequeue however the timing
	// lands, so the silence it must keep is deterministic
	rig.pump <- pumpLine{kind: command.KindPrompt, rest: "uncorrelated", gen: 0}
	require.Eventually(t, func() bool { return len(rig.pump) == 0 },
		5*time.Second, 5*time.Millisecond)
	assert.Empty(t, rig.log.named(ctl.EventQueueDropped))
	assert.Empty(t, rig.prov.Requests())
}

// TestRewindDispatchRaceKeepsBranch races a dispatched prompt against a rewind
// over and over: whichever wins, the live state must keep describing exactly
// the writer head's branch, the invariant a torn swap-then-SetHead window used
// to break by appending from the old head while the head moved.
func TestRewindDispatchRaceKeepsBranch(t *testing.T) {
	t.Parallel()

	const iterations = 16
	rig := newRewindRig(t, iterations+4)
	entries := mustEntries(t, rig.rec.w)
	target := session.Branch(entries, rig.rec.w.Head())[1].ID

	for i := 0; i < iterations; i++ {
		rig.send(pumpLine{kind: command.KindPrompt, rest: "race " + strconv.Itoa(i)})
		rewound := make(chan struct{})
		go func() {
			defer close(rewound)
			_, _ = rig.rec.rewindTo(rig.ui, rig.ag, rig.reg, target, rewindCauseRewind)
		}()
		<-rewound

		// settle: the drain exits and the pump holds no buffered line
		require.Eventually(t, func() bool {
			return !rig.q.drainLive() && len(rig.pump) == 0
		}, 5*time.Second, 5*time.Millisecond)

		entries := mustEntries(t, rig.rec.w)
		rebuilt, _ := session.State(session.Branch(entries, rig.rec.w.Head()), modelResolver(rig.reg))
		assert.Equal(t, rebuilt.Messages, rig.st.Messages)
	}
}

// TestRewindCleanupRunsOnRestoreRefusal pins the transaction's cleanup ordering:
// a model restore that hits a running turn returns early, and the queue clear
// (with its recorded notice) must already have run, never leaving prompts
// queued for the branch the rewind abandoned.
func TestRewindCleanupRunsOnRestoreRefusal(t *testing.T) {
	t.Parallel()

	reg, warnings := llm.NewRegistry(llm.File{
		Providers: map[string]llm.ProviderConfig{"p": {Models: []llm.ModelConfig{{ID: "a"}}}},
	}, nil, llm.RegistryOptions{})
	require.Empty(t, warnings)

	ui, _, _ := plainUI(t)
	p := filepath.Join(t.TempDir(), "2026-01-02T03-04-05Z-clean.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version(), Model: "p/a"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	_, err = w.Append(session.TypeMessage,
		session.MessageData{Message: llm.Text(llm.RoleUser, "one")})
	require.NoError(t, err)
	entries := readEntriesRewind(t, p)

	fake := &fakeQueueUI{}
	waitReady, takeReady := stagePair(t)
	q := newSteerQueue(fake, func(int) {}, func() {}, waitReady, takeReady, nil)
	q.hold(agent.Input{Text: "queued for the old branch", QueueIDs: []string{"row-9"}},
		"queued for the old branch", 0)
	// a secret row: the recorded notice names what was cleared without ever
	// writing the value, matching every other surface's redaction rule
	q.hold(agent.Input{Text: "secret: hunter2", QueueIDs: []string{"row-secret"}},
		"secret: hunter2", 0)
	require.Equal(t, 2, q.pending())

	var discarded bool
	ag, _, start, release := heldTurnAgent(t)
	r := &sessRec{w: w, rec: session.NewRecorder(w), lane: newStateLane(),
		discardStaged: func() {
			discarded = true
			start() // land the turn between the head move and the model restore
		},
		clearQueued: q.clearForRewind}

	_, err = r.rewindTo(wrapUI(t, ui), ag, reg, entries[1].ID, rewindCauseRewind)
	require.ErrorIs(t, err, errTurnRunning) // the restore refused, cleanup behind it
	release()

	assert.True(t, discarded)
	assert.Equal(t, 0, q.pending()) // the clear ran despite the refusal

	after := readEntriesRewind(t, p)
	noticed := bulk.SliceFilter(func(e session.Entry) bool { return e.Type == session.TypeNotice }, after)
	require.Len(t, noticed, 1)
	// the notice chains from the switched head, so it replays on the new branch
	assert.Equal(t, entries[0].ID, noticed[0].ParentID)
	var nd session.NoticeData
	require.NoError(t, noticed[0].Decode(&nd))
	assert.Contains(t, nd.Message, "queued for the old branch")
	assert.NotContains(t, nd.Message, "hunter2") // the secret stays off the record
	assert.Equal(t, noticed[0].ID, w.Head())
}

// syncBuffer guards one strings.Builder for the plainUI drain goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.Write(p)
}

// String snapshots what the UI has painted so far.
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.String()
}

// plainUI opens a plain-mode UI whose painted output drains into a buffer, so
// notifications are assertable. The returned writer feeds its input, so a test
// can answer the prompts plain mode reads; the last func snapshots the output.
func plainUI(t *testing.T) (*tui.UI, io.Writer, func() string) {
	t.Helper()

	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
	})
	ui, err := tui.New(tui.Options{In: inR, Out: outW, Mode: tui.ModePlain})
	require.NoError(t, err)
	t.Cleanup(ui.Close)

	buf := &syncBuffer{}
	go func() { _, _ = io.Copy(buf, outR) }()
	return ui, inW, buf.String
}

func TestRestoreStateReadFailure(t *testing.T) {
	t.Parallel()

	t.Run("unreadable_warns", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := session.Create(p, session.SessionData{Version: session.Version()})
		require.NoError(t, err)
		t.Cleanup(func() { _ = w.Close() })
		r := &sessRec{w: w}
		// the flaky mount drops the file Open just succeeded on
		require.NoError(t, os.Remove(p))

		var st agent.State
		branch, warns := r.restoreState(nil, nil, &st, nil)
		assert.Nil(t, branch)
		require.Len(t, warns, 1)
		assert.Contains(t, warns[0], "transcript unreadable")
	})

	t.Run("empty_stays_silent", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := session.Create(p, session.SessionData{Version: session.Version()})
		require.NoError(t, err)
		t.Cleanup(func() { _ = w.Close() })
		r := &sessRec{w: w}
		require.NoError(t, os.Truncate(p, 0)) // readable but holds no lines

		var st agent.State
		branch, warns := r.restoreState(nil, nil, &st, nil)
		assert.Nil(t, branch)
		assert.Empty(t, warns)
	})
}

func TestRebuildWarnsUnreadable(t *testing.T) {
	t.Parallel()

	ui, _, out := plainUI(t)
	reg, warnings := llm.NewRegistry(llm.File{
		Providers: map[string]llm.ProviderConfig{"p": {Models: []llm.ModelConfig{{ID: "a"}}}},
	}, nil, llm.RegistryOptions{})
	require.Empty(t, warnings)

	p := filepath.Join(t.TempDir(), "2026-01-02T03-04-05Z-lost.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	set, _, err := config.Load(config.Options{
		Workspace: t.TempDir(),
		Env:       func(string) string { return "" },
	})
	require.NoError(t, err)

	st := &agent.State{Model: reg.Active(), Tokens: tokens.New(reg.Active())}
	require.NoError(t, os.Remove(p))
	(&sessRec{w: w}).rebuild(set, wrapUI(t, ui), reg, st, nil)

	require.Eventually(t, func() bool {
		return strings.Contains(out(), "resume: transcript unreadable")
	}, 2*time.Second, 5*time.Millisecond)
}

func TestRewindWarnsUnreadable(t *testing.T) {
	t.Parallel()

	ui, _, out := plainUI(t)
	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	r := &sessRec{w: w}
	require.NoError(t, os.Remove(p))

	r.pickRewind(wrapUI(t, ui)) // the failure returns before the agent or registry are touched

	require.Eventually(t, func() bool {
		return strings.Contains(out(), "rewind: ")
	}, 2*time.Second, 5*time.Millisecond)
	assert.NotContains(t, out(), "nothing to rewind onto yet")
}

func TestNewSessionSeedsSnapshot(t *testing.T) {
	// t.Chdir pins config.Cwd, so this is sequential
	wss := t.TempDir()
	t.Setenv("AJENT_HOME", filepath.Join(wss, "home")) // newSession's store stays test-local
	t.Chdir(wss)

	store, err := session.NewStore()
	require.NoError(t, err)

	seed, err := store.Create(wss, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	for _, text := range []string{"one", "two"} {
		_, aerr := seed.Append(session.TypeMessage,
			session.MessageData{Message: llm.Text(llm.RoleUser, text)})
		require.NoError(t, aerr)
	}
	require.NoError(t, seed.Close())

	list, lerr := store.List(wss)
	require.NoError(t, lerr)
	require.Len(t, list, 1)

	set, _, cerr := config.Load(config.Options{Workspace: wss})
	require.NoError(t, cerr)
	reg, _ := llm.NewRegistry(llm.File{}, nil, llm.RegistryOptions{})

	rec, nerr := newSession(nil, nil, ResumeID, list[0].ID, "", session.Info{})
	require.NoError(t, nerr)
	t.Cleanup(func() { _ = rec.w.Close() })

	// the open-time parse rides on the record and the hint answers from it
	want, _, rerr := session.Read(seed.Path())
	require.NoError(t, rerr)
	require.NotEmpty(t, rec.snapshot.entries)
	assert.Equal(t, want, rec.snapshot.entries)
	assert.Equal(t, rootID(want), rec.hint)
	assert.Equal(t, rec.hint, sessionHint(rec))

	// the startup rebuild consumes the snapshot and restores its messages
	st := &agent.State{Tokens: tokens.New(llm.Model{})}
	branch, warns := rec.restoreState(set, reg, st, nil)
	require.Empty(t, warns)
	assert.Equal(t, snapshot{}, rec.snapshot)
	assert.Equal(t, want, branch) // a linear transcript: the branch is every entry
	assert.Len(t, st.Messages, 2)
	assert.Equal(t, rec.hint, sessionHint(rec)) // the root id outlives the snapshot
}

func TestResumeSurfacesSnapshotWarnings(t *testing.T) {
	// t.Chdir pins config.Cwd, so this is sequential
	wss := t.TempDir()
	t.Setenv("AJENT_HOME", filepath.Join(wss, "home"))
	t.Chdir(wss)

	store, err := session.NewStore()
	require.NoError(t, err)

	seed, err := store.Create(wss, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	_, aerr := seed.Append(session.TypeMessage,
		session.MessageData{Message: llm.Text(llm.RoleUser, "hello")})
	require.NoError(t, aerr)
	require.NoError(t, seed.Close())

	// a corrupt middle line warns on resume exactly as a fresh Read would
	f, err := os.OpenFile(seed.Path(), os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString("{not json}\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	list, lerr := store.List(wss)
	require.NoError(t, lerr)
	require.Len(t, list, 1)

	set, _, cerr := config.Load(config.Options{Workspace: wss})
	require.NoError(t, cerr)
	reg, _ := llm.NewRegistry(llm.File{}, nil, llm.RegistryOptions{})

	rec, nerr := newSession(nil, nil, ResumeID, list[0].ID, "", session.Info{})
	require.NoError(t, nerr)
	t.Cleanup(func() { _ = rec.w.Close() })

	st := &agent.State{Tokens: tokens.New(llm.Model{})}
	_, warns := rec.restoreState(set, reg, st, nil)
	assert.NotEmpty(t, warns)
}

func TestResumeSeesConcurrentAppend(t *testing.T) {
	// t.Chdir pins config.Cwd, so this is sequential
	wss := t.TempDir()
	t.Setenv("AJENT_HOME", filepath.Join(wss, "home"))
	t.Chdir(wss)

	store, err := session.NewStore()
	require.NoError(t, err)

	seed, err := store.Create(wss, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	_, aerr := seed.Append(session.TypeMessage,
		session.MessageData{Message: llm.Text(llm.RoleUser, "mine")})
	require.NoError(t, aerr)
	require.NoError(t, seed.Close())

	list, lerr := store.List(wss)
	require.NoError(t, lerr)
	require.Len(t, list, 1)

	set, _, cerr := config.Load(config.Options{Workspace: wss})
	require.NoError(t, cerr)
	reg, _ := llm.NewRegistry(llm.File{}, nil, llm.RegistryOptions{})

	rec, nerr := newSession(nil, nil, ResumeID, list[0].ID, "", session.Info{})
	require.NoError(t, nerr)
	t.Cleanup(func() { _ = rec.w.Close() })
	require.NotEmpty(t, rec.snapshot.entries)

	// a second ajent on the same transcript appends after our open-time parse,
	// then a corrupt line lands behind it
	other, oerr := session.Open(seed.Path())
	require.NoError(t, oerr)
	_, aerr = other.Append(session.TypeMessage,
		session.MessageData{Message: llm.Text(llm.RoleUser, "theirs")})
	require.NoError(t, aerr)
	require.NoError(t, other.Close())
	f, ferr := os.OpenFile(seed.Path(), os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, ferr)
	_, werr := f.WriteString("{not json}\n")
	require.NoError(t, werr)
	require.NoError(t, f.Close())

	st := &agent.State{Tokens: tokens.New(llm.Model{})}
	branch, warns := rec.restoreState(set, reg, st, nil)
	// the rebuild must notice the growth and re-read: only a live parse sees the
	// unparseable tail, so a stale-snapshot rebuild would restore silently
	assert.NotEmpty(t, warns)
	require.NotEmpty(t, branch)
	// the restored branch follows this writer's head (the last-writer-wins
	// cursor), leaving theirs one rewind away
	assert.False(t, slices.ContainsFunc(branch, func(e session.Entry) bool {
		return e.ID == other.Head()
	}))
	assert.True(t, slices.ContainsFunc(st.Messages, func(m llm.Message) bool {
		return m.Role == llm.RoleUser && llm.FirstBlockText(m.Content) == "mine"
	}))
}

func TestOpenSessionAdoptsPreResolution(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	store := session.StoreAt(filepath.Join(ws, "root"))
	seed, err := store.Create(ws, session.SessionData{Version: session.Version(), Name: "fix-parser"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = seed.Close() })

	list, lerr := store.List(ws)
	require.NoError(t, lerr)
	require.Len(t, list, 1)
	pre := list[0]

	t.Run("matching_pre_opens_same_path", func(t *testing.T) {
		w, _, _, err := openSession(store, ResumeID, ws, pre.ID, "", nil, pre)
		require.NoError(t, err)
		t.Cleanup(func() { _ = w.Close() })
		assert.Equal(t, seed.Path(), w.Path())

		w2, _, _, err := openSession(store, ResumeSessionName, ws, "fix-parser", "", nil, pre)
		require.NoError(t, err)
		t.Cleanup(func() { _ = w2.Close() })
		assert.Equal(t, seed.Path(), w2.Path())
	})

	t.Run("foreign_pre_is_dropped", func(t *testing.T) {
		foreign := session.StoreAt(filepath.Join(t.TempDir(), "other"))
		listed, lerr := foreign.List(ws)
		require.NoError(t, lerr)
		require.Empty(t, listed) // pre belongs to a directory this store never reads

		w, _, _, err := openSession(store, ResumeID, ws, pre.ID, "", nil, pre)
		require.NoError(t, err)
		t.Cleanup(func() { _ = w.Close() })
		assert.Equal(t, seed.Path(), w.Path())
	})

	t.Run("blocked_pre_is_dropped", func(t *testing.T) {
		pre.Blocked = true
		w, _, _, err := openSession(store, ResumeID, ws, pre.ID, "", nil, pre)
		require.NoError(t, err)
		t.Cleanup(func() { _ = w.Close() })
		assert.Equal(t, seed.Path(), w.Path())
	})
}

// mustEntries reads a transcript's entries for label assertions.
func mustEntries(t *testing.T, w *session.Writer) []session.Entry {
	t.Helper()
	entries, _, err := session.Read(w.Path())
	require.NoError(t, err)
	return entries
}

// TestRenameRaceLoser pins the create-race rule: the older session id keeps the
// name, the younger suffix-renames with a recorded session_name entry, and a
// younger rival never displaces this run's name.
func TestRenameRaceLoser(t *testing.T) {
	t.Setenv("AJENT_HOME", t.TempDir())
	store, err := session.NewStore()
	require.NoError(t, err)
	ws := t.TempDir()

	older, err := store.Create(ws, session.SessionData{Version: session.Version(), Name: "shared"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = older.Close() })
	olderID := older.Head() // the root entry id sorts by creation time
	// the younger racer mints after the older one, so its root id sorts higher
	younger, err := store.Create(ws, session.SessionData{Version: session.Version(), Name: "shared"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = younger.Close() })
	require.Greater(t, younger.Head(), olderID)

	note := renameRaceLoser(store, ws, "shared", younger)
	assert.Contains(t, note, "shared-2")

	entries, _, err := session.Read(younger.Path())
	require.NoError(t, err)
	assert.Equal(t, "shared-2", session.NameOf(entries)) // the rename entry records it

	// the older session's own data is untouched and still answers to the name
	entries, _, err = session.Read(older.Path())
	require.NoError(t, err)
	assert.Equal(t, "shared", session.NameOf(entries))

	// a younger rival: this run keeps the name, no rename happens
	note = renameRaceLoser(store, ws, "shared", older)
	assert.Empty(t, note)
}

// TestRewindClearsQueuedWithNotice pins the transaction tail: a rewind clears
// the queue instead of re-targeting, records a notice entry naming the cleared
// prompts so resume replays it, and fires the ephemeral drop event beside the
// queue update.
func TestRewindClearsQueuedWithNotice(t *testing.T) {
	rig := newControlRig(t, func(rec *sessRec) {
		writeMessage(t, rec, "first prompt")
	})
	wireAttachRewind(t, rig)
	c := dialControl(t, rig.path)
	c.hello()
	attachControl(t, c, "rewinder")

	// the rig wires no pump, so the queue stands alone with the hub's drop seam
	// and the transaction's clear seam, exactly what the driver installs
	rig.hub.q = newSteerQueue(rig.hub.ui, func(int) {}, func() {},
		func(context.Context) {}, func() []agent.MessageInfo { return nil }, rig.hub.queueDroppedCause)
	rig.rec.clearQueued = rig.hub.q.clearForRewind

	// park two prompts as rows with their correlations, the way a gate hold leaves them
	rig.hub.q.hold(agent.Input{Text: "queued one", QueueIDs: []string{"row-1"}}, "queued one", 0)
	rig.hub.q.hold(agent.Input{Text: "queued two", QueueIDs: []string{"row-2"}}, "queued two", 0)
	require.Equal(t, 2, rig.hub.q.pending())

	// rewind onto the first user message: the session entry itself is not a
	// valid target, the branch's first message is
	branch := rigBranch(t, rig.rec)
	rp := c.call("rw", ctl.MethodRewind, map[string]any{"head": branch[1].ID})
	require.Nil(t, rp.Error)

	require.Eventually(t, func() bool { return rig.hub.q.pending() == 0 },
		ctlWait, 5*time.Millisecond)

	// the notice entry naming them is on the transcript, so resume replays it
	entries, _, err := session.Read(rig.rec.w.Path())
	require.NoError(t, err)
	notices := bulk.SliceFilter(func(e session.Entry) bool {
		return e.Type == session.TypeNotice
	}, entries)
	require.NotEmpty(t, notices)
	var nd session.NoticeData
	require.NoError(t, notices[len(notices)-1].Decode(&nd))
	assert.Contains(t, nd.Message, "queued one")
	assert.Contains(t, nd.Message, "queued two")

	// the ephemeral drop event rode the same transaction, published before the
	// emptied rows in one seq window so a re-offer keeps its fresh pending row.
	// Both surface between the branch change and this read; gather without
	// discarding, since collect drops non-matching events it holds.
	var droppedEv, cleared ctl.Event
	require.Eventually(t, func() bool {
		for _, ev := range c.takePending() {
			switch {
			case ev.Name == ctl.EventQueueDropped && droppedEv.Name == "":
				droppedEv = ev
			case ev.Name == ctl.EventQueued && droppedEv.Name != "" && cleared.Name == "":
				cleared = ev // the rows update beside the drop
			}
		}
		return droppedEv.Name != "" && cleared.Name != ""
	}, ctlWait, 5*time.Millisecond)

	var qd ctl.QueueDroppedData
	require.NoError(t, droppedEv.Decode(&qd))
	assert.ElementsMatch(t,
		[]ctl.QueueRow{{ID: "row-1", Text: "queued one"}, {ID: "row-2", Text: "queued two"}}, qd.Rows)
	var cq ctl.QueuedData
	require.NoError(t, cleared.Decode(&cq))
	assert.Empty(t, cq.Rows)
	assert.Greater(t, cleared.Seq, droppedEv.Seq) // drop first, rows update beside it
}

// TestRenameSettleYieldsToOlderRival pins the rename race settlement: a rename
// landing beside an older session holding the same name suffix-renames this one
// with a notice, while a younger rival leaves the name here and says so.
func TestRenameSettleYieldsToOlderRival(t *testing.T) {
	t.Setenv("AJENT_HOME", t.TempDir())
	store, err := session.NewStore()
	require.NoError(t, err)
	ws := t.TempDir()

	older, err := store.Create(ws, session.SessionData{Version: session.Version(), Name: "shared"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = older.Close() })

	mine, err := store.Create(ws, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mine.Close() })
	rec := session.NewRecorder(mine)
	require.NoError(t, rec.Rename("shared")) // the rename that raced the older holder

	eff, note := renameSettle(store, ws, "shared", mine.Path(), rec)
	assert.Equal(t, "shared-2", eff)
	assert.Contains(t, note, "shared-2")

	entries, _, err := session.Read(mine.Path())
	require.NoError(t, err)
	assert.Equal(t, "shared-2", session.NameOf(entries))

	// a younger rival leaves the name here and reports the ambiguity instead
	mine2, err := store.Create(ws, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mine2.Close() })
	rec2 := session.NewRecorder(mine2)
	require.NoError(t, rec2.Rename("contested"))
	younger, err := store.Create(ws, session.SessionData{Version: session.Version(), Name: "contested"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = younger.Close() })
	require.Greater(t, younger.Head(), mine2.Head())

	eff, note = renameSettle(store, ws, "contested", mine2.Path(), rec2)
	assert.Equal(t, "contested", eff)
	assert.Contains(t, note, "ambiguous")
}

func TestCheckSessionTarget(t *testing.T) {
	ws := t.TempDir()
	t.Chdir(ws)
	t.Setenv("AJENT_HOME", t.TempDir())

	store, err := session.NewStore()
	require.NoError(t, err)
	w, cerr := store.Create(ws, session.SessionData{Version: session.Version(), Name: "fix-parser"})
	require.NoError(t, cerr)
	id := w.Head()
	require.NoError(t, w.Close())

	t.Run("resume_by_name", func(t *testing.T) {
		_, err := CheckSessionTarget(ResumeID, "fix-parser")
		assert.NoError(t, err)
	})

	t.Run("resume_by_id", func(t *testing.T) {
		_, err = CheckSessionTarget(ResumeID, id)
		assert.NoError(t, err)
	})

	t.Run("resume_unknown_fails", func(t *testing.T) {
		_, err = CheckSessionTarget(ResumeID, "no-such-session")
		assert.Error(t, err)
	})

	t.Run("new_name_is_allowed", func(t *testing.T) {
		_, err = CheckSessionTarget(ResumeSessionName, "brand-new")
		assert.NoError(t, err)
	})

	t.Run("existing_name_is_allowed", func(t *testing.T) {
		_, err = CheckSessionTarget(ResumeSessionName, "fix-parser")
		assert.NoError(t, err)
	})

	t.Run("name_matching_id_fails", func(t *testing.T) {
		_, err = CheckSessionTarget(ResumeSessionName, id)
		assert.Error(t, err)
	})
}
