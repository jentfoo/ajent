package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/refs"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitCommands(t *testing.T) {
	t.Parallel()

	t.Run("absent_without_controller", func(t *testing.T) {
		assert.Nil(t, initCommands(nil))
	})

	t.Run("registers_beside_builtins", func(t *testing.T) {
		h := newInitHarness(t)
		cmds := command.NewRegistry()
		reg, _ := llm.NewRegistry(llm.File{}, nil, llm.RegistryOptions{})
		console := &uiConsole{reg: reg, st: &agent.State{}, tools: tools.New()}

		baseline := command.NewRegistry()
		command.RegisterBuiltins(baseline, console)
		registerCommands(cmds, console, initCommands(h.ctl)...)

		assert.Subset(t, cmds.Names(), baseline.Names())
		assert.Contains(t, cmds.Names(), "init")
	})
}

func TestInitControllerStart(t *testing.T) {
	t.Parallel()

	t.Run("hands_survey_to_pump", func(t *testing.T) {
		h := newInitHarness(t)
		h.ctl.start()

		line := h.awaitPump(t)
		require.NotNil(t, line.input)
		assert.Equal(t, command.KindPrompt, line.kind)
		assert.True(t, line.input.Injected)
		assert.NotEmpty(t, line.input.Before) // the survey's tool pairs ride along
		assert.Contains(t, line.input.Text, "AGENTS.md")
	})

	t.Run("reports_missing_subagents", func(t *testing.T) {
		h := newInitHarness(t)
		reg, err := tools.Builtins(tools.Options{Cwd: h.ctl.deps.cwd}) // read, but no agent_*
		require.NoError(t, err)
		h.ctl.deps.toolsReg = reg
		h.ctl = newInitController(h.ctl.deps)
		h.ctl.start()

		require.Eventually(t, func() bool { return len(h.notices()) > 0 }, time.Second, time.Millisecond)
		assert.Contains(t, h.notices()[0], "sub-agents are unavailable")
		assert.Empty(t, h.pump) // nothing reaches the model
	})

	t.Run("reports_missing_read", func(t *testing.T) {
		h := newInitHarness(t)
		h.ctl.deps.toolsReg = tools.New()
		h.ctl = newInitController(h.ctl.deps)
		h.ctl.start()

		require.Eventually(t, func() bool { return len(h.notices()) > 0 }, time.Second, time.Millisecond)
		assert.Contains(t, h.notices()[0], "read tool is unavailable")
	})

	t.Run("one_survey_at_a_time", func(t *testing.T) {
		h := newInitHarness(t)
		h.hold()

		h.ctl.start()
		h.awaitPolling(t)

		h.ctl.start() // refused while the first is in flight
		require.Eventually(t, func() bool { return len(h.notices()) > 0 }, time.Second, time.Millisecond)
		assert.Contains(t, strings.Join(h.notices(), "\n"), "already running")
		assert.Empty(t, h.pump) // the second start never produced a turn
	})

	t.Run("abort_cancels_in_flight", func(t *testing.T) {
		h := newInitHarness(t)
		h.hold()

		assert.False(t, h.ctl.abort()) // nothing running yet
		h.ctl.start()
		h.awaitPolling(t)
		assert.True(t, h.ctl.running())

		assert.True(t, h.ctl.abort())
		require.Eventually(t, func() bool { return !h.ctl.running() }, 5*time.Second, time.Millisecond)
		assert.Contains(t, strings.Join(h.notices(), "\n"), "cancelled")
		assert.Empty(t, h.pump) // an aborted survey never reaches the model
	})

	t.Run("second_run_reuses_no_ids", func(t *testing.T) {
		h := newInitHarness(t)
		h.ctl.start()
		first := h.awaitPump(t)
		// the slot frees just after the push, so wait rather than race the refusal
		require.Eventually(t, func() bool { return !h.ctl.running() }, 5*time.Second, time.Millisecond)
		h.ctl.start()
		second := h.awaitPump(t)

		// Before stays in State, so a repeated tool_use id 400s every later request
		ids := callIDs(first.input.Before)
		require.NotEmpty(t, ids)
		for _, id := range callIDs(second.input.Before) {
			assert.NotContains(t, ids, id)
		}
	})

	t.Run("tracks_only_its_own_children", func(t *testing.T) {
		h := newInitHarness(t)
		h.ctl.start()
		h.awaitPump(t)

		h.ctl.mu.Lock()
		spawned := h.ctl.spawned
		h.ctl.mu.Unlock()
		assert.NotEmpty(t, spawned) // abort stops these by id, never StopAll
	})

	t.Run("nil_controller_never_aborts", func(t *testing.T) {
		var ctl *initController
		assert.False(t, ctl.abort())
	})
}

func TestInitWatch(t *testing.T) {
	t.Parallel()

	// touch sets path's mtime explicitly; two writes can share a coarse timestamp.
	touch := func(t *testing.T, path string, at time.Time) {
		t.Helper()
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))
		require.NoError(t, os.Chtimes(path, at, at))
	}

	t.Run("written_file_notifies", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), agentsFileName)
		touch(t, path, time.Now().Add(-time.Hour))

		var got []string
		w := &initWatch{notify: func(msg string, _ tui.Level) { got = append(got, msg) }}
		w.arm(path)
		touch(t, path, time.Now())
		w.TurnEnd(agent.TurnResult{})

		require.Len(t, got, 1)
		assert.Contains(t, got[0], "applies on the next start")
	})

	t.Run("denied_write_stays_quiet", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), agentsFileName)
		touch(t, path, time.Now().Add(-time.Hour))

		var got []string
		w := &initWatch{notify: func(msg string, _ tui.Level) { got = append(got, msg) }}
		w.arm(path)
		w.TurnEnd(agent.TurnResult{}) // the barrier refused; the file never changed
		assert.Empty(t, got)
	})

	t.Run("survives_an_unrelated_turn", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), agentsFileName)
		touch(t, path, time.Now().Add(-time.Hour))

		var got []string
		w := &initWatch{notify: func(msg string, _ tui.Level) { got = append(got, msg) }}
		w.arm(path)
		// a turn already running when the survey landed ends first and wrote nothing;
		// disarming here lost the notice entirely
		w.TurnEnd(agent.TurnResult{})
		assert.Empty(t, got)

		touch(t, path, time.Now())
		w.TurnEnd(agent.TurnResult{}) // the survey's own turn
		assert.Len(t, got, 1)
	})

	t.Run("fires_once", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), agentsFileName)
		var got []string
		w := &initWatch{notify: func(msg string, _ tui.Level) { got = append(got, msg) }}
		w.arm(path)
		touch(t, path, time.Now())
		w.TurnEnd(agent.TurnResult{})
		w.TurnEnd(agent.TurnResult{}) // a later, unrelated turn says nothing
		assert.Len(t, got, 1)
	})

	t.Run("unarmed_and_nil", func(t *testing.T) {
		w := &initWatch{notify: func(string, tui.Level) { t.Error("unarmed watch notified") }}
		w.TurnEnd(agent.TurnResult{})

		var nilWatch *initWatch
		nilWatch.arm("anything") // the driver may hand /init no watch at all
	})
}

// initHarness drives an initController over a temp project with stub sub-agent
// tools, so a survey completes without a provider.
type initHarness struct {
	ctl  *initController
	pump chan pumpLine

	// held blocks agent_poll until the test releases it; polling closes once to
	// announce that a survey really reached stage 2.
	held    chan struct{}
	polling chan struct{}
	once    sync.Once

	mu   sync.Mutex
	msgs []string
}

// hold makes agent_poll wait, so a test can act while a survey is in flight.
func (h *initHarness) hold() { h.held = make(chan struct{}) }

// awaitPolling blocks until the survey has entered agent_poll.
func (h *initHarness) awaitPolling(t *testing.T) {
	t.Helper()
	select {
	case <-h.polling:
	case <-time.After(5 * time.Second):
		t.Fatalf("survey never polled; notices: %v", h.notices())
	}
}

func newInitHarness(t *testing.T) *initHarness {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# demo\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "pkg"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pkg", "a.go"), []byte("package a\n"), 0o644))

	reg, err := tools.Builtins(tools.Options{Cwd: dir})
	require.NoError(t, err)

	h := &initHarness{pump: make(chan pumpLine, 4), polling: make(chan struct{})}
	reg.RegisterFrom(tools.SourceBuiltin, &initStub{name: "agent_start", run: func(context.Context, agent.ToolCall) agent.ToolResult {
		return agent.ToolResult{
			Content: llm.BlockList{llm.TextBlock{Text: "started"}},
			Details: map[string]string{"id": "sub-1"},
		}
	}}, true)
	reg.RegisterFrom(tools.SourceBuiltin, &initStub{name: "agent_poll", run: func(ctx context.Context, call agent.ToolCall) agent.ToolResult {
		h.once.Do(func() { close(h.polling) })
		if h.held != nil {
			select {
			case <-h.held:
			case <-ctx.Done(): // the real agent_poll releases on an interrupted turn
				return agent.ToolResult{
					Content: llm.BlockList{llm.TextBlock{Text: "poll interrupted"}},
					Details: map[string]string{"id": "sub-1", "status": "running"},
				}
			}
		}
		return agent.ToolResult{
			Content: llm.BlockList{llm.TextBlock{Text: "summary"}},
			Details: map[string]string{"id": "sub-1", "status": "done"},
		}
	}}, true)

	h.ctl = newInitController(initDeps{
		cwd: dir, toolsReg: reg, sink: agent.NopSink{}, pump: h.pump,
		notify: func(msg string, _ tui.Level) {
			h.mu.Lock()
			h.msgs = append(h.msgs, msg)
			h.mu.Unlock()
		},
		watch: &initWatch{notify: func(string, tui.Level) {}},
	})
	require.NotNil(t, h.ctl)
	return h
}

// awaitPump returns the line the survey handed back, failing if none arrives.
func (h *initHarness) awaitPump(t *testing.T) pumpLine {
	t.Helper()
	select {
	case line := <-h.pump:
		return line
	case <-time.After(5 * time.Second):
		t.Fatalf("survey never reached the pump; notices: %v", h.notices())
		return pumpLine{}
	}
}

// callIDs returns the tool_use id of every call block in msgs.
func callIDs(msgs []llm.Message) []string {
	var out []string
	for _, m := range msgs {
		for _, b := range m.Content {
			if cb, ok := b.(llm.ToolCallBlock); ok {
				out = append(out, cb.ID)
			}
		}
	}
	return out
}

func (h *initHarness) notices() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.msgs...)
}

// running reports whether a survey holds the controller's single slot.
func (c *initController) running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cancel != nil
}

// initStub is a scriptable stand-in for the agent_* tools.
type initStub struct {
	name string
	run  func(ctx context.Context, call agent.ToolCall) agent.ToolResult
}

func (t *initStub) Name() string                { return t.name }
func (t *initStub) Label(agent.ToolCall) string { return t.name }
func (t *initStub) Description() string         { return "stub" }
func (t *initStub) Schema() llm.ToolSchema      { return llm.ToolSchema{Name: t.name} }
func (t *initStub) Mode() agent.ExecutionMode   { return agent.ModeParallel }
func (t *initStub) Execute(ctx context.Context, call agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	return t.run(ctx, call), nil
}

func TestPromptInput(t *testing.T) {
	t.Parallel()

	staged := []llm.Message{llm.Text(llm.RoleUser, "staged shell result")}
	surveyed := []llm.Message{llm.Text(llm.RoleUser, "survey pair")}

	t.Run("assembled_input_skips_expansion", func(t *testing.T) {
		var armed int
		line := pumpLine{
			kind:   command.KindPrompt,
			rest:   "/init",
			input:  &agent.Input{Text: "distill this", Before: surveyed, Injected: true},
			onTurn: func() { armed++ },
		}
		// a nil expander proves this path never reaches @ expansion
		in, echo := promptInput(line, staged, nil, func(string) { t.Error("unexpected notice") })

		assert.Equal(t, "distill this", in.Text)
		assert.True(t, in.Injected)
		assert.Equal(t, "/init", echo) // a label, not the whole instruction
		assert.Equal(t, 1, armed)      // armed as it becomes a turn
		// staged shell results still land ahead of the survey's own pairs
		require.Len(t, in.Before, 2)
		assert.Equal(t, staged[0], in.Before[0])
		assert.Equal(t, surveyed[0], in.Before[1])
	})

	t.Run("assembled_input_without_hook", func(t *testing.T) {
		line := pumpLine{kind: command.KindPrompt, input: &agent.Input{Text: "x"}}
		in, echo := promptInput(line, nil, nil, func(string) {})
		assert.Equal(t, "x", in.Text)
		assert.Empty(t, echo)
	})

	t.Run("typed_prompt_expands", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644))
		reg, err := tools.Builtins(tools.Options{Cwd: dir})
		require.NoError(t, err)
		expander := refs.NewExpander(reg, agent.NopSink{}, tools.PathPolicy{Cwd: dir})

		line := pumpLine{kind: command.KindPrompt, rest: "look at @a.go"}
		in, echo := promptInput(line, staged, expander, func(string) {})

		assert.Contains(t, in.Text, "@a.go")
		assert.Equal(t, in.Text, echo) // a typed prompt echoes itself
		assert.False(t, in.Injected)
		require.NotEmpty(t, in.Before)
		assert.Equal(t, staged[0], in.Before[0]) // staged results stay first
		assert.Contains(t, callIDs(in.Before), "ref-a.go")
	})

	t.Run("expansion_notices_surface", func(t *testing.T) {
		dir := t.TempDir()
		reg, err := tools.Builtins(tools.Options{Cwd: dir})
		require.NoError(t, err)
		expander := refs.NewExpander(reg, agent.NopSink{}, tools.PathPolicy{Cwd: dir})

		var warned []string
		line := pumpLine{kind: command.KindPrompt, rest: "see @missing.go"}
		_, _ = promptInput(line, nil, expander, func(n string) { warned = append(warned, n) })
		require.Len(t, warned, 1)
		assert.Contains(t, warned[0], "missing.go")
	})
}

func TestSubmitEstimate(t *testing.T) {
	t.Parallel()

	text := agent.Input{Text: "a short prompt"}
	textOnly := submitEstimate(text)
	assert.Positive(t, textOnly)

	withBefore := text
	withBefore.Before = []llm.Message{llm.Text(llm.RoleUser, strings.Repeat("payload ", 200))}
	// injected pairs are the larger half of a survey; the bucket must count them
	assert.Greater(t, submitEstimate(withBefore), textOnly)
}
