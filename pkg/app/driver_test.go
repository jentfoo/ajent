package app

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/mcp"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/subagent"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

// stubTool is a minimal agent.Tool for guard-chain and registry tests.
type stubTool struct{ name string }

func (s *stubTool) Name() string                { return s.name }
func (s *stubTool) Label(agent.ToolCall) string { return s.name + " ..." }
func (s *stubTool) Description() string         { return "test tool" }
func (s *stubTool) Schema() llm.ToolSchema      { return llm.ToolSchema{Name: s.name} }
func (s *stubTool) Mode() agent.ExecutionMode   { return agent.ModeSerial }
func (s *stubTool) Close()                      {}
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
func (holdBash) Close()                      {}

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

// TestControlLoop covers the idle-editor quit gestures: one Ctrl+C arms the
// window and only a second inside it quits, while Ctrl+D quits outright.
// Mode cycling rides the hub's barrier, so the cycled channel carries the
// barrier mode after each control, giving a test its ordering signal.
func TestControlLoop(t *testing.T) {
	t.Parallel()

	rec := &hintRecorder{} // records what the loop paints on the hint line
	start := func(t *testing.T) (controls chan tui.Control, quit chan struct{}, cycled chan permit.Mode, hub *sessionHub) {
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
		cycled = make(chan permit.Mode, 4)
		ag := agent.New(&agent.State{Tokens: tokens.New(llm.Model{})}, agent.Options{})
		stager := command.NewStager(t.Context(), nil, nil)
		hub = newTestHub(ui)
		hub.ag = ag
		hub.q = newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, stager.WaitReady, stager.TakeReady, nil)
		hub.stager = stager
		// a spy over a production-shaped barrier reports each cycle's resulting
		// mode, giving the test a signal ordered behind earlier controls
		hub.barrier = &modeSpy{Barrier: wiredBarrier(tools.New()), out: cycled}
		go controlLoop(t.Context(), controls, newHintBoard(rec.record), hub, func() { close(quit) })
		return controls, quit, cycled, hub
	}
	// quitted blocks until controlLoop exits: the positive wait is unbounded,
	// a stalled quit fails via the test timeout instead of a fake deadline.
	quitted := func(t *testing.T, quit chan struct{}) {
		t.Helper()
		<-quit
	}

	t.Run("double_press_quits", func(t *testing.T) {
		controls, quit, _, _ := start(t)
		controls <- tui.ControlInterrupt
		controls <- tui.ControlInterrupt
		quitted(t, quit)
	})

	// one press only arms the window: the app stays up until the second arrives
	t.Run("single_press_holds", func(t *testing.T) {
		controls, quit, cycled, _ := start(t)
		controls <- tui.ControlInterrupt
		controls <- tui.ControlModeCycle // ordered behind the press: it has been handled
		assert.Equal(t, permit.ModeAuto, <-cycled)
		assert.Equal(t, []string{"ctrl+c again to quit"}, rec.last())

		select {
		case <-quit:
			t.Error("quit on a single ctrl+c")
		default:
		}
		controls <- tui.ControlInterrupt
		quitted(t, quit)
	})

	// both mode-cycle controls reach the barrier, each with its own direction
	t.Run("mode_cycle_reports_direction", func(t *testing.T) {
		controls, _, cycled, _ := start(t)
		controls <- tui.ControlModeCycle
		assert.Equal(t, permit.ModeAuto, <-cycled)
		controls <- tui.ControlModeCycleBack
		assert.Equal(t, permit.ModeAllowRead, <-cycled)
	})

	// the picker copy control routes without a picker open: silent no-op, never a quit
	t.Run("copy_without_picker_is_silent", func(t *testing.T) {
		controls, quit, cycled, _ := start(t)
		controls <- tui.ControlCopySelection
		controls <- tui.ControlModeCycle // ordered behind: the copy control settled
		assert.Equal(t, permit.ModeAuto, <-cycled)

		select {
		case <-quit:
			t.Error("quit on a picker copy without a picker")
		default:
		}
	})

	t.Run("ctrl_d_quits", func(t *testing.T) {
		controls, quit, _, _ := start(t)
		controls <- tui.ControlEOF
		quitted(t, quit)
	})
}

// modeSpy reports each cycle's resulting mode on out, an ordering signal for
// control-loop tests that drive mode-cycle controls.
type modeSpy struct {
	*permit.Barrier
	out chan permit.Mode
}

func (s *modeSpy) Cycle() permit.Mode {
	m := s.Barrier.Cycle()
	s.out <- m
	return m
}

func (s *modeSpy) Prev() permit.Mode {
	m := s.Barrier.Prev()
	s.out <- m
	return m
}

// TestAbortRunning pins the interrupt's two targets: a running turn and the
// drain's between-turns window. An idle queue with no turn stops nothing.
func TestAbortRunning(t *testing.T) {
	t.Parallel()

	newQueue := func(fake *fakeQueueUI) *steerQueue {
		waitReady, takeReady := stagePair(t)
		return newSteerQueue(fake, func(int) {}, func() {}, waitReady, takeReady, nil)
	}
	idleAgent := func() *agent.Agent {
		return agent.New(&agent.State{Tokens: tokens.New(llm.Model{})}, agent.Options{})
	}
	stager := func() *command.Stager {
		return command.NewStager(t.Context(), nil, agent.NopSink{})
	}

	t.Run("idle_stops_nothing", func(t *testing.T) {
		q := newQueue(&fakeQueueUI{})
		assert.False(t, abortRunning(idleAgent(), q, stager()))
	})

	t.Run("drain_live_stops_between_turns", func(t *testing.T) {
		fake := &fakeQueueUI{}
		q := newQueue(fake)
		q.offer(agent.Input{Text: "running"}, "running", 0) // marks draining, like the pump
		require.True(t, q.offer(agent.Input{Text: "queued"}, "queued", 0))

		assert.True(t, abortRunning(idleAgent(), q, stager()))
		assert.Equal(t, 0, q.pending())
		assert.Equal(t, []string{"queued"}, fake.prepended)
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
	st := &agent.State{Model: llm.Model{Provider: "p", ID: "session"}, Tokens: tokens.New(llm.Model{Provider: "p", ID: "session"})}

	// configured child model resolves through the registry
	t.Run("configured_model_resolves", func(t *testing.T) {
		require.NoError(t, set.SetSession("subagent.model", "p/child"))
		got := resolveSubAgentModel(set, reg, st)
		assert.Equal(t, "child", got.ID)
	})

	// unset falls back to the session's current model
	t.Run("unset_inherits_session", func(t *testing.T) {
		set2, _, _ := config.Load(config.Options{Workspace: t.TempDir()})
		got := resolveSubAgentModel(set2, reg, st)
		assert.Equal(t, "session", got.ID)
	})
}

func TestSubagentPollWait(t *testing.T) {
	// t.Setenv forbids t.Parallel, and a temp AJENT_HOME hides the real user config layer
	t.Setenv(config.EnvHome, t.TempDir())

	set, _, err := config.Load(config.Options{Workspace: t.TempDir()})
	require.NoError(t, err)
	var warns []string
	warn := func(msg string) { warns = append(warns, msg) }

	// unset resolves the compiled-in default silently
	assert.Equal(t, 10*time.Minute, subagentPollWait(set, warn))
	assert.Empty(t, warns)

	t.Run("empty_takes_builtin", func(t *testing.T) {
		warns = nil
		require.NoError(t, set.SetSession("subagent.maxPollWait", ""))
		assert.Zero(t, subagentPollWait(set, warn)) // manager applies its own default
		assert.Empty(t, warns)
	})

	t.Run("parses_duration", func(t *testing.T) {
		warns = nil
		require.NoError(t, set.SetSession("subagent.maxPollWait", "20m"))
		assert.Equal(t, 20*time.Minute, subagentPollWait(set, warn))
		assert.Empty(t, warns)
	})

	t.Run("rejects_garbage", func(t *testing.T) {
		warns = nil
		require.NoError(t, set.SetSession("subagent.maxPollWait", "soon"))
		assert.Zero(t, subagentPollWait(set, warn))
		assert.Len(t, warns, 1)
	})

	t.Run("rejects_negative", func(t *testing.T) {
		warns = nil
		require.NoError(t, set.SetSession("subagent.maxPollWait", "-5m"))
		assert.Zero(t, subagentPollWait(set, warn))
		assert.Len(t, warns, 1)
	})
}

// TestSubagentSinkTurnEnd pins the TurnEnd release: an aborted turn clears a
// queued batch's in-flight marks so the next Flush re-offers it, while a clean
// StopEndTurn leaves them set (no duplicate on a normal turn).
func TestSubagentSinkTurnEnd(t *testing.T) {
	t.Parallel()

	newMgr := func(t *testing.T) (*subagent.Manager, *atomic.Int32) {
		t.Helper()

		var delivered atomic.Int32 // steers accepted by Deliver, Delivered never fires
		mgr := subagent.New(subagent.Options{
			Root:  t.Context(),
			Tools: emptyToolSource{},
			Provider: func(llm.Model) (llm.Provider, error) {
				return &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textTurnRewind("summary")}}}, nil
			},
			Model:     func() llm.Model { return llm.Model{} },
			Reasoning: func() llm.ReasoningConfig { return llm.ReasoningConfig{} },
			Parent:    func() *tokens.Accounting { return tokens.New(llm.Model{}) },
			Notice:    func(string) {},
			Deliver:   func(agent.Input) bool { delivered.Add(1); return true },
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

	startTask := func(t *testing.T, m *subagent.Manager) string {
		t.Helper()
		for _, tool := range m.Tools() { // spawn through the agent_start tool, no exported Start
			if tool.Name() != "agent_start" {
				continue
			}
			res, err := tool.Execute(t.Context(), agent.ToolCall{
				Input: json.RawMessage(`{"task":"x","instructions":""}`),
			}, agent.NewOutput(agent.NopSink{}, "c"))
			require.NoError(t, err)
			dets, ok := res.Details.(map[string]string)
			require.True(t, ok, "start result missing details")
			return dets["id"]
		}
		t.Fatal("agent_start not registered")
		return ""
	}

	t.Run("abort_releases_marks", func(t *testing.T) {
		mgr, delivered := newMgr(t)
		id := startTask(t, mgr)
		settle(t, mgr, id)

		sink := subagentSink{mgr: mgr}
		// the completed job is queued just after StatusDone publishes, flush until its
		// steer lands so this never races enqueue.
		require.Eventually(t, func() bool {
			if delivered.Load() == 0 {
				mgr.Flush() // steer accepted, the mark is now in flight
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
		mgr, delivered := newMgr(t)
		id := startTask(t, mgr)
		settle(t, mgr, id)

		sink := subagentSink{mgr: mgr}
		require.Eventually(t, func() bool {
			if delivered.Load() == 0 {
				mgr.Flush() // steer accepted, the mark is now in flight
			}
			return delivered.Load() >= 1
		}, 2*time.Second, 5*time.Millisecond)

		sink.TurnEnd(agent.TurnResult{Stop: llm.StopEndTurn}) // no release
		mgr.Flush()                                           // still in flight, nothing may re-offer
		assert.EqualValues(t, 1, delivered.Load())
	})
}

func TestAppendHistory(t *testing.T) {
	t.Parallel()

	// pins the submit-time recall decision: the pasted-secret exclusion reads
	// line.Rest (the text reaching the transcript), so a raw line's escape space
	// cannot land an excluded value in the durable store, while every other kind
	// keeps its typed form for round-tripping.
	newHist := func(t *testing.T) *session.EditorHistory {
		t.Helper()

		s := session.StoreAt(filepath.Join(t.TempDir(), "sessions"))
		h, err := session.NewEditorHistory(s, t.TempDir(), secretPrefix)
		require.NoError(t, err)
		return h
	}

	for _, tc := range []struct {
		name string
		msg  string
		want []string // the recall entries the submission leaves behind
	}{
		{"command_hidden", "/model x", nil},
		{"shell_recorded", "!ls -la", []string{"!ls -la"}},
		{"prompt_recorded", "hello world", []string{"hello world"}},
		{"escaped_prompt_round_trips", " /notacommand", []string{" /notacommand"}},
		{"plain_secret_skipped", "secret:hunter2", nil},
		{"escaped_secret_skipped", " secret:hunter2", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHist(t)
			appendHistory(h, secretPrefix, tc.msg, command.ParseLine(tc.msg))
			assert.Equal(t, tc.want, h.Recent())
		})
	}
}

func TestLineRouterRoute(t *testing.T) {
	t.Parallel()

	// pins the shared dispatch: prompts and commands queue for the pump, shell
	// lines stage instead of queueing, and editor-coupled side effects (recall
	// history, clipboard image tokens) stay behind fromEditor.
	newUI := func(t *testing.T) *tui.UI {
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
		return ui
	}
	newHist := func(t *testing.T) *session.EditorHistory {
		t.Helper()

		s := session.StoreAt(filepath.Join(t.TempDir(), "sessions"))
		h, err := session.NewEditorHistory(s, t.TempDir(), secretPrefix)
		require.NoError(t, err)
		return h
	}
	// only the shell case runs its stager, a sinkless one keeps an accidental
	// Run from panicking on a notice
	idleStager := func() *command.Stager {
		return command.NewStager(t.Context(), nil, agent.NopSink{})
	}
	armed := func(gate *typingGate) bool {
		gate.mu.Lock()
		defer gate.mu.Unlock()
		return gate.inFlight
	}

	t.Run("prompt_queues_for_pump", func(t *testing.T) {
		var lines []pumpLine
		gate := &typingGate{}
		r := &lineRouter{ui: wrapUI(t, newUI(t)), gate: gate, hist: nil,
			send:   func(pl pumpLine) { lines = append(lines, pl) },
			stager: idleStager()}

		r.route("hello world", true)

		require.Len(t, lines, 1)
		assert.Equal(t, command.KindPrompt, lines[0].kind)
		assert.Equal(t, "hello world", lines[0].rest)
		assert.True(t, armed(gate), "a routed prompt arms the handoff")
	})

	t.Run("command_queues_for_pump", func(t *testing.T) {
		var lines []pumpLine
		r := &lineRouter{ui: wrapUI(t, newUI(t)), gate: &typingGate{}, hist: nil,
			send:   func(pl pumpLine) { lines = append(lines, pl) },
			stager: idleStager()}

		r.route("/model x", false)

		require.Len(t, lines, 1)
		assert.Equal(t, command.KindCommand, lines[0].kind)
		assert.Equal(t, "model x", lines[0].rest)
	})

	t.Run("shell_stages_without_queue", func(t *testing.T) {
		reg, err := tools.Builtins(tools.Options{Cwd: t.TempDir(), SessionID: "router-shell"})
		require.NoError(t, err)
		stager := command.NewStager(t.Context(), reg, agent.NopSink{})
		t.Cleanup(stager.Cancel) // the sleep must not outlive the test
		var lines []pumpLine
		gate := &typingGate{}
		r := &lineRouter{ui: wrapUI(t, newUI(t)), gate: gate, hist: nil,
			send:   func(pl pumpLine) { lines = append(lines, pl) },
			stager: stager}

		r.route("!sleep 5", false)

		require.Eventually(t, stager.Pending, time.Second, time.Millisecond)
		assert.Empty(t, lines)
		assert.False(t, armed(gate), "a staged shell never arms the prompt handoff")
	})

	t.Run("editor_source_records_history", func(t *testing.T) {
		h := newHist(t)
		var lines []pumpLine
		r := &lineRouter{ui: wrapUI(t, newUI(t)), gate: &typingGate{}, hist: h,
			send:   func(pl pumpLine) { lines = append(lines, pl) },
			stager: idleStager()}

		r.route("remember this", true)

		assert.Equal(t, []string{"remember this"}, h.Recent())
	})

	t.Run("external_source_skips_editor", func(t *testing.T) {
		h := newHist(t)
		var lines []pumpLine
		r := &lineRouter{ui: wrapUI(t, newUI(t)), gate: &typingGate{}, hist: h,
			send:   func(pl pumpLine) { lines = append(lines, pl) },
			stager: idleStager()}

		r.route("[image #3] describe this", false)

		require.Len(t, lines, 1)
		assert.Equal(t, "[image #3] describe this", lines[0].rest) // token stays literal
		assert.Empty(t, lines[0].blocks)
		assert.Empty(t, h.Recent())
	})
}

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

func TestMCPConfigDisabledToolsEnableViaSlashTools(t *testing.T) {
	reg := tools.New()
	reg.RegisterState("builtin", &stubTool{name: "read"}, tools.StateEnabled)

	var disabled bool
	mgr := mcp.New(map[string]mcp.ServerConfig{
		"fake": {Command: buildFakeMCPServer(t), Enabled: &disabled},
	}, mcp.Options{Registrar: registryAdapter{reg}, Notice: func(string, bool) {}})

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

func TestMCPConfigDisabledToolsResumeRestoresEnablement(t *testing.T) {
	reg := tools.New()
	reg.RegisterState("builtin", &stubTool{name: "read"}, tools.StateEnabled)

	var disabled bool
	mgr := mcp.New(map[string]mcp.ServerConfig{
		"fake": {Command: buildFakeMCPServer(t), Enabled: &disabled},
	}, mcp.Options{
		Registrar: registryAdapter{reg},
		Notice:    func(string, bool) {},
		Restore:   []string{"read", "fake__tool_00"}, // persisted from a prior session
	})

	mgr.LoadOnFirstMessage(t.Context())
	t.Cleanup(mgr.Close)

	assert.Contains(t, reg.Names(), "fake__tool_00")
}

var (
	fakeMu       sync.Mutex
	fakeMCPPath  string // shared fakeserver binary, compiled on first use
	fakeMCPUsers int    // tests holding it; the last release removes its dir
)

// buildFakeMCPServer retains the pkg/mcp fakeserver binary for the calling
// test's lifetime, compiled once per run. The lock spans the whole build:
// parallel first-callers wait and reuse rather than leak copies; t.Fatalf under
// it is safe because Goexit runs the deferred Unlock. Cleanup runs after each
// test's own (LIFO): spawned servers exit before dir removal.
func buildFakeMCPServer(t *testing.T) string {
	t.Helper()

	fakeMu.Lock()
	defer fakeMu.Unlock()

	if fakeMCPPath == "" {
		// resolved off this file, never the test's working directory
		_, here, _, _ := runtime.Caller(0)
		src := filepath.Join(filepath.Dir(here), "..", "mcp", "testdata", "fakeserver")
		dir, err := os.MkdirTemp("", "ajent-mcp-fake-")
		if err != nil {
			t.Fatalf("temp dir for fakeserver: %v", err)
		}
		out := filepath.Join(dir, "fakeserver")
		cmd := exec.CommandContext(t.Context(), "go", "build", "-o", out, src)
		if b, err := cmd.CombinedOutput(); err != nil {
			_ = os.RemoveAll(dir)
			t.Fatalf("build fakeserver: %v\n%s", err, b)
		}
		fakeMCPPath = out
	}
	fakeMCPUsers++

	t.Cleanup(func() {
		fakeMu.Lock()
		defer fakeMu.Unlock()
		fakeMCPUsers--
		if fakeMCPUsers == 0 && fakeMCPPath != "" {
			_ = os.RemoveAll(filepath.Dir(fakeMCPPath))
			fakeMCPPath = ""
		}
	})
	return fakeMCPPath
}

func TestGuardRegisteredAgainstRegistry(t *testing.T) {
	reg := tools.New()
	b := wiredBarrier(tools.New())
	reg.InstallGate([]tools.Guard{b.Guard()}, b.Asker())

	tool := &stubTool{name: "write"}
	reg.RegisterState("builtin", tool, tools.StateEnabled)

	g, ok := reg.Get("write")
	require.True(t, ok)

	res, err := g.Execute(t.Context(), agent.ToolCall{
		ID: "c1", Name: "write", Input: []byte(`{}`),
	}, agent.NewOutput(agent.NopSink{}, "c"))
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
