package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/mcp"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/subagent"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
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
	assert.False(t, oldActive)
}

// TestInitialRow verifies the rewind picker opens where the context currently
// ends rather than at the bottom of the newest branch, so reopening it after a
// rewind lands back at the same place in the tree.
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

// TestRewindRowLabels verifies each tree row kind carries a tag, so the picker's
// role column is never blank and the tree guides stay aligned, and that the kind
// word is not repeated in the row body.
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
	a := agent.New(&agent.State{Model: llm.Model{ID: "a"}, Tools: []string{"bash"}}, agent.Options{
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
	assert.True(t, ok)
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
	assert.Equal(t, firstPath, wCont.Path())

	// --resume with a selection: reuse that root's transcript.
	wPick, err := openSession(store, modeResumePick, ws, "", "", pickFirst)
	require.NoError(t, err)
	assert.Equal(t, firstPath, wPick.Path())

	// --resume <id>: reuse that exact saved transcript directly.
	wID, err := openSession(store, modeResumeID, ws, seedID, "", pickFirst)
	require.NoError(t, err)
	assert.Equal(t, firstPath, wID.Path())

	// no flag: always a fresh file, never reuses.
	wNew, err := openSession(store, modeNewSession, ws, "", "", pickFirst)
	require.NoError(t, err)
	assert.NotEqual(t, firstPath, wNew.Path())

	// --resume cancelled (ErrCancelled): start fresh rather than stall.
	wCancel, err := openSession(store, modeResumePick, ws, "", "",
		func([]session.Info) (int, error) { return 0, tui.ErrCancelled })
	require.NoError(t, err)
	assert.NotEqual(t, firstPath, wCancel.Path())
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

	// the picker's chosen index maps to that exact root among several.
	for i, in := range list {
		if in.Path != older.Path() {
			continue
		}
		wPick, err := openSession(store, modeResumePick, ws, "", "",
			func([]session.Info) (int, error) { return i, nil })
		require.NoError(t, err)
		assert.Equal(t, older.Path(), wPick.Path())
	}

	w, err := openSession(store, modeResumeID, ws, want.ID, "", nil)
	require.NoError(t, err)
	assert.Equal(t, older.Path(), w.Path())

	// a bogus id resolves to ErrNotFound.
	_, err = openSession(store, modeResumeID, ws, "NO-SUCH-ID", "", nil)
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

// TestExtractResume locks in how --resume and its optional id are parsed out of
// argv: bare means pick, a following token or = form carries the id, and unrelated
// args pass through untouched for flag.Parse.
// TestSubmittedEcho asserts a submitted KindPrompt echoes above the line, while
// commands and shell lines do not — matching TurnStart's old behaviour.
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

// TestEmptyReportsNoConversation verifies a fresh transcript with zero message
// entries is detected as empty so it can be dropped on exit, while any recorded
// turn (or an unrecorded run) keeps the session.
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
}

func TestSearchItems(t *testing.T) {
	t.Parallel()
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

func TestResolveSubAgentModel(t *testing.T) {
	t.Parallel()

	win := 100000
	reg, _ := llm.NewRegistry(llm.File{Providers: map[string]llm.ProviderConfig{
		"p": {Models: []llm.ModelConfig{{ID: "child", ContextWindow: &win}}},
	}}, nil, llm.RegistryOptions{})
	set, _, err := config.Load(config.Options{Workspace: t.TempDir()})
	require.NoError(t, err)

	// configured child model resolves through the registry.
	require.NoError(t, set.SetSession("subagent.model", "p/child"))
	st := &agent.State{Model: llm.Model{Provider: "p", ID: "session"}}
	got := resolveSubAgentModel(set, reg, st)
	assert.Equal(t, "child", got.ID) // the configured child model wins

	// unset falls back to the session's current model.
	set2, _, _ := config.Load(config.Options{Workspace: t.TempDir()})
	got = resolveSubAgentModel(set2, reg, st)
	assert.Equal(t, "session", got.ID) // inherited when subagent.model is empty
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

// reasoningModel is a model that supports every standard level.
func reasoningModel() llm.Model {
	m := llm.Model{Provider: "p", ID: "m"}
	m.Caps.Reasoning = true
	return m
}

func TestReasoningFromConfig(t *testing.T) {
	t.Parallel()
	rc := reasoningFrom(config.Reasoning{Level: "high", Retain: "none", Show: false}, reasoningModel())
	assert.Equal(t, llm.LevelHigh, rc.Level)
	assert.Equal(t, llm.RetainNone, rc.Retain)
	assert.False(t, rc.Show)

	// an empty block falls back to the compiled-in defaults
	d := reasoningFrom(config.Reasoning{}, reasoningModel())
	assert.Equal(t, llm.LevelMedium, d.Level)
	assert.Equal(t, llm.RetainWholeTurn, d.Retain)
}

func TestToolLimitsFromConfig(t *testing.T) {
	t.Parallel()
	l := toolLimitsFrom(config.ToolLimits{Bash: config.Limit{Lines: 10}, Read: config.Limit{Bytes: 4096}})
	// each configured axis copies straight through; unset axes stay zero here,
	// and ApplyLimits fills them from the package defaults at startup.
	assert.Equal(t, tools.Limit{Lines: 10}, l.Bash)
	assert.Equal(t, tools.Limit{Bytes: 4096}, l.Read)
}

// TestPermissionsModeDefaultResolves asserts the compiled-in default is
// allow-read and Explain reports it at the (default) layer. The home dir is
// isolated so a real user config on the machine never shifts the source.
func TestPermissionsModeDefaultResolves(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	set, _, err := config.Load(config.Options{
		Workspace: t.TempDir(),
		Env:       func(string) string { return "" },
	})
	require.NoError(t, err)
	assert.Equal(t, "allow-read", set.Settings().Permissions.Mode)

	v, src, ok := set.Explain("permissions.mode")
	require.True(t, ok)
	assert.Equal(t, `"allow-read"`, string(v))
	assert.Equal(t, "default", src)
}

// TestSetSessionSettingAppliesModeAndPublishesSegment verifies the permissions
// branch of SetSessionSetting drives the live barrier and its status segment.
func TestSetSessionSettingAppliesModeAndPublishesSegment(t *testing.T) {
	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	outR, outW, err := os.Pipe()
	require.NoError(t, err)

	ui, err := tui.New(tui.Options{In: inR, Out: outW, Mode: tui.ModePlain})
	require.NoError(t, err)
	t.Cleanup(func() {
		ui.Close()
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
	})

	set, _, _ := config.Load(config.Options{
		Workspace: t.TempDir(),
		Env:       func(string) string { return "" },
	})
	b := permit.NewBarrier(func(string) bool { return false })
	c := &uiConsole{ui: ui, set: set, permit: b}

	assert.Equal(t, permit.ModeAllowRead, b.Mode())
	err = c.SetSessionSetting("permissions.mode", "block-all")
	require.NoError(t, err)
	assert.Equal(t, permit.ModeBlockAll, b.Mode())

	// recorded as a session override so Explain reports (session) and resume restores it.
	v, src, ok := set.Explain("permissions.mode")
	require.True(t, ok)
	assert.Equal(t, `"block-all"`, string(v))
	assert.Equal(t, "session", src)

	// an unparsable value leaves the barrier untouched.
	err = c.SetSessionSetting("permissions.mode", "nonsense")
	require.NoError(t, err)
	assert.Equal(t, permit.ModeBlockAll, b.Mode())
}

// TestSetSessionSettingOtherKeysLeaveBarrierAlone asserts non-permission keys do
// not disturb the barrier's mode.
func TestSetSessionSettingOtherKeysLeaveBarrierAlone(t *testing.T) {
	set, _, _ := config.Load(config.Options{
		Workspace: t.TempDir(),
		Env:       func(string) string { return "" },
	})
	b := permit.NewBarrier(func(string) bool { return false })
	c := &uiConsole{set: set, permit: b}
	err := c.SetSessionSetting("model", "p/m")
	require.NoError(t, err)
	assert.Equal(t, permit.ModeAllowRead, b.Mode())
}

// TestPromptAdapterPlainModeReportsNoUI asserts the prompter refuses in headless
// mode so a call that would prompt is denied rather than silently allowed.
func TestPromptAdapterPlainModeReportsNoUI(t *testing.T) {
	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	outR, outW, err := os.Pipe()
	require.NoError(t, err)

	ui, err := tui.New(tui.Options{In: inR, Out: outW, Mode: tui.ModePlain})
	require.NoError(t, err)
	t.Cleanup(func() {
		ui.Close()
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
	})

	a := promptAdapter{ui: ui}
	_, err = a.Open("Allow?", "cmd", []string{"Allow"})
	assert.ErrorIs(t, err, tui.ErrNoUI)
}

// TestClassifierAdapterClassifiesShellCommands drives a fresh-context model call
// against a scripted provider, covering readonly/write/garbled verdicts.
func TestClassifierAdapterClassifiesShellCommands(t *testing.T) {
	t.Parallel()

	model := llm.Model{ID: "p/m", Provider: "scripted"}
	adapterFor := func(reply string) classifierAdapter {
		sp := newScripted(reply)
		return classifierAdapter{
			providerFor: func(llm.Model) (llm.Provider, error) { return sp, nil },
			model:       func() llm.Model { return model },
		}
	}

	t.Run("readonly_verdict", func(t *testing.T) {
		assert.Equal(t, permit.ClassReadOnly, adapterFor("read-only").Classify(t.Context(), permit.Subject{Name: "bash", Args: "stat a"}))
	})
	t.Run("write_verdict_with_noise", func(t *testing.T) {
		assert.Equal(t, permit.ClassWrite, adapterFor("WRITE: it modifies the file!").Classify(t.Context(), permit.Subject{Name: "bash", Args: "rm a"}))
	})
	t.Run("garbled_maps_to_unsure", func(t *testing.T) {
		assert.Equal(t, permit.ClassUnsure, adapterFor("? maybe 42").Classify(t.Context(), permit.Subject{Name: "bash", Args: "weird cmd"}))
	})
}

// A classifier adapter that cannot reach a provider must fail safe to "unsure"
// rather than guess.
func TestClassifierAdapterFailuresAreUnsure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ad   classifierAdapter
	}{
		{"no_model_configured", classifierAdapter{ // zero model means no provider
			providerFor: func(llm.Model) (llm.Provider, error) { return nil, nil },
			model:       func() llm.Model { return llm.Model{} },
		}},
		{"provider_error", classifierAdapter{
			providerFor: func(llm.Model) (llm.Provider, error) { return nil, errors.New("no provider") },
			model:       func() llm.Model { return llm.Model{ID: "p/m"} },
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, permit.ClassUnsure, tc.ad.Classify(t.Context(), permit.Subject{Name: "bash", Args: "anything"}))
		})
	}
}

func TestClassifierAdapterRequestUsesFreshContextAndMinimalReasoning(t *testing.T) {
	t.Parallel()

	model := llm.Model{ID: "p/m", Provider: "scripted", MaxOutput: 64000}
	sp := newScripted("readonly")
	adapter := classifierAdapter{
		providerFor: func(llm.Model) (llm.Provider, error) { return sp, nil },
		model:       func() llm.Model { return model },
	}

	_ = adapter.Classify(t.Context(), permit.Subject{Name: "bash", Args: "stat a"})
	reqs := sp.Requests()
	require.Len(t, reqs, 1)
	r := reqs[0]
	assert.Equal(t, llm.RoleUser, r.Messages[0].Role) // the command is the whole user turn
	var sys strings.Builder
	for _, b := range r.System {
		if tb, ok := b.(llm.TextBlock); ok {
			sys.WriteString(tb.Text)
		}
	}
	assert.Contains(t, sys.String(), "readonly")
	// a model that cannot reason clamps minimal to off; the field is always populated.
	assert.Equal(t, llm.ClampLevel(model, llm.LevelMinimal), r.Reasoning.Level)
}

func TestClassifierAdapterClassifiesMCPCallWithMetadata(t *testing.T) {
	t.Parallel()

	model := llm.Model{ID: "p/m", Provider: "scripted"}
	sp := newScripted("readonly")
	adapter := classifierAdapter{
		providerFor: func(llm.Model) (llm.Provider, error) { return sp, nil },
		model:       func() llm.Model { return model },
		schema: func(name string) (llm.ToolSchema, bool) {
			if name != "srv__list" {
				return llm.ToolSchema{}, false
			}
			return llm.ToolSchema{Name: name, Description: "lists things", Parameters: []byte(`{"type":"object"}`)}, true
		},
	}

	assert.Equal(t, permit.ClassReadOnly, adapter.Classify(t.Context(), permit.Subject{Name: "srv__list", Args: `{}`}))

	reqs := sp.Requests()
	require.Len(t, reqs, 1)
	var sys strings.Builder
	for _, b := range reqs[0].System {
		if tb, ok := b.(llm.TextBlock); ok {
			sys.WriteString(tb.Text)
		}
	}
	// the MCP prompt embeds description and parameters so the model can judge it.
	assert.Contains(t, sys.String(), "lists things")
	assert.Contains(t, sys.String(), `{"type":"object"}`)
}

func TestClassifierAdapterUnknownMCPSafelyUnsure(t *testing.T) {
	t.Parallel()

	adapter := classifierAdapter{ // no schema lookup wired at all
		providerFor: func(llm.Model) (llm.Provider, error) { return nil, nil },
		model:       func() llm.Model { return llm.Model{ID: "p/m"} },
	}
	assert.Equal(t, permit.ClassUnsure, adapter.Classify(t.Context(), permit.Subject{Name: "srv__list", Args: `{}`}))

	adapter.schema = func(string) (llm.ToolSchema, bool) { return llm.ToolSchema{}, false } // unknown tool
	assert.Equal(t, permit.ClassUnsure, adapter.Classify(t.Context(), permit.Subject{Name: "ghost", Args: `{}`}))
}

// A truncated verdict must never be guessed from partial text: it falls back to
// the approval dialog as unsure, same philosophy as TestRunSummaryCancelStopsDraining.
func TestClassifierAdapterTruncatedVerdictIsUnsure(t *testing.T) {
	t.Parallel()

	sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: truncatedStream("readonly")}}}
	adapter := classifierAdapter{
		providerFor: func(llm.Model) (llm.Provider, error) { return sp, nil },
		model:       func() llm.Model { return llm.Model{ID: "p/m", Provider: "scripted"} },
	}

	assert.Equal(t, permit.ClassUnsure, adapter.Classify(t.Context(), permit.Subject{Name: "bash", Args: "stat a"}))
}

func newScripted(reply string) *llm.ScriptedProvider {
	return &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: []llm.Event{
		{Type: llm.EventMessageStart},
		{Type: llm.EventTextStart, Index: 0},
		{Type: llm.EventTextDelta, Index: 0, Text: reply},
		{Type: llm.EventTextEnd, Index: 0},
		{Type: llm.EventDone, StopReason: llm.StopEndTurn},
	}}}}
}

// TestMCPConfigDisabledToolsEnableViaSlashTools wires a real tools.Registry to
// an MCP manager for a config-disabled server and verifies /tools can bring its
// tools into the agent context via live free-select (SetEnabled after load). The
// resume path is covered by TestMCPConfigDisabledToolsResumeRestoresEnablement.
func TestMCPConfigDisabledToolsEnableViaSlashTools(t *testing.T) {
	reg := tools.New()
	reg.RegisterState("builtin", &stubTool{name: "read"}, tools.StateEnabled)

	disabled := false
	mgr := mcp.New(map[string]mcp.ServerConfig{
		"fake": {Command: buildFakeMCPServer(t), Enabled: &disabled},
	}, mcp.Options{Registrar: registryAdapter{reg}})

	// first-message load registers every fake tool as disabled (config-off default)
	mgr.LoadOnFirstMessage(t.Context())
	t.Cleanup(mgr.Close)
	assert.NotContains(t, reg.Names(), "fake__tool_00")
	assert.Contains(t, reg.DisabledNames("mcp: fake"), "fake__tool_00")

	// /tools free-select enables the MCP tool alongside builtins
	reg.SetEnabled([]string{"read", "fake__tool_00"})

	// it must now be in the agent context (Schemas/Names) and active for status
	assert.Contains(t, reg.Names(), "fake__tool_00")
	assert.NotEmpty(t, reg.Schemas())
	hasSchema := false
	for _, s := range reg.Schemas() {
		if s.Name == "fake__tool_00" {
			hasSchema = true
		}
	}
	assert.True(t, hasSchema, "enabled MCP tool must reach the agent's tool block")
	assert.NotContains(t, reg.DisabledNames("mcp: fake"), "fake__tool_00")
}

// TestMCPConfigDisabledToolsResumeRestoresEnablement verifies a resumed session's
// persisted tools.enabled (Options.Restore) re-enables an MCP tool even when its
// server is config-disabled — /tools enablement is explicit, not vetoed by the
// config default.
func TestMCPConfigDisabledToolsResumeRestoresEnablement(t *testing.T) {
	reg := tools.New()
	reg.RegisterState("builtin", &stubTool{name: "read"}, tools.StateEnabled)

	disabled := false
	mgr := mcp.New(map[string]mcp.ServerConfig{
		"fake": {Command: buildFakeMCPServer(t), Enabled: &disabled},
	}, mcp.Options{
		Registrar: registryAdapter{reg},
		Restore:   []string{"read", "fake__tool_00"}, // persisted from a prior session
	})

	mgr.LoadOnFirstMessage(t.Context())
	t.Cleanup(mgr.Close)

	assert.Contains(t, reg.Names(), "fake__tool_00")
}

// buildFakeMCPServer builds the pkg/mcp fakeserver binary and returns its path.
func buildFakeMCPServer(t *testing.T) string {
	t.Helper()
	out := filepath.Join(os.TempDir(), fmt.Sprintf("ajent-main-fakeserver-%d", os.Getpid()))
	ctx := context.Background() // build is not tied to a test's lifetime
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "./pkg/mcp/testdata/fakeserver")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fakeserver: %v\n%s", err, b)
	}
	return out
}

// TestGuardRegisteredAgainstRegistry verifies the barrier's guard and asker are
// registered so an unverifiable call asks through them.
func TestGuardRegisteredAgainstRegistry(t *testing.T) {
	reg := tools.New()
	b := permit.NewBarrier(func(string) bool { return false })
	reg.AddGuard(b.Guard())
	reg.SetAsker(b.Asker())

	tool := &stubTool{name: "write"}
	reg.RegisterState("builtin", tool, tools.StateEnabled)

	g, ok := reg.Get("write")
	require.True(t, ok)

	res, err := g.Execute(t.Context(), agent.ToolCall{
		ID: "c1", Name: "write", Input: []byte(`{}`),
	}, nil)
	require.NoError(t, err) // a denial is an error result, not a Go error
	assert.True(t, res.IsError)
}

// stubTool is a minimal agent.Tool for guard-chain tests.
type stubTool struct{ name string }

func (s *stubTool) Name() string                { return s.name }
func (s *stubTool) Label(agent.ToolCall) string { return s.name + " ..." }
func (s *stubTool) Description() string         { return "test tool" }
func (s *stubTool) Schema() llm.ToolSchema      { return llm.ToolSchema{Name: s.name} }
func (s *stubTool) Mode() agent.ExecutionMode   { return agent.ModeSerial }
func (s *stubTool) Execute(_ context.Context, _ agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
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
	st := &agent.State{Model: model, Tools: []string{"bash"}, Tokens: tokens.New(model)}
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
	committed := false
	r := &sessRec{w: w, rec: session.NewRecorder(w), started: &committed}
	r.rec.Message(agent.MessageInfo{Message: llm.Text(llm.RoleUser, "hello there")})

	entries := readEntriesRewind(t, p)
	require.NoError(t, r.switchState(nil, ag, reg, entries[len(entries)-1].ID, "rewind: "))

	cs := st.Tokens.Context()
	assert.Equal(t, 100000, cs.Window) // framed by the live model, not left at zero
	assert.Equal(t, tokens.CompactAt(model), cs.Compact)
	assert.NotZero(t, cs.Used)
}

// TestSubagentSinkTurnEnd pins the TurnEnd release: an aborted turn clears a
// queued batch's in-flight marks so the next Flush re-offers it, while a clean
// StopEndTurn leaves them set (no duplicate on a normal turn).
func TestSubagentSinkTurnEnd(t *testing.T) {
	t.Parallel()

	newMgr := func() (*subagent.Manager, *atomic.Int32) {
		var delivered atomic.Int32 // steers accepted by Deliver; Delivered never fires
		mgr := subagent.New(subagent.Options{
			Provider: func(llm.Model) (llm.Provider, error) {
				return &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textTurnRewind("summary")}}}, nil
			},
			Deliver: func(agent.Input) bool { delivered.Add(1); return true },
		})
		t.Cleanup(mgr.Close)
		return mgr, &delivered
	}
	settle := func(t *testing.T, m *subagent.Manager, id string) {
		t.Helper()
		require.Eventually(t, func() bool {
			for _, j := range m.List() {
				if j.ID == id && j.Status == subagent.StatusDone {
					return true
				}
			}
			return false
		}, 2*time.Second, 5*time.Millisecond)
	}

	t.Run("abort_releases_marks", func(t *testing.T) {
		mgr, delivered := newMgr()
		id := mgr.Start("task", "")
		settle(t, mgr, id)

		sink := subagentSink{mgr: mgr}
		mgr.Flush() // steer accepted; the mark is now in flight
		assert.EqualValues(t, 1, delivered.Load())

		sink.TurnEnd(agent.TurnResult{Stop: llm.StopAborted})
		mgr.Flush() // released marks let the pending id ride again
		assert.EqualValues(t, 2, delivered.Load())
	})

	t.Run("clean_keeps_marks", func(t *testing.T) {
		mgr, delivered := newMgr()
		id := mgr.Start("task", "")
		settle(t, mgr, id)

		sink := subagentSink{mgr: mgr}
		mgr.Flush() // steer accepted; the mark is now in flight
		assert.EqualValues(t, 1, delivered.Load())

		sink.TurnEnd(agent.TurnResult{Stop: llm.StopEndTurn}) // no release
		mgr.Flush()                                           // still in flight; nothing may re-offer
		assert.EqualValues(t, 1, delivered.Load())
	})
}
