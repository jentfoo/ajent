package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/ctl"
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
		<-woke
	})
}

// TestRunPump pins the pump's line loop: command and prompt dispatch, quit
// semantics, deferred-state flushes on wake, parked holds that skip flushes,
// stale-prompt recovery and interrupt-during-hold drops.
func TestRunPump(t *testing.T) {
	t.Run("dispatch_and_exit", func(t *testing.T) {
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
					newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, stager.WaitReady, stager.TakeReady, nil),
					nil, nil, &agent.State{Tokens: tokens.New(llm.Model{})}, nil, &sync.Once{}, func() {}, planHooks{}, &drain)
				close(done)
			}()

			pump <- pumpLine{kind: command.KindCommand, rest: "ping hi"}
			got := <-handled
			assert.Equal(t, "hi", got)

			cancel() // the driver's quit path: no close(pump), just the root context
			<-done
			// the channel outlives runPump, so a late sender can never hit a close
			require.NotPanics(t, func() { pump <- pumpLine{kind: command.KindCommand, rest: "ping late"} })
		})
	})

	t.Run("wake_flushes_deferred", func(t *testing.T) {
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
		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady, nil)
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
				q, nil, nil, st, nil, &sync.Once{}, func() {}, planHooks{}, &drain)
			close(done)
		}()

		pump <- pumpLine{wake: true}
		<-applied
		select {
		case <-handled:
			t.Fatal("a wake line must not dispatch a command")
		default:
		}

		cancel()
		<-done
	})

	t.Run("parked_hold", func(t *testing.T) {
		ui := newPumpTestUI(t)
		p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
			{Events: textTurn("kickoff reply")},
			{Events: textTurn("held reply")},
		}}
		ag, st := scriptedAgent(p)
		console := &uiConsole{ui: wrapUI(t, ui), commands: command.NewRegistry()}
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady, nil)

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
			false, wrapUI(t, ui), starts, func() {}, q, nil, nil, st, nil, &sync.Once{}, func() {}, hooks, &drain)

		pump <- pumpLine{kind: command.KindPrompt, rest: "note", input: &agent.Input{Text: "note", Injected: true}}
		require.Eventually(t, func() bool { return q.pending() == 1 }, time.Second, time.Millisecond)
		assert.Empty(t, p.Requests()) // held at the gate, no turn started

		pump <- pumpLine{kind: command.KindPrompt, rest: "the plan", input: &agent.Input{Text: "the plan"}}
		require.Eventually(t, func() bool {
			return len(p.Requests()) == 2 && q.pending() == 0
		}, 5*time.Second, time.Millisecond)
		assert.Contains(t, texts(p.Requests()[0]), "kickoff") // the gated submit ran first
		assert.Contains(t, texts(p.Requests()[1]), "note")    // held input landed behind it
	})

	t.Run("parked_skips_flush", func(t *testing.T) {
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
		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, stager.WaitReady, stager.TakeReady, nil)
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
			false, wrapUI(t, ui), starts, func() {}, q, nil, nil, st, nil, &sync.Once{}, func() {}, hooks, &drain)

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
	})

	t.Run("stale_prompt_recovers", func(t *testing.T) {
		ui := newPumpTestUI(t)
		wrapped := wrapUI(t, ui)
		p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textTurn("ran")}}}
		ag, st := scriptedAgent(p)
		console := &uiConsole{ui: wrapped, commands: command.NewRegistry()}
		lane := newStateLane()
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady, nil)
		expander := refs.NewExpander(nil, agent.NopSink{}, tools.PathPolicy{Cwd: t.TempDir()}, nil)
		log := &emitLog{}
		log.attach(wrapped.hub)

		pump := make(chan pumpLine, 4)
		starts := newStartState(false)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		var drain drainWaiter

		// the line queues before the pump serves it, then the rewind completes and
		// bumps the generation: the line is aged out the moment it is dequeued
		line := pumpLine{kind: command.KindPrompt, rest: "stale question", queueID: "row-1"}
		line.gen = lane.stamp()
		pump <- line
		lane.bump()
		go runPump(ctx, pump, ag, console, command.NewStager(ctx, nil, nil), expander,
			false, wrapped, starts, func() {}, q, lane, nil, st, nil, &sync.Once{}, func() {}, planHooks{}, &drain)

		require.Eventually(t, func() bool {
			return len(log.named(ctl.EventQueueDropped)) > 0
		}, 5*time.Second, 5*time.Millisecond)

		var qd ctl.QueueDroppedData
		require.NoError(t, log.named(ctl.EventQueueDropped)[0].Decode(&qd))
		assert.Equal(t, rewindCauseRewind, qd.Cause)
		assert.Equal(t, []ctl.QueueRow{{ID: "row-1", Text: "stale question"}}, qd.Rows)
		assert.Empty(t, p.Requests()) // never dispatched onto the new branch
		assert.False(t, q.drainLive())

		// a line queued after the rewind carries the fresh generation and runs
		fresh := pumpLine{kind: command.KindPrompt, rest: "fresh question"}
		fresh.gen = lane.stamp()
		pump <- fresh
		require.Eventually(t, func() bool { return len(p.Requests()) == 1 },
			5*time.Second, 5*time.Millisecond)
		assert.Contains(t, texts(p.Requests()[0]), "fresh question")
	})

	t.Run("interrupt_during_hold", func(t *testing.T) {
		ui := newPumpTestUI(t)
		wrapped := wrapUI(t, ui)
		p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textTurn("ran")}}}
		st := &agent.State{Model: llm.Model{ID: "test"}, Reasoning: llm.ReasoningConfig{}, Tokens: tokens.New(llm.Model{ID: "test"})}
		entered := make(chan struct{})
		release := make(chan struct{})
		ag := agent.New(st, agent.Options{
			Provider: func(llm.Model) (llm.Provider, error) { return p, nil },
			Sinks:    []agent.Sink{agent.NopSink{}},
			// the typing gate's boundary hold, pinned like a user mid-message
			AwaitInput: func(context.Context) {
				close(entered)
				<-release
			},
		})
		console := &uiConsole{ui: wrapped, commands: command.NewRegistry()}
		lane := newStateLane()
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady, nil)
		expander := refs.NewExpander(nil, agent.NopSink{}, tools.PathPolicy{Cwd: t.TempDir()}, nil)
		log := &emitLog{}
		log.attach(wrapped.hub)

		pump := make(chan pumpLine, 4)
		starts := newStartState(false)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		var drain drainWaiter
		go runPump(ctx, pump, ag, console, command.NewStager(ctx, nil, nil), expander,
			false, wrapped, starts, func() {}, q, lane, nil, st, nil, &sync.Once{}, func() {}, planHooks{}, &drain)

		line := pumpLine{kind: command.KindPrompt, rest: "held question", queueID: "row-held"}
		line.gen = lane.stamp()
		pump <- line
		<-entered // parked at the hold, its message not yet landed

		ag.Interrupt()
		close(release) // the hold returns into a cancelled turn

		require.Eventually(t, func() bool {
			return len(log.named(ctl.EventQueueDropped)) > 0
		}, 5*time.Second, 5*time.Millisecond)
		var qd ctl.QueueDroppedData
		require.NoError(t, log.named(ctl.EventQueueDropped)[0].Decode(&qd))
		assert.Equal(t, "interrupt", qd.Cause)
		assert.Equal(t, []ctl.QueueRow{{ID: "row-held", Text: "held question"}}, qd.Rows)
		assert.Empty(t, p.Requests()) // the turn never streamed
		assert.Empty(t, st.Messages)  // nothing landed in context either
	})
}

// TestStartDrain pins drain startup: the parked gate, idle exits and the
// interrupt fill that lands before teardown reads state.
func TestStartDrain(t *testing.T) {
	t.Run("parked_gate", func(t *testing.T) {
		t.Run("parked_queue_holds", func(t *testing.T) {
			ui := newPumpTestUI(t)
			p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textTurn("planned")}}}
			ag, _ := scriptedAgent(p)

			waitReady, takeReady := stagePair(t)
			q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady, nil)
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
			q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady, nil)
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
	})

	t.Run("idle_exit", func(t *testing.T) {
		t.Run("restores_when_recording", func(t *testing.T) {
			ui := newPumpTestUI(t)
			p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textTurn("done")}}}
			ag, _ := scriptedAgent(p)

			waitReady, takeReady := stagePair(t)
			q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady, nil)
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
			q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady, nil)
			starts := newStartState(false)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			var drain drainWaiter
			startDrain(ctx, wrapUI(t, ui), false, ag, q, []agent.Input{{Text: "run"}}, starts, planHooks{}, &drain)

			drain.wait(5 * time.Second)
			assert.False(t, ui.Idle()) // recording off: Esc never returns to rewind
		})
	})

	t.Run("interrupt_lands_fill", func(t *testing.T) {
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
		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady, nil)
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
		assert.True(t, answered)
	})
}

func TestPromptInput(t *testing.T) {
	t.Parallel()

	surveyed := []agent.MessageInfo{{Message: llm.Text(llm.RoleUser, "survey pair")}}

	t.Run("assembled_input_skips_expansion", func(t *testing.T) {
		var armed int
		line := pumpLine{
			kind:   command.KindPrompt,
			rest:   "/init",
			input:  &agent.Input{Text: "distill this", Before: surveyed, Injected: true},
			onTurn: func() { armed++ },
		}
		// a nil expander proves this path never reaches @ expansion
		in, echo, pending := promptInput(line, nil, func(string) { t.Error("unexpected notice") })
		assert.Zero(t, pending)

		assert.Equal(t, "distill this", in.Text)
		assert.True(t, in.Injected)
		assert.True(t, in.Prepared)    // assembled by its sender, the seam must not re-expand
		assert.Equal(t, "/init", echo) // a label, not the whole instruction
		assert.Equal(t, 1, armed)      // armed as it becomes a turn
		// the survey's own pairs ride unchanged; staged shell output attaches at
		// delivery now, never at the submit seam
		assert.Equal(t, surveyed, in.Before)
	})

	t.Run("assembled_input_without_hook", func(t *testing.T) {
		line := pumpLine{kind: command.KindPrompt, input: &agent.Input{Text: "x"}}
		in, echo, _ := promptInput(line, nil, func(string) {})
		assert.Equal(t, "x", in.Text)
		assert.True(t, in.Prepared)
		assert.Empty(t, echo)
	})

	t.Run("typed_prompt_expands", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644))
		reg, err := tools.Builtins(tools.Options{Cwd: dir})
		require.NoError(t, err)
		expander := refs.NewExpander(reg, agent.NopSink{}, tools.PathPolicy{Cwd: dir}, nil)

		line := pumpLine{kind: command.KindPrompt, rest: "look at @a.go"}
		in, echo, pending := promptInput(line, expander, func(string) {})

		assert.Contains(t, in.Text, "@a.go")
		assert.Equal(t, in.Text, echo) // a typed prompt echoes itself
		assert.False(t, in.Injected)
		assert.True(t, in.Prepared) // expanded here, the seam must not re-expand
		assert.Positive(t, pending) // the read is sized before it lands
		assert.Empty(t, in.Before)  // staged shell output attaches at delivery, not submit
		require.NotNil(t, in.After)
		assert.Equal(t, []string{"ref-1-r-a.go"}, callIDs(in.After(t.Context())))
	})

	t.Run("expansion_notices_surface", func(t *testing.T) {
		dir := t.TempDir()
		reg, err := tools.Builtins(tools.Options{Cwd: dir})
		require.NoError(t, err)
		expander := refs.NewExpander(reg, agent.NopSink{}, tools.PathPolicy{Cwd: dir}, nil)

		var warned []string
		line := pumpLine{kind: command.KindPrompt, rest: "see @missing.go"}
		_, _, _ = promptInput(line, expander, func(n string) { warned = append(warned, n) })
		require.Len(t, warned, 1)
		assert.Contains(t, warned[0], "missing.go")
	})

	// only dispatched prompts reach this path: escaped text that looks like a
	// command or shell line was already classified, so it echoes verbatim
	dir := t.TempDir()
	reg, err := tools.Builtins(tools.Options{Cwd: dir})
	require.NoError(t, err)
	echoExpander := refs.NewExpander(reg, agent.NopSink{}, tools.PathPolicy{Cwd: dir}, nil)
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"prompt_echoes", "refactor the parser", "refactor the parser"},
		{"blank_prompt_silent", "   ", ""},
		{"escaped_shell_echos", "!git status", "!git status"},
		{"multiline_echos_verbatim", "line one\nline two", "line one\nline two"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := pumpLine{kind: command.KindPrompt, rest: tc.in}
			_, echo, _ := promptInput(line, echoExpander, func(string) {})
			assert.Equal(t, tc.want, echo)
		})
	}
}

func TestSubmitEstimate(t *testing.T) {
	t.Parallel()

	text := agent.Input{Text: "a short prompt"}

	t.Run("text_only", func(t *testing.T) {
		assert.Positive(t, submitEstimate(text, 0))
	})

	// injected pairs are the larger half of a survey, the bucket must count them
	t.Run("before_weighted", func(t *testing.T) {
		withBefore := text
		withBefore.Before = []agent.MessageInfo{{Message: llm.Text(llm.RoleUser, strings.Repeat("payload ", 200))}}
		assert.Greater(t, submitEstimate(withBefore, 0), submitEstimate(text, 0))
	})

	// @ reads have not run yet, so their measured size is carried in separately
	t.Run("pending_carried", func(t *testing.T) {
		assert.Equal(t, submitEstimate(text, 0)+500, submitEstimate(text, 500))
	})
}
