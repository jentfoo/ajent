package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestRewindStateRebuild drives a transcript, rewinds onto an earlier message,
// and verifies the rebuilt agent state carries exactly that branch's context —
// the heart of "double-Esc opens the context tree".
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
	a := agent.New(&agent.State{Model: llm.Model{ID: "p/m"}, Tools: []string{tools.ToolBash}}, agent.Options{
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

	// rewind onto the *first user message* (index 1): context is just that prompt,
	// before its assistant reply — rewinding drops everything after the pick.
	st, _ := r.stateFor(rewindResolve(), entries, branch[1].ID)
	require.Len(t, st.Messages, 1)
	assert.Equal(t, llm.RoleUser, st.Messages[0].Role)
	assert.Equal(t, "one", textOf(st.Messages[0]))

	// rewinding onto the tip keeps the full chain.
	stTip, _ := r.stateFor(rewindResolve(), entries, branch[len(branch)-1].ID)
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

	// both branches are present: the old tip and the new one.
	ids := make([]string, len(tree))
	for i, tr := range tree {
		ids[i] = tr.ID
	}
	assert.Contains(t, ids, tipOld)
	assert.Contains(t, ids, forkA.ID)

	// only the new chain is active; the abandoned old reply is not.
	assert.False(t, slices.ContainsFunc(tree, func(tr session.TreeRow) bool {
		return tr.ID == tipOld && tr.Active
	}))
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

// TestRewindKeepsCurrentModel locks in that a rewind onto an earlier message does
// not silently revert the active model to whatever produced it: /model then fork
// must keep the switched-to model when that prior message is re-sent.
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

	// grow a chain on model a.
	a := agent.New(&agent.State{Model: llm.Model{ID: "a"}, Tools: []string{tools.ToolBash}}, agent.Options{
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

	// switch to b live and record it, as /model does.
	bModel, err := reg.Resolve("p/b")
	require.NoError(t, err)
	a.WithState(func(st *agent.State) { st.Model = bModel })
	r.rec.ModelChange(bModel, "command")

	// rewind onto the first user message and restore the fork model: it must stay b.
	save := r.liveModel(a)
	require.NoError(t, r.switchState(nil, a, reg, branch[1].ID, "rewind: "))
	r.restoreForkModel(nil, a, reg, save)

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

	// picking the assistant reply keeps that message as its own head with no pre-fill.
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

// TestOpenSessionModes locks in how --continue/--resume/no-flag choose a
// transcript: new always makes one even when sessions exist; continue and resume
// (via pick) reuse an existing one.
func TestOpenSessionModes(t *testing.T) {
	t.Parallel()
	ws := t.TempDir() + "/workspace"
	require.NoError(t, os.MkdirAll(ws, 0o700))
	store := session.StoreAt(filepath.Join(t.TempDir(), "root"))

	// seed one saved transcript so resume/continue have something to reuse.
	seed, err := store.Create(ws, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	firstPath := seed.Path()

	// capture the seed's id while it is still the only session; a later
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
	// session; ResumeNewSession and the cancelled resume each create a newer file,
	// so they come last to keep "latest" pointing at the seed above.

	// --continue: reuse the most recent transcript (the seed, still alone).
	wCont, err := openSession(store, ResumeContinue, ws, "", "", pickFirst)
	require.NoError(t, err)
	assert.Equal(t, firstPath, wCont.Path())

	// --resume with a selection: reuse that root's transcript.
	wPick, err := openSession(store, ResumePick, ws, "", "", pickFirst)
	require.NoError(t, err)
	assert.Equal(t, firstPath, wPick.Path())

	// --resume <id>: reuse that exact saved transcript directly.
	wID, err := openSession(store, ResumeID, ws, seedID, "", pickFirst)
	require.NoError(t, err)
	assert.Equal(t, firstPath, wID.Path())

	// no flag: always a fresh file, never reuses.
	wNew, err := openSession(store, ResumeNewSession, ws, "", "", pickFirst)
	require.NoError(t, err)
	assert.NotEqual(t, firstPath, wNew.Path())

	// --resume cancelled (ErrCancelled): start fresh rather than stall.
	wCancel, err := openSession(store, ResumePick, ws, "", "",
		func([]session.Info) (int, error) { return 0, tui.ErrCancelled })
	require.NoError(t, err)
	assert.NotEqual(t, firstPath, wCancel.Path())
}

func TestOpenSessionContinueRewound(t *testing.T) {
	t.Parallel()
	ws := t.TempDir() + "/workspace"
	require.NoError(t, os.MkdirAll(ws, 0o700))
	store := session.StoreAt(filepath.Join(t.TempDir(), "root"))

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

	w, err := openSession(store, ResumeContinue, ws, "", "", nil)
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

// TestOpenSessionByName locks in --session: an unknown name creates a transcript
// carrying it, and the same name reopens that transcript rather than a new one.
func TestOpenSessionByName(t *testing.T) {
	t.Parallel()
	ws := t.TempDir() + "/workspace"
	require.NoError(t, os.MkdirAll(ws, 0o700))
	store := session.StoreAt(filepath.Join(t.TempDir(), "root"))

	created, err := openSession(store, ResumeSessionName, ws, "fix-parser", "p/a", nil)
	require.NoError(t, err)
	require.NoError(t, created.Close())

	entries, _, rerr := session.Read(created.Path())
	require.NoError(t, rerr)
	assert.Equal(t, "fix-parser", session.NameOf(entries))

	// the same name reopens it; a different one starts its own transcript
	resumed, err := openSession(store, ResumeSessionName, ws, "fix-parser", "p/a", nil)
	require.NoError(t, err)
	assert.Equal(t, created.Path(), resumed.Path())

	other, err := openSession(store, ResumeSessionName, ws, "other", "p/a", nil)
	require.NoError(t, err)
	assert.NotEqual(t, created.Path(), other.Path())

	// a name is reachable through --resume too
	byResume, err := openSession(store, ResumeID, ws, "fix-parser", "", nil)
	require.NoError(t, err)
	assert.Equal(t, created.Path(), byResume.Path())

	// a name that only reaches a session by id is refused rather than resuming one
	// the user never named; the guard lives in openSession, not just its caller
	list, lerr := store.List(ws)
	require.NoError(t, lerr)
	require.NotEmpty(t, list)
	_, err = openSession(store, ResumeSessionName, ws, list[0].ID, "p/a", nil)
	assert.ErrorContains(t, err, "matches session id")
}

// TestResumeByID verifies --resume <id> reopens exactly that saved transcript by
// its root id (not the picker), and that an unknown id is reported as not found.
func TestResumeByID(t *testing.T) {
	t.Parallel()
	ws := t.TempDir() + "/w"
	require.NoError(t, os.MkdirAll(ws, 0o700))
	store := session.StoreAt(filepath.Join(t.TempDir(), "root"))

	// two distinct saved sessions; resume the older one by id.
	_, err := store.Create(ws, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	older, err := store.Create(ws, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	list, err := store.List(ws)
	require.NoError(t, err)
	require.Len(t, list, 2)

	// resolve the saved info for that older file so we can address it by id.
	var want session.Info
	for _, in := range list {
		if in.Path == older.Path() {
			want = in
		}
	}
	require.NotEmpty(t, want.ID)

	// the picker's chosen index maps to that exact root among several.
	for i, in := range list {
		if in.Path != older.Path() {
			continue
		}
		wPick, err := openSession(store, ResumePick, ws, "", "",
			func([]session.Info) (int, error) { return i, nil })
		require.NoError(t, err)
		assert.Equal(t, older.Path(), wPick.Path())
	}

	w, err := openSession(store, ResumeID, ws, want.ID, "", nil)
	require.NoError(t, err)
	assert.Equal(t, older.Path(), w.Path())

	// a bogus id resolves to ErrNotFound.
	_, err = openSession(store, ResumeID, ws, "NO-SUCH-ID", "", nil)
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

	st := &agent.State{Model: reg.Active()}
	(&sessRec{w: w}).rebuild(set, ui, reg, st, nil)

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

	dir := t.TempDir()
	p := filepath.Join(dir, "2026-01-02T03-04-05Z-model.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version(), Model: "p/a"})
	require.NoError(t, err)

	// a couple of messages so the branch is non-empty, then a threshold override.
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

	st := &agent.State{Model: reg.Active()}
	(&sessRec{w: w}).rebuild(set, ui, reg, st, nil)

	// the session override 0.5 survives resume onto a model with no declared one,
	// so the trigger and the bar read it rather than the global default.
	require.NotNil(t, st.Tokens)
	cs := st.Tokens.Context()
	at := tokens.CompactAt(st.Model)
	assert.InDelta(t, 0.5, st.Model.CompactThreshold, 1e-9)
	assert.Equal(t, at, cs.Compact) // the ledger's compact term follows the resumed threshold
}

func TestSubmittedEcho(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"prompt_echoes", "refactor the parser", "refactor the parser"},
		{"blank_prompt_silent", "   ", ""},
		{"command_not_echoed", "/model", ""},
		{"shell_not_echoed", "!git status", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, submittedEcho(tc.in))
		})
	}
}

func TestSessionHint(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version()})
	require.NoError(t, err)

	r := &sessRec{w: w}
	hint := sessionHint(r)
	assert.NotEmpty(t, hint, "a recorded transcript must expose its resume id")

	// nil / unwired records yield no hint.
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

func TestEmptyReportsNoConversation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	s := session.StoreAt(filepath.Dir(p))
	r := &sessRec{w: w, store: s}

	// a brand-new transcript with only its root entry is empty.
	assert.True(t, r.empty())

	// any message (even just the prompt) makes it worth keeping.
	_, aerr := w.Append(session.TypeMessage, session.MessageData{
		Message: llm.Text(llm.RoleUser, "hello"),
	})
	require.NoError(t, aerr)
	assert.False(t, r.empty())

	// an unrecorded run (nil store) is never dropped.
	assert.False(t, (&sessRec{w: w}).empty())

	// a named session is kept even with no conversation: --session resumes by name.
	named := filepath.Join(t.TempDir(), "named.jsonl")
	nw, nerr := session.Create(named, session.SessionData{Version: session.Version(), Name: "keep-me"})
	require.NoError(t, nerr)
	assert.False(t, (&sessRec{w: nw, store: session.StoreAt(filepath.Dir(named))}).empty())
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

	committed := true
	r := &sessRec{w: w, rec: session.NewRecorder(w), started: &committed}
	wantBase := ag.BaseEstimate(true)
	require.NotZero(t, wantBase) // the system prompt alone is never free

	// an empty head starts a new root: no messages, but the next request still
	// carries the system prompt and the committed tool block
	require.NoError(t, r.switchState(nil, ag, reg, "", "rewind: "))
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
	var committed bool
	r := &sessRec{w: w, rec: session.NewRecorder(w), started: &committed}
	r.rec.Message(agent.MessageInfo{Message: llm.Text(llm.RoleUser, "hello there")})

	entries := readEntriesRewind(t, p)
	require.NoError(t, r.switchState(nil, ag, reg, entries[len(entries)-1].ID, "rewind: "))

	cs := st.Tokens.Context()
	assert.Equal(t, 100000, cs.Window) // framed by the live model, not left at zero
	assert.Equal(t, tokens.CompactAt(model), cs.Compact)
	assert.NotZero(t, cs.Used)
}
