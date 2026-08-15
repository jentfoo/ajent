package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	a := agent.New(&agent.State{Model: llm.Model{ID: "p/m"}, Tools: []string{"bash"}}, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) {
			return &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textTurnRewind("reply one")}}}, nil
		},
		Sinks:     []agent.Sink{r.rec.Sink(agent.NopSink{})},
		Tools:     set,
		Env:       agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
		OnMessage: []func(agent.MessageInfo){r.rec.Message},
	})
	require.NoError(t, a.Prompt(context.Background(), agent.Input{Text: "one"}))

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
	forkA, _ := w.Append(session.TypeMessage,
		session.MessageData{Message: llm.Text(llm.RoleUser, "rewind + resubmit")})
	require.NoError(t, w.Sync())

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
	var oldActive bool
	for _, tr := range tree {
		if tr.ID == tipOld {
			oldActive = tr.Active
		}
	}
	assert.False(t, oldActive, "the rewound-away branch must read as a fork, not active")
}

// TestRewindToPrior verifies picking a message maps to rewinding *before* it:
// the head becomes that message's parent and its full text is returned for the
// editor, so re-sending starts the new branch. A user prompt rewinds to its
// parent and pre-fills; an assistant reply stays as its own head with no text.
func TestRewindTarget(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := session.Create(p, session.SessionData{Version: session.Version()})
	require.NoError(t, err)

	u1, _ := w.Append(session.TypeMessage,
		session.MessageData{Message: llm.Text(llm.RoleUser, "hello world")})
	a2, _ := w.Append(session.TypeMessage,
		session.MessageData{Message: llm.Text(llm.RoleAssistant, "hi there")})

	entries := readEntriesRewind(t, p)

	// picking the assistant reply keeps that message as its own head with no pre-fill.
	head, fill, ok := session.RewindTarget(entries, a2.ID)
	assert.True(t, ok)
	assert.Equal(t, a2.ID, head)
	assert.Empty(t, fill)

	// picking the first user message rewinds to its parent (the session entry),
	// so only that text sits in the editor.
	head2, fill2, ok := session.RewindTarget(entries, u1.ID)
	assert.True(t, ok, "even the root-most message has the session line before it")
	assert.Equal(t, entries[0].ID, head2) // the session entry
	assert.Equal(t, "hello world", fill2)
}

type singleToolSet struct{ tool agent.Tool }

func (s singleToolSet) Get(name string) (agent.Tool, bool) { return s.tool, name == "bash" }
func (s singleToolSet) Schemas() []llm.ToolSchema          { return []llm.ToolSchema{s.tool.Schema()} }
func (s singleToolSet) Names() []string                    { return []string{"bash"} }

// noopRewindTool executes instantly so the loop never blocks.
type noopRewindTool struct{}

func (noopRewindTool) Name() string                { return "bash" }
func (noopRewindTool) Label(agent.ToolCall) string { return "bash: ..." }
func (noopRewindTool) Description() string         { return "test tool" }
func (noopRewindTool) Schema() llm.ToolSchema      { return llm.ToolSchema{Name: "bash"} }
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
	// session; modeNewSession and the cancelled resume each create a newer file,
	// so they come last to keep "latest" pointing at the seed above.

	// --continue: reuse the most recent transcript (the seed, still alone).
	wCont, err := openSession(store, modeContinue, ws, "", "", pickFirst)
	require.NoError(t, err)
	assert.Equal(t, firstPath, wCont.Path(), "--continue must resume the latest session")

	// --resume with a selection: reuse that root's transcript.
	wPick, err := openSession(store, modeResumePick, ws, "", "", pickFirst)
	require.NoError(t, err)
	assert.Equal(t, firstPath, wPick.Path(), "--resume must reopen the selected session")

	// --resume <id>: reuse that exact saved transcript directly.
	wID, err := openSession(store, modeResumeID, ws, seedID, "", pickFirst)
	require.NoError(t, err)
	assert.Equal(t, firstPath, wID.Path(), "--resume <id> must reopen that exact session")

	// no flag: always a fresh file, never reuses.
	wNew, err := openSession(store, modeNewSession, ws, "", "", pickFirst)
	require.NoError(t, err)
	assert.NotEqual(t, firstPath, wNew.Path(), "no flag must start a new session")

	// --resume cancelled (ErrCancelled): start fresh rather than stall.
	wCancel, err := openSession(store, modeResumePick, ws, "", "",
		func([]session.Info) (int, error) { return 0, tui.ErrCancelled })
	require.NoError(t, err)
	assert.NotEqual(t, firstPath, wCancel.Path(), "cancelling --resume should start a new session")
}

// TestOpenSessionResumePick verifies --resume opens the selected root's file.
func TestOpenSessionResumePick(t *testing.T) {
	t.Parallel()
	ws := t.TempDir() + "/w"
	require.NoError(t, os.MkdirAll(ws, 0o700))
	store := session.StoreAt(filepath.Join(t.TempDir(), "root"))

	// two distinct saved sessions.
	_, err := store.Create(ws, session.SessionData{Version: session.Version()})
	require.NoError(t, err)
	have, _ := store.List(ws)
	require.Len(t, have, 1)
	target := have[0]

	// picker returns the only root's index -> open that exact file.
	w, err := openSession(store, modeResumePick, ws, "", "",
		func(list []session.Info) (int, error) {
			assert.Len(t, list, 1)
			return 0, nil
		})
	require.NoError(t, err)
	assert.Equal(t, target.Path, w.Path())
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
	older, _ := store.Create(ws, session.SessionData{Version: session.Version()})
	list, _ := store.List(ws)
	require.Len(t, list, 2)

	// resolve the saved info for that older file so we can address it by id.
	var want session.Info
	for _, in := range list {
		if in.Path == older.Path() {
			want = in
		}
	}
	require.NotEmpty(t, want.ID)

	w, err := openSession(store, modeResumeID, ws, want.ID, "", nil)
	require.NoError(t, err)
	assert.Equal(t, older.Path(), w.Path())

	// a bogus id resolves to ErrNotFound.
	_, err = openSession(store, modeResumeID, ws, "NO-SUCH-ID", "", nil)
	assert.ErrorIs(t, err, session.ErrNotFound)
}

// TestExtractResume locks in how --resume and its optional id are parsed out of
// argv: bare means pick, a following token or = form carries the id, and unrelated
// args pass through untouched for flag.Parse.
func TestExtractResume(t *testing.T) {
	t.Parallel()
	given, id, rest := extractResume([]string{"--resume", "01JXYZ"})
	assert.True(t, given)
	assert.Equal(t, "01JXYZ", id)
	assert.Empty(t, rest)

	// bare --resume (no trailing token) -> pick mode.
	given, id, rest = extractResume([]string{"--resume"})
	assert.True(t, given)
	assert.Empty(t, id)
	assert.Empty(t, rest)

	// the = form carries the id too.
	_, id, _ = extractResume([]string{"--resume=01ABC", "--continue"})
	assert.Equal(t, "01ABC", id)

	// unrelated flags and positionals are preserved; --continue still parses later.
	given, _, rest = extractResume([]string{"-m", "p/m", "hello", "world"})
	assert.False(t, given)
	assert.Equal(t, []string{"-m", "p/m", "hello", "world"}, rest)

	// a trailing id after --resume is consumed even when other args follow.
	given, id, rest = extractResume([]string{"--resume", "01ZZZ", "seed"})
	assert.True(t, given)
	assert.Equal(t, "01ZZZ", id)
	assert.Equal(t, []string{"seed"}, rest)
}

// TestSessionHint verifies the resume hint resolves to the transcript's root id.
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

func TestSearchItems(t *testing.T) {
	got := searchItems([]session.Prompt{
		{Text: "fix retry", At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{Text: "line one\nline two"},
	})

	require.Len(t, got, 2)
	assert.Equal(t, "fix retry", got[0].Text)
	// sessions are stored as UTC and rendered with the same format as the resume picker
	assert.Equal(t, "2026-01-02 03:04 UTC", got[0].Detail)
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
