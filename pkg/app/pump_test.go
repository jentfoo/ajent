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
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tui"
)

// newPumpTestUI returns a plain-mode UI over pipes with its output discarded.
func newPumpTestUI(t *testing.T) *tui.UI {
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

// scriptedAgent returns an agent over p with a throwaway state, for pump and drain tests.
func scriptedAgent(p *llm.ScriptedProvider) (*agent.Agent, *agent.State) {
	st := &agent.State{Model: llm.Model{ID: "test"}, Reasoning: llm.ReasoningConfig{}, Tokens: tokens.New(llm.Model{ID: "test"})}
	ag := agent.New(st, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) { return p, nil },
		Sinks:    []agent.Sink{agent.NopSink{}},
	})
	return ag, st
}

func TestRunPump(t *testing.T) {
	t.Parallel()

	t.Run("serves_then_exits_on_cancel", func(t *testing.T) {
		ui := newPumpTestUI(t)

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
		started := newStartedFlag(false)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		var drain drainWaiter
		go func() {
			runPump(ctx, pump, nil, console, command.NewStager(ctx, nil, nil), nil,
				false, ui, started, func() {},
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

	t.Run("exit_fires_wake", func(t *testing.T) {
		var d drainWaiter
		woke := make(chan struct{})
		d.wake = func() { close(woke) }
		d.launch(func() {})
		select {
		case <-woke:
		case <-time.After(5 * time.Second):
			t.Fatal("drain exit never fired wake")
		}
	})
}

// TestRunPumpWakeFlushesDeferred pins the wake path: a drain exit lands queued
// state writes without dispatching a line
func TestRunPumpWakeFlushesDeferred(t *testing.T) {
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

	cmds := command.NewRegistry()
	handled := make(chan string, 4)
	cmds.Register(command.Command{
		Name: "ping", Description: "test",
		Handler: func(_ context.Context, arg string, _ command.Console) error {
			handled <- arg
			return nil
		},
	})
	q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {})
	st := &agent.State{Model: llm.Model{ID: "m", Provider: "p"}}
	console := &uiConsole{ui: ui, commands: cmds, st: st, q: q}
	applied := make(chan struct{})
	console.deferred = append(console.deferred, func() { close(applied) })

	pump := make(chan pumpLine, 4)
	started := newStartedFlag(false)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan struct{})
	var drain drainWaiter
	go func() {
		runPump(ctx, pump, nil, console, command.NewStager(ctx, nil, nil), nil,
			false, ui, started, func() {},
			q, nil, st, nil, &sync.Once{}, func() {}, planHooks{}, &drain)
		close(done)
	}()

	pump <- pumpLine{wake: true}
	select {
	case <-applied:
	case <-time.After(5 * time.Second):
		t.Fatal("the wake line never flushed the deferred write")
	}
	select {
	case <-handled:
		t.Fatal("a wake line must not dispatch a command")
	default:
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runPump did not exit on cancel")
	}
}

func TestStartDrainParkedGate(t *testing.T) {
	t.Parallel()

	t.Run("parked_queue_holds", func(t *testing.T) {
		ui := newPumpTestUI(t)
		p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textTurn("planned")}}}
		ag, _ := scriptedAgent(p)

		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {})
		q.offer(agent.Input{Text: "seed"}, "seed", 0) // marks draining, like the pump
		require.True(t, q.offer(agent.Input{Text: "steering"}, "steering", 0))
		started := newStartedFlag(false)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		var drain drainWaiter
		hooks := planHooks{
			advance: func(context.Context) (agent.Input, bool) { return agent.Input{}, false },
			parked:  func() bool { return true },
		}
		startDrain(ctx, ui, false, ag, q, []agent.Input{{Text: "plan"}}, started, hooks, &drain)
		drain.wait(5 * time.Second)

		require.True(t, started.marked())
		require.Len(t, p.Requests(), 1) // the queued prompt never ran as a turn
		assert.Equal(t, 1, q.pending())
		assert.False(t, q.offer(agent.Input{Text: "next"}, "next", 0)) // idle again
	})

	t.Run("unparked_queue_delivers", func(t *testing.T) {
		ui := newPumpTestUI(t)
		p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
			{Events: textTurn("planned")},
			{Events: textTurn("steered")},
		}}
		ag, _ := scriptedAgent(p)

		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {})
		q.offer(agent.Input{Text: "seed"}, "seed", 0) // marks draining, like the pump
		require.True(t, q.offer(agent.Input{Text: "steering"}, "steering", 0))
		started := newStartedFlag(false)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		var drain drainWaiter
		startDrain(ctx, ui, false, ag, q, []agent.Input{{Text: "plan"}}, started, planHooks{}, &drain)
		drain.wait(5 * time.Second)

		require.Len(t, p.Requests(), 2) // no workflow: steering delivers as before
		assert.Contains(t, texts(p.Requests()[1]), "steering")
	})
}

func TestRunPumpParkedHold(t *testing.T) {
	t.Parallel()

	ui := newPumpTestUI(t)
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: textTurn("kickoff reply")},
		{Events: textTurn("held reply")},
	}}
	ag, st := scriptedAgent(p)
	console := &uiConsole{ui: ui, commands: command.NewRegistry()}
	q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {})

	var captured bool
	hooks := planHooks{
		beforePrompt: func(_ context.Context, in agent.Input) (agent.Input, bool) {
			if in.Injected {
				return in, false // the gate never captures injected input
			}
			captured = true // the gated submit starts the implementor's turn
			return agent.Input{Text: "kickoff", Injected: true}, true
		},
		parked: func() bool { return !captured },
	}

	pump := make(chan pumpLine, 4)
	started := newStartedFlag(false)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var drain drainWaiter
	go runPump(ctx, pump, ag, console, command.NewStager(ctx, nil, nil), nil,
		false, ui, started, func() {}, q, nil, st, nil, &sync.Once{}, func() {}, hooks, &drain)

	pump <- pumpLine{kind: command.KindPrompt, rest: "note", input: &agent.Input{Text: "note", Injected: true}}
	require.Eventually(t, func() bool { return q.pending() == 1 }, time.Second, time.Millisecond)
	assert.Empty(t, p.Requests()) // held at the gate, no turn started

	pump <- pumpLine{kind: command.KindPrompt, rest: "the plan", input: &agent.Input{Text: "the plan"}}
	require.Eventually(t, func() bool {
		return len(p.Requests()) == 2 && q.pending() == 0
	}, 5*time.Second, time.Millisecond)
	assert.Contains(t, texts(p.Requests()[0]), "kickoff") // the gated submit ran first
	assert.Contains(t, texts(p.Requests()[1]), "note")    // held input landed behind it
}

func TestStartDrainInterruptLandsFill(t *testing.T) {
	t.Parallel()

	ui := newPumpTestUI(t)

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
	started := newStartedFlag(false)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var drain drainWaiter
	startDrain(ctx, ui, false, ag, q, []agent.Input{{Text: "run"}}, started, planHooks{}, &drain)

	<-entered // the turn is pinned inside its tool call

	ag.Interrupt()
	drain.wait(5 * time.Second)

	require.True(t, started.marked())
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
