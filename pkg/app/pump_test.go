package app

import (
	"context"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tui"
)

func TestRunPump(t *testing.T) {
	t.Parallel()

	t.Run("serves_then_exits_on_cancel", func(t *testing.T) {
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

		cmds := command.NewRegistry()
		handled := make(chan string, 4)
		cmds.Register(command.Command{
			Name: "ping", Description: "test",
			Handler: func(_ context.Context, arg string, _ command.Console) error {
				handled <- arg
				return nil
			},
		})
		console := &uiConsole{ui: ui, commands: cmds}
		pump := make(chan pumpLine, 4)
		var started bool
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		var drain drainWaiter
		go func() {
			runPump(ctx, pump, nil, console, command.NewStager(ctx, nil, nil), nil,
				false, ui, &started, func() {},
				newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}),
				nil, &agent.State{}, nil, &sync.Once{}, func() {}, planHooks{}, &drain)
			close(done)
		}()

		pump <- pumpLine{kind: command.KindCommand, rest: "ping hi"}
		select {
		case got := <-handled:
			assert.Equal(t, "hi", got)
		case <-time.After(5 * time.Second):
			t.Fatal("command never ran")
		}

		cancel() // the driver's quit path: no close(pump), just the root context
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("runPump did not exit on cancel")
		}
		// the channel outlives runPump, so a late sender can never hit a close
		require.NotPanics(t, func() { pump <- pumpLine{kind: command.KindCommand, rest: "ping late"} })
	})
}

func TestDrainWaiterWait(t *testing.T) {
	t.Parallel()

	t.Run("no_drain_returns_at_once", func(t *testing.T) {
		var d drainWaiter
		d.wait(time.Hour) // must not block: nothing ever launched
	})

	t.Run("exiting_drain_releases", func(t *testing.T) {
		var d drainWaiter
		d.launch(func() {})
		require.Eventually(t, func() bool {
			d.mu.Lock()
			defer d.mu.Unlock()
			select {
			case <-d.done:
				return true
			default:
				return false
			}
		}, time.Second, time.Millisecond)
		d.wait(time.Hour)
	})

	t.Run("stuck_drain_is_bounded", func(t *testing.T) {
		var d drainWaiter
		blocked := make(chan struct{})
		d.launch(func() { <-blocked })
		d.wait(20 * time.Millisecond)
		select {
		case <-blocked:
			t.Fatal("wait must return before the drain exits")
		default:
		}
		close(blocked)
	})
}

// TestStartDrainInterruptLandsFill pins the shutdown invariant: interrupt plus
// a bounded drain wait leaves no dangling tool_use in state.
func TestStartDrainInterruptLandsFill(t *testing.T) {
	t.Parallel()

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
	go func() { _, _ = io.Copy(io.Discard, outR) }()
	ui, err := tui.New(tui.Options{In: inR, Out: outW, Mode: tui.ModePlain})
	require.NoError(t, err)
	t.Cleanup(ui.Close)

	release := make(chan struct{})
	entered := make(chan struct{})
	set := singleToolSet{tool: holdBash{release: release, entered: entered}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: mainToolTurn()}}}
	st := &agent.State{Model: llm.Model{ID: "test"}, Reasoning: llm.ReasoningConfig{}}
	ag := agent.New(st, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) { return p, nil },
		Sinks:    []agent.Sink{agent.NopSink{}},
		Tools:    set,
		Env:      agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
	})

	q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {})
	q.offer(agent.Input{Text: "run"}, "run", 0) // marks draining, like the pump
	var started bool
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var drain drainWaiter
	startDrain(ctx, ui, false, ag, q, []agent.Input{{Text: "run"}}, &started, planHooks{}, &drain)

	<-entered // the turn is pinned inside its tool call

	ag.Interrupt()
	drain.wait(5 * time.Second)

	require.True(t, started)
	assert.False(t, ag.Running())
	// the dangling tool_use must be answered: a tool_result for c1 follows it
	var answered bool
	for _, m := range st.Messages {
		for _, b := range m.Content {
			if tr, ok := b.(llm.ToolResultBlock); ok && tr.CallID == "c1" {
				answered = true
			}
		}
	}
	assert.True(t, answered, "the abort fill must land before teardown reads state")
}
