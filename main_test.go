package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/session"
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
		assert.Equal(t, permit.ClassReadOnly, adapterFor("read-only").Classify(t.Context(), "stat a"))
	})
	t.Run("write_verdict_with_noise", func(t *testing.T) {
		assert.Equal(t, permit.ClassWrite, adapterFor("WRITE: it modifies the file!").Classify(t.Context(), "rm a"))
	})
	t.Run("garbled_maps_to_unsure", func(t *testing.T) {
		assert.Equal(t, permit.ClassUnsure, adapterFor("? maybe 42").Classify(t.Context(), "weird cmd"))
	})
}

func TestClassifierAdapterNoModelIsUnsure(t *testing.T) {
	t.Parallel()

	adapter := classifierAdapter{
		providerFor: func(llm.Model) (llm.Provider, error) { return nil, nil },
		model:       func() llm.Model { return llm.Model{} }, // no model configured
	}
	assert.Equal(t, permit.ClassUnsure, adapter.Classify(t.Context(), "anything"))
}

func TestClassifierAdapterProviderErrorIsUnsure(t *testing.T) {
	t.Parallel()

	adapter := classifierAdapter{
		providerFor: func(llm.Model) (llm.Provider, error) { return nil, errors.New("no provider") },
		model:       func() llm.Model { return llm.Model{ID: "p/m"} },
	}
	assert.Equal(t, permit.ClassUnsure, adapter.Classify(t.Context(), "anything"))
}

func TestClassifierAdapterRequestUsesFreshContextAndMinimalReasoning(t *testing.T) {
	t.Parallel()

	model := llm.Model{ID: "p/m", Provider: "scripted", MaxOutput: 64000}
	sp := newScripted("readonly")
	adapter := classifierAdapter{
		providerFor: func(llm.Model) (llm.Provider, error) { return sp, nil },
		model:       func() llm.Model { return model },
	}

	_ = adapter.Classify(t.Context(), "stat a")
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

func newScripted(reply string) *llm.ScriptedProvider {
	return &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: []llm.Event{
		{Type: llm.EventMessageStart},
		{Type: llm.EventTextStart, Index: 0},
		{Type: llm.EventTextDelta, Index: 0, Text: reply},
		{Type: llm.EventTextEnd, Index: 0},
		{Type: llm.EventDone, StopReason: llm.StopEndTurn},
	}}}}
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
