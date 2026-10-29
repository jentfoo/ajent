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
	"github.com/jentfoo/ajent/pkg/refs"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
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
		console := &uiConsole{ui: wrapUI(t, ui), commands: cmds}
		pump := make(chan pumpLine, 4)
		starts := newStartState(false)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		var drain drainWaiter
		stager := command.NewStager(ctx, nil, nil)
		go func() {
			runPump(ctx, pump, nil, console, stager, nil,
				false, wrapUI(t, ui), starts, func() {},
				newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, stager.WaitReady, stager.TakeReady),
				nil, &agent.State{Tokens: tokens.New(llm.Model{})}, nil, &sync.Once{}, func() {}, planHooks{}, &drain)
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
	waitReady, takeReady := stagePair(t)
	q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady)
	st := &agent.State{Model: llm.Model{ID: "m", Provider: "p"}, Tokens: tokens.New(llm.Model{ID: "m", Provider: "p"})}
	console := &uiConsole{ui: wrapUI(t, ui), commands: cmds, st: st, q: q}
	applied := make(chan struct{})
	console.deferred = append(console.deferred, func() { close(applied) })

	pump := make(chan pumpLine, 4)
	starts := newStartState(false)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan struct{})
	var drain drainWaiter
	go func() {
		runPump(ctx, pump, nil, console, command.NewStager(ctx, nil, nil), nil,
			false, wrapUI(t, ui), starts, func() {},
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

		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady)
		q.offer(agent.Input{Text: "seed"}, "seed", 0) // marks draining, like the pump
		require.True(t, q.offer(agent.Input{Text: "steering"}, "steering", 0))
		starts := newStartState(false)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		var drain drainWaiter
		hooks := planHooks{
			advance: func(context.Context) (agent.Input, bool) { return agent.Input{}, false },
			parked:  func() bool { return true },
		}
		startDrain(ctx, wrapUI(t, ui), false, ag, q, []agent.Input{{Text: "plan"}}, starts, hooks, &drain)
		drain.wait(5 * time.Second)

		require.True(t, starts.dispatched.marked())
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

		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady)
		q.offer(agent.Input{Text: "seed"}, "seed", 0) // marks draining, like the pump
		require.True(t, q.offer(agent.Input{Text: "steering"}, "steering", 0))
		starts := newStartState(false)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		var drain drainWaiter
		startDrain(ctx, wrapUI(t, ui), false, ag, q, []agent.Input{{Text: "plan"}}, starts, planHooks{}, &drain)
		drain.wait(5 * time.Second)

		require.Len(t, p.Requests(), 2) // no workflow: steering delivers as before
		assert.Contains(t, texts(p.Requests()[1]), "steering")
	})
}

func TestStartDrainIdle(t *testing.T) {
	t.Parallel()

	t.Run("restores_when_recording", func(t *testing.T) {
		ui := newPumpTestUI(t)
		p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textTurn("done")}}}
		ag, _ := scriptedAgent(p)

		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady)
		starts := newStartState(false)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		var drain drainWaiter
		startDrain(ctx, wrapUI(t, ui), true, ag, q, []agent.Input{{Text: "run"}}, starts, planHooks{}, &drain)

		assert.False(t, ui.Idle()) // the dispatch owns Esc's interrupt role at once

		drain.wait(5 * time.Second)
		assert.True(t, ui.Idle()) // the exit restores the double-Esc rewind gesture
	})

	t.Run("no_restore_when_unrecorded", func(t *testing.T) {
		ui := newPumpTestUI(t)
		p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textTurn("done")}}}
		ag, _ := scriptedAgent(p)

		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady)
		starts := newStartState(false)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		var drain drainWaiter
		startDrain(ctx, wrapUI(t, ui), false, ag, q, []agent.Input{{Text: "run"}}, starts, planHooks{}, &drain)

		drain.wait(5 * time.Second)
		assert.False(t, ui.Idle()) // recording off: Esc never returns to rewind
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
	console := &uiConsole{ui: wrapUI(t, ui), commands: command.NewRegistry()}
	waitReady, takeReady := stagePair(t)
	q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady)

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
	starts := newStartState(false)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var drain drainWaiter
	go runPump(ctx, pump, ag, console, command.NewStager(ctx, nil, nil), nil,
		false, wrapUI(t, ui), starts, func() {}, q, nil, st, nil, &sync.Once{}, func() {}, hooks, &drain)

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

// TestRunPumpParkedSkipsFlush pins the P2-004 fix: a submit held at the plan
// gate must consume no staged output. Held then recalled, and the gated approval
// after it, both leave the `!` result in the stager for a later prompt.
func TestRunPumpParkedSkipsFlush(t *testing.T) {
	t.Parallel()

	ui := newPumpTestUI(t)
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: textTurn("kickoff reply")},
		{Events: textTurn("later reply")},
	}}
	ag, st := scriptedAgent(p)
	console := &uiConsole{ui: wrapUI(t, ui), commands: command.NewRegistry()}

	reg, err := tools.Builtins(tools.Options{Cwd: t.TempDir(), SessionID: "parked-flush"})
	require.NoError(t, err)
	stager := command.NewStager(t.Context(), reg, agent.NopSink{})
	var mu sync.Mutex
	var barEst int
	stager.SetOnChange(func(est int) { mu.Lock(); barEst = est; mu.Unlock() })
	q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, stager.WaitReady, stager.TakeReady)
	expander := refs.NewExpander(nil, agent.NopSink{}, tools.PathPolicy{Cwd: t.TempDir()}, nil)

	var captured bool
	hooks := planHooks{
		beforePrompt: func(_ context.Context, in agent.Input) (agent.Input, bool) {
			if captured || in.Injected {
				return in, false // approval taken, and the gate never captures injected input
			}
			captured = true // the gated submit starts the implementor's turn
			return agent.Input{Text: "kickoff", Injected: true}, true
		},
		parked: func() bool { return !captured },
	}

	pump := make(chan pumpLine, 4)
	starts := newStartState(false)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var drain drainWaiter
	go runPump(ctx, pump, ag, console, stager, expander,
		false, wrapUI(t, ui), starts, func() {}, q, nil, st, nil, &sync.Once{}, func() {}, hooks, &drain)

	stager.Run("echo boom", false)
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return barEst > 0 // the `!` output counts on the bar once staged
	}, 10*time.Second, time.Millisecond)

	// held at the gate: no turn, and the staged output was never consumed. The
	// gate never captures injected input, so it holds instead of approving
	pump <- pumpLine{kind: command.KindPrompt, rest: "note", input: &agent.Input{Text: "note", Injected: true}}
	require.Eventually(t, func() bool { return q.pending() == 1 }, time.Second, time.Millisecond)
	require.True(t, q.recall()) // Alt+Up: the held line leaves the queue
	mu.Lock()
	assert.Positive(t, barEst) // consuming it would have reported zero
	mu.Unlock()

	// the gated approval starts the implementor's turn without the staged output
	pump <- pumpLine{kind: command.KindPrompt, rest: "the plan"}
	require.Eventually(t, func() bool { return len(p.Requests()) == 1 }, 5*time.Second, time.Millisecond)
	assert.NotContains(t, texts(p.Requests()[0]), "User Ran")

	// a later prompt delivers the staged output instead of losing it
	pump <- pumpLine{kind: command.KindPrompt, rest: "and then?"}
	require.Eventually(t, func() bool { return len(p.Requests()) == 2 }, 5*time.Second, time.Millisecond)
	assert.Contains(t, texts(p.Requests()[1]), "User Ran: echo boom")
	assert.Contains(t, texts(p.Requests()[1]), "and then?")
}

func TestStartDrainInterruptLandsFill(t *testing.T) {
	t.Parallel()

	ui := newPumpTestUI(t)

	release := make(chan struct{})
	entered := make(chan struct{})
	set := singleToolSet{tool: holdBash{release: release, entered: entered}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: mainToolTurn()}}}
	st := &agent.State{Model: llm.Model{ID: "test"}, Reasoning: llm.ReasoningConfig{}, Tokens: tokens.New(llm.Model{ID: "test"})}
	ag := agent.New(st, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) { return p, nil },
		Sinks:    []agent.Sink{agent.NopSink{}},
		Tools:    set,
		Env:      agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
	})

	waitReady, takeReady := stagePair(t)
	q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady)
	q.offer(agent.Input{Text: "run"}, "run", 0) // marks draining, like the pump
	starts := newStartState(false)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var drain drainWaiter
	startDrain(ctx, wrapUI(t, ui), false, ag, q, []agent.Input{{Text: "run"}}, starts, planHooks{}, &drain)

	<-entered // the turn is pinned inside its tool call

	ag.Interrupt()
	drain.wait(5 * time.Second)

	require.True(t, starts.dispatched.marked())
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
