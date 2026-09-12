package app

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/mcp"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/subagent"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubTool is a minimal agent.Tool for guard-chain and registry tests.
type stubTool struct{ name string }

func (s *stubTool) Name() string                { return s.name }
func (s *stubTool) Label(agent.ToolCall) string { return s.name + " ..." }
func (s *stubTool) Description() string         { return "test tool" }
func (s *stubTool) Schema() llm.ToolSchema      { return llm.ToolSchema{Name: s.name} }
func (s *stubTool) Mode() agent.ExecutionMode   { return agent.ModeSerial }
func (s *stubTool) Execute(_ context.Context, _ agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
}

// holdBash blocks in Execute until released or cancelled, pinning a turn mid-dispatch.
type holdBash struct {
	release chan struct{}
	entered chan struct{}
}

func (holdBash) Name() string                { return tools.ToolBash }
func (holdBash) Label(agent.ToolCall) string { return "bash: ..." }
func (holdBash) Description() string         { return "test tool" }
func (holdBash) Schema() llm.ToolSchema      { return llm.ToolSchema{Name: tools.ToolBash} }
func (holdBash) Mode() agent.ExecutionMode   { return agent.ModeSerial }

func (t holdBash) Execute(ctx context.Context, _ agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	if t.entered != nil {
		close(t.entered)
	}
	select {
	case <-t.release:
	case <-ctx.Done():
		return agent.ToolResult{}, ctx.Err()
	}
	return agent.ToolResult{Content: llm.BlockList{llm.TextBlock{Text: "ok"}}}, nil
}

// mainToolTurn frames one assistant turn that ends in a single bash tool call.
func mainToolTurn() []llm.Event {
	out := make([]llm.Event, 0, 5)

	var start llm.Event
	start.Type = llm.EventMessageStart

	var call llm.Event
	call.Type = llm.EventToolCallStart
	call.ToolCallID = "c1"
	call.ToolName = tools.ToolBash

	var delta llm.Event
	delta.Type = llm.EventToolCallDelta
	delta.Text = `{"a":1}`

	var end llm.Event
	end.Type = llm.EventToolCallEnd
	end.Block = llm.ToolCallBlock{ID: "c1", Name: tools.ToolBash, Input: []byte(`{"a":1}`)}

	var done llm.Event
	done.Type = llm.EventDone
	done.StopReason = llm.StopEndTurn

	return append(out, start, call, delta, end, done)
}

// TestTypingGateDeliversIntoHeldBoundary wires the real gate, steer queue and agent:
// a prompt submitted while AwaitInput holds a boundary lands in that same step, with no
// intervening model request.
func TestTypingGateDeliversIntoHeldBoundary(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})
	set := singleToolSet{tool: holdBash{release: release, entered: entered}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: mainToolTurn()},
		{Events: textTurnRewind("after steer")},
	}}

	fakeUI := &fakeQueueUI{}
	q := newSteerQueue(fakeUI, nil, nil)

	st := &agent.State{Model: llm.Model{ID: "test"}, Reasoning: llm.ReasoningConfig{}}
	held := make(chan struct{}, 1)
	gate := &typingGate{
		idle:    time.Hour,
		handoff: 30 * time.Millisecond,
		poll:    5 * time.Millisecond,
		pending: q.pending,
	}
	// status is the only place a held boundary signals; non-empty text means engaged
	gate.status = func(text, short string) {
		if text == "" {
			return
		}
		select {
		case held <- struct{}{}:
		default:
		}
	}

	a := agent.New(st, agent.Options{
		Provider:   func(llm.Model) (llm.Provider, error) { return p, nil },
		Sinks:      []agent.Sink{agent.NopSink{}},
		Tools:      set,
		Env:        agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
		OnBoundary: q.pull,
		AwaitInput: gate.hold,
	})

	in := agent.Input{Text: "start"}
	q.offer(in, "start", 0) // starts the drain; not queued
	gate.taken()            // runPump clears any handoff before spawning a turn
	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), in) }()

	// pin step one inside its tool call, then begin typing mid-turn
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("the turn never reached its tool call")
	}
	gate.edit("draft") // the user is composing a message

	close(release) // step one finishes; AwaitInput at step two holds on the draft
	select {
	case <-held:
	case <-time.After(time.Second):
		t.Fatal("AwaitInput never held the boundary while typing")
	}
	assert.Len(t, p.Requests(), 1, "no second request may leave while the hold is engaged")

	// a prompt submitted during the hold queues and must land at this same step
	require.True(t, q.offer(agent.Input{Text: "steered"}, "steered", 3))

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("the turn never finished after the held boundary delivered")
	}
	reqs := p.Requests()
	assert.Len(t, reqs, 2, "the steer must ride into step two with no third model call")

	foundSteer := false
	for _, m := range reqs[len(reqs)-1].Messages {
		if m.Role != llm.RoleUser || len(m.Content) == 0 {
			continue
		}
		tb, ok := m.Content[0].(llm.TextBlock)
		if !ok {
			continue
		}
		if strings.Contains(tb.Text, "steered") {
			foundSteer = true
		}
	}
	assert.True(t, foundSteer, "step two's request must carry the queued steer")
}

// TestControlLoop covers the idle-editor quit gestures: one Ctrl+C arms the
// window and only a second inside it quits, while Ctrl+D quits outright.
func TestControlLoop(t *testing.T) {
	t.Parallel()

	// start runs the loop over a control channel the caller drives. cycled fires
	// per Shift+Tab, giving a test a signal ordered behind earlier controls.
	start := func(t *testing.T) (controls chan tui.Control, quit chan struct{}, cycled chan struct{}) {
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
		go func() { _, _ = io.Copy(io.Discard, outR) }() // status writes must not fill the pipe
		ui, err := tui.New(tui.Options{In: inR, Out: outW, Mode: tui.ModePlain})
		require.NoError(t, err)
		t.Cleanup(ui.Close)

		controls = make(chan tui.Control, 4)
		quit = make(chan struct{})
		cycled = make(chan struct{}, 4)
		ag := agent.New(&agent.State{}, agent.Options{})
		go controlLoop(controls, ui, ag, &steerQueue{}, command.NewStager(nil, nil), nil, quit,
			func() { cycled <- struct{}{} })
		return controls, quit, cycled
	}
	quitted := func(t *testing.T, quit chan struct{}) bool {
		t.Helper()
		select {
		case <-quit:
			return true
		case <-time.After(2 * time.Second):
			return false
		}
	}

	t.Run("double_press_quits", func(t *testing.T) {
		controls, quit, _ := start(t)
		controls <- tui.ControlInterrupt
		controls <- tui.ControlInterrupt
		assert.True(t, quitted(t, quit))
	})

	// one press only arms the window: the app stays up until the second arrives
	t.Run("single_press_holds", func(t *testing.T) {
		controls, quit, cycled := start(t)
		controls <- tui.ControlInterrupt
		controls <- tui.ControlModeCycle // ordered behind the press: it has been handled
		<-cycled

		select {
		case <-quit:
			assert.Fail(t, "quit on a single Ctrl+C")
		default:
		}
		controls <- tui.ControlInterrupt
		assert.True(t, quitted(t, quit))
	})

	t.Run("ctrl_d_quits", func(t *testing.T) {
		controls, quit, _ := start(t)
		controls <- tui.ControlEOF
		assert.True(t, quitted(t, quit))
	})
}

// TestCheckSessionTarget covers the pre-TUI resolution: a bad --resume target
// and a --session name that would resume a session by id must fail fast, while a
// merely new name is fine because --session creates it.
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
		assert.NoError(t, CheckSessionTarget(ResumeID, "fix-parser"))
	})

	t.Run("resume_by_id", func(t *testing.T) {
		assert.NoError(t, CheckSessionTarget(ResumeID, id))
	})

	t.Run("resume_unknown_fails", func(t *testing.T) {
		assert.Error(t, CheckSessionTarget(ResumeID, "no-such-session"))
	})

	t.Run("new_name_is_allowed", func(t *testing.T) {
		assert.NoError(t, CheckSessionTarget(ResumeSessionName, "brand-new"))
	})

	t.Run("existing_name_is_allowed", func(t *testing.T) {
		assert.NoError(t, CheckSessionTarget(ResumeSessionName, "fix-parser"))
	})

	t.Run("name_matching_id_fails", func(t *testing.T) {
		assert.Error(t, CheckSessionTarget(ResumeSessionName, id))
	})
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
			return slices.ContainsFunc(m.List(), func(j subagent.Job) bool {
				return j.ID == id && j.Status == subagent.StatusDone
			})
		}, 2*time.Second, 5*time.Millisecond)
	}

	t.Run("abort_releases_marks", func(t *testing.T) {
		mgr, delivered := newMgr()
		id := mgr.Start("task", "")
		settle(t, mgr, id)

		sink := subagentSink{mgr: mgr}
		// the completed job is queued just after StatusDone publishes; flush until its
		// steer lands so this never races enqueue.
		require.Eventually(t, func() bool {
			if delivered.Load() == 0 {
				mgr.Flush() // steer accepted; the mark is now in flight
			}
			return delivered.Load() >= 1
		}, 2*time.Second, 5*time.Millisecond)

		sink.TurnEnd(agent.TurnResult{Stop: llm.StopAborted})
		require.Eventually(t, func() bool {
			mgr.Flush() // released marks let the pending id ride again
			return delivered.Load() >= 2
		}, 2*time.Second, 5*time.Millisecond)
	})

	t.Run("clean_keeps_marks", func(t *testing.T) {
		mgr, delivered := newMgr()
		id := mgr.Start("task", "")
		settle(t, mgr, id)

		sink := subagentSink{mgr: mgr}
		require.Eventually(t, func() bool {
			if delivered.Load() == 0 {
				mgr.Flush() // steer accepted; the mark is now in flight
			}
			return delivered.Load() >= 1
		}, 2*time.Second, 5*time.Millisecond)

		sink.TurnEnd(agent.TurnResult{Stop: llm.StopEndTurn}) // no release
		mgr.Flush()                                           // still in flight; nothing may re-offer
		assert.EqualValues(t, 1, delivered.Load())
	})
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

// TestMCPConfigDisabledToolsEnableViaSlashTools wires a real tools.Registry to
// an MCP manager for a config-disabled server and verifies /tools can bring its
// tools into the agent context via live free-select (SetEnabled after load). The
// resume path is covered by TestMCPConfigDisabledToolsResumeRestoresEnablement.
func TestMCPConfigDisabledToolsEnableViaSlashTools(t *testing.T) {
	reg := tools.New()
	reg.RegisterState("builtin", &stubTool{name: "read"}, tools.StateEnabled)

	var disabled bool
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
	var hasSchema bool
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

	var disabled bool
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
	out := filepath.Join(t.TempDir(), "fakeserver")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", out, "../../pkg/mcp/testdata/fakeserver")
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

func newScripted(reply string) *llm.ScriptedProvider {
	return &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: []llm.Event{
		{Type: llm.EventMessageStart},
		{Type: llm.EventTextStart, Index: 0},
		{Type: llm.EventTextDelta, Index: 0, Text: reply},
		{Type: llm.EventTextEnd, Index: 0},
		{Type: llm.EventDone, StopReason: llm.StopEndTurn},
	}}}}
}

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

	t.Run("allow_verdict", func(t *testing.T) {
		assert.Equal(t, permit.ClassAllow, adapterFor("allow").Classify(t.Context(), permit.Subject{Name: "bash", Args: "stat a"}))
	})
	t.Run("deny_verdict_with_noise", func(t *testing.T) {
		assert.Equal(t, permit.ClassDeny, adapterFor("DENY: it modifies the file!").Classify(t.Context(), permit.Subject{Name: "bash", Args: "rm a"}))
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

func TestClassifierAdapterRequestUsesFreshContextNoReasoning(t *testing.T) {
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
	assert.Contains(t, sys.String(), `"allow"`)
	// the classifier never reasons; off stays off whatever the model supports.
	assert.Equal(t, llm.ClampLevel(model, llm.LevelOff), r.Reasoning.Level)
}

func TestClassifierAdapterClassifiesMCPCallWithMetadata(t *testing.T) {
	t.Parallel()

	model := llm.Model{ID: "p/m", Provider: "scripted"}
	sp := newScripted("allow")
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

	assert.Equal(t, permit.ClassAllow, adapter.Classify(t.Context(), permit.Subject{Name: "srv__list", Args: `{}`}))

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
// the approval dialog as unsure.
func TestClassifierAdapterTruncatedVerdictIsUnsure(t *testing.T) {
	t.Parallel()

	sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: truncatedStream("readonly")}}}
	adapter := classifierAdapter{
		providerFor: func(llm.Model) (llm.Provider, error) { return sp, nil },
		model:       func() llm.Model { return llm.Model{ID: "p/m", Provider: "scripted"} },
	}

	assert.Equal(t, permit.ClassUnsure, adapter.Classify(t.Context(), permit.Subject{Name: "bash", Args: "stat a"}))
}

// TestClassifierAdapterSelectsPromptPerRuleSet covers auto+write's prompt and its
// allow/deny vocabulary, and that MCP calls keep the strict read-only prompt.
func TestClassifierAdapterSelectsPromptPerRuleSet(t *testing.T) {
	t.Parallel()

	model := llm.Model{ID: "p/m", Provider: "scripted"}
	adapterFor := func(reply string) (classifierAdapter, *llm.ScriptedProvider) {
		sp := newScripted(reply)
		return classifierAdapter{
			providerFor: func(llm.Model) (llm.Provider, error) { return sp, nil },
			model:       func() llm.Model { return model },
			schema: func(string) (llm.ToolSchema, bool) {
				return llm.ToolSchema{Name: "mcp__x", Description: "d", Parameters: []byte(`{}`)}, true
			},
			cwd: "/work/proj",
			tmp: "/tmp",
		}, sp
	}
	systemOf := func(sp *llm.ScriptedProvider) string {
		reqs := sp.Requests()
		require.Len(t, reqs, 1)
		require.Len(t, reqs[0].System, 1)
		tb, ok := reqs[0].System[0].(llm.TextBlock)
		require.True(t, ok)
		return tb.Text
	}

	t.Run("allow_write_shell_uses_workspace_prompt", func(t *testing.T) {
		a, sp := adapterFor("allow")
		got := a.Classify(t.Context(), permit.Subject{Name: "bash", Args: "python3 -c x", AllowWrite: true})
		assert.Equal(t, permit.ClassAllow, got)
		assert.Equal(t, permit.WorkspaceClassifierSystem("/work/proj", "/tmp"), systemOf(sp))
	})
	t.Run("allow_write_shell_denies", func(t *testing.T) {
		a, _ := adapterFor("deny")
		got := a.Classify(t.Context(), permit.Subject{Name: "bash", Args: "curl x", AllowWrite: true})
		assert.Equal(t, permit.ClassDeny, got)
	})
	t.Run("allow_write_rejects_readonly_word", func(t *testing.T) {
		a, _ := adapterFor("readonly")
		got := a.Classify(t.Context(), permit.Subject{Name: "bash", Args: "ls", AllowWrite: true})
		assert.Equal(t, permit.ClassUnsure, got) // the retired vocabulary no longer approves
	})
	t.Run("plain_shell_keeps_strict_prompt", func(t *testing.T) {
		a, sp := adapterFor("allow")
		got := a.Classify(t.Context(), permit.Subject{Name: "bash", Args: "ls"})
		assert.Equal(t, permit.ClassAllow, got)
		assert.Equal(t, permit.ClassifierSystem, systemOf(sp))
	})
	t.Run("mcp_keeps_strict_prompt", func(t *testing.T) {
		a, sp := adapterFor("allow")
		got := a.Classify(t.Context(), permit.Subject{Name: "mcp__x", Args: `{}`, AllowWrite: true})
		assert.Equal(t, permit.ClassAllow, got)
		assert.Contains(t, systemOf(sp), "You decide whether a single tool invocation may run unattended")
	})
}
