package app

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/client"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/refs"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

// attachControl claims the control seat on one attached client.
func attachControl(t *testing.T, c *ctlClient, holder string) ctl.AttachResult {
	t.Helper()
	rp := c.call("att", ctl.MethodAttach, map[string]any{"control": true, "holder": holder})
	require.Nil(t, rp.Error)
	var res ctl.AttachResult
	require.NoError(t, rp.DecodeResult(&res))
	return res
}

// TestControlAttach pins the seat contract: one controller claims, a second
// claim refuses naming the holder, release-on-EOF frees the seat at once, and
// re-claim is idempotent while force moves it.
func TestControlAttach(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME

	t.Run("single_holder", func(t *testing.T) {
		rig := newControlRig(t, nil)
		one := dialControl(t, rig.path)
		one.hello()
		two := dialControl(t, rig.path)
		two.hello()

		assert.Equal(t, ctl.AttachResult{Control: true}, attachControl(t, one, "ci-runner"))

		// a plain attach stays an observer
		rp := two.call("obs", ctl.MethodAttach, map[string]any{})
		require.Nil(t, rp.Error)
		var obs ctl.AttachResult
		require.NoError(t, rp.DecodeResult(&obs))
		assert.False(t, obs.Control)

		// a second claim refuses naming the holder
		rp = two.call("dup", ctl.MethodAttach, map[string]any{"control": true})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeSeatHeld, rp.Error.Code)
		assert.Contains(t, rp.Error.Message, "ci-runner")

		// every write method refuses the observer, each naming the holder
		for _, m := range []string{ctl.MethodSubmit, ctl.MethodInterrupt, ctl.MethodModeCycle,
			ctl.MethodInputSet, ctl.MethodQueueRecall, ctl.MethodRewind, ctl.MethodResolve} {
			rp = two.call("obs-"+m, m, map[string]any{})
			require.NotNil(t, rp.Error)
			assert.Equal(t, ctl.CodeObserver, rp.Error.Code)
			assert.Contains(t, rp.Error.Message, "ci-runner")
		}

		// read methods stay open to the observer
		rp = two.call("st", ctl.MethodState, map[string]any{})
		require.Nil(t, rp.Error)
	})

	t.Run("released_on_disconnect", func(t *testing.T) {
		rig := newControlRig(t, nil)
		one := dialControl(t, rig.path)
		one.hello()
		two := dialControl(t, rig.path)
		two.hello()
		attachControl(t, one, "first")

		require.NoError(t, one.conn.Close())
		rig.awaitSeatFree(t)

		// a waiting controller attaches immediately, no cooperating call
		attachControl(t, two, "second")
	})

	t.Run("retake_force", func(t *testing.T) {
		rig := newControlRig(t, nil)
		one := dialControl(t, rig.path)
		one.hello()
		two := dialControl(t, rig.path)
		two.hello()
		attachControl(t, one, "first")

		// the holder re-attaching succeeds and refreshes its name
		rp := one.call("again", ctl.MethodAttach, map[string]any{"control": true, "holder": "renamed"})
		require.Nil(t, rp.Error)
		var res ctl.AttachResult
		require.NoError(t, rp.DecodeResult(&res))
		assert.True(t, res.Control)

		// a second claim still refuses naming the refreshed holder
		rp = two.call("dup", ctl.MethodAttach, map[string]any{"control": true})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeSeatHeld, rp.Error.Code)
		assert.Contains(t, rp.Error.Message, "renamed")

		// force takes the seat from the live holder
		rp = two.call("force", ctl.MethodAttach,
			map[string]any{"control": true, "holder": "second", "force": true})
		require.Nil(t, rp.Error)
		require.NoError(t, rp.DecodeResult(&res))
		assert.True(t, res.Control)

		// the takeover announces to every connection: the displaced holder downgrades
		// without probing, and observers learn who drives now
		var lostOne, lostTwo ctl.SeatLostData
		require.NoError(t, one.collect(ctl.EventSeatLost, 1)[0].Decode(&lostOne))
		require.NoError(t, two.collect(ctl.EventSeatLost, 1)[0].Decode(&lostTwo))
		assert.Equal(t, "renamed", lostOne.From)
		assert.Equal(t, "second", lostOne.To)

		// the displaced holder writes no more: refusals name the new holder
		rp = one.call("w", ctl.MethodInterrupt, map[string]any{})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeObserver, rp.Error.Code)
		assert.Contains(t, rp.Error.Message, "second")

		// snapshots report the seat from each connection's side of it
		rp = one.call("seat1", ctl.MethodState, map[string]any{})
		require.Nil(t, rp.Error)
		var stOne ctl.State
		require.NoError(t, rp.DecodeResult(&stOne))
		assert.True(t, stOne.Seat.Held)
		assert.Equal(t, "second", stOne.Seat.Holder)
		assert.False(t, stOne.Seat.Yours)

		rp = two.call("seat2", ctl.MethodState, map[string]any{})
		require.Nil(t, rp.Error)
		var stTwo ctl.State
		require.NoError(t, rp.DecodeResult(&stTwo))
		assert.True(t, stTwo.Seat.Held)
		assert.Equal(t, "second", stTwo.Seat.Holder)
		assert.True(t, stTwo.Seat.Yours)
	})
}

// TestControlMethods pins the registry contract beside the table: every
// mutating method declares seat-required, read methods never do, and an
// unknown method errors by code rather than a silent no-op.
func TestControlMethods(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME

	t.Run("write_methods_require_seat", func(t *testing.T) {
		for _, m := range []string{ctl.MethodSubmit, ctl.MethodInterrupt, ctl.MethodModeCycle,
			ctl.MethodInputSet, ctl.MethodQueueRecall, ctl.MethodRewind, ctl.MethodResolve} {
			entry, ok := controlMethods[m]
			require.True(t, ok)
			assert.True(t, entry.seat)
		}
	})

	t.Run("read_methods_stay_open", func(t *testing.T) {
		for _, m := range []string{ctl.MethodState, ctl.MethodAttach} {
			entry, ok := controlMethods[m]
			require.True(t, ok)
			assert.False(t, entry.seat)
		}
	})

	t.Run("mutating_verbs_run_the_lane", func(t *testing.T) {
		for _, m := range []string{ctl.MethodSubmit, ctl.MethodInterrupt,
			ctl.MethodModeCycle, ctl.MethodInputSet, ctl.MethodQueueRecall, ctl.MethodRewind} {
			entry, ok := controlMethods[m]
			require.True(t, ok)
			assert.True(t, entry.lane)
		}
		// interrupt jumps the queue so Esc latency never waits behind a rewind
		assert.True(t, controlMethods[ctl.MethodInterrupt].priority)
	})

	t.Run("unknown_method_refused", func(t *testing.T) {
		rig := newControlRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()

		rp := c.call("x1", "prompt.send", map[string]any{})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeUnknownMethod, rp.Error.Code)
		assert.Contains(t, rp.Error.Message, "prompt.send")
	})
}

// spyCycler records barrier cycles, the hub's permission-gate seam in tests.
type spyCycler struct {
	mu    sync.Mutex
	mode  permit.Mode
	calls []string
}

func (s *spyCycler) Mode() permit.Mode { return s.mode }

func (s *spyCycler) Cycle() permit.Mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "cycle")
	s.mode = s.mode.Next()
	return s.mode
}

func (s *spyCycler) Prev() permit.Mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "prev")
	s.mode = s.mode.Prev()
	return s.mode
}

// recorded snapshots the call sequence.
func (s *spyCycler) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// TestControlModeCycle pins mode.cycle: the reply names the new mode, the
// hub's barrier stepped in the asked direction, and the permission notice
// reached observers.
func TestControlModeCycle(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newControlRig(t, nil)
	c := dialControl(t, rig.path)
	c.hello()
	attachControl(t, c, "m")
	spy := &spyCycler{mode: permit.ModeAllowRead} // the interactive default
	rig.hub.barrier = spy

	rp := c.call("m1", ctl.MethodModeCycle, map[string]any{})
	require.Nil(t, rp.Error)
	var res ctl.ModeResult
	require.NoError(t, rp.DecodeResult(&res))
	assert.Equal(t, permit.ModeAuto.String(), res.Mode)

	rp = c.call("m2", ctl.MethodModeCycle, map[string]any{"back": true})
	require.Nil(t, rp.Error)
	require.NoError(t, rp.DecodeResult(&res))
	assert.Equal(t, permit.ModeAllowRead.String(), res.Mode)

	assert.Equal(t, []string{"cycle", "prev"}, spy.recorded())
}

// TestControlSubmit pins the submit rules: an empty line errors rather than
// dispatching, the startup window before the router exists is named, and over
// the real pump a submitted prompt drives a turn carrying its controller
// origin onto the transcript while further submits queue truthfully.
func TestControlSubmit(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME

	t.Run("refusals", func(t *testing.T) {
		rig := newControlRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()
		attachControl(t, c, "s")

		for _, line := range []string{"", "   \t "} {
			rp := c.call("e", ctl.MethodSubmit, map[string]any{"line": line})
			require.NotNil(t, rp.Error)
			assert.Equal(t, ctl.CodeBadRequest, rp.Error.Code)
		}

		// the hub has no submit seam yet: the run is still starting
		rp := c.call("u", ctl.MethodSubmit, map[string]any{"line": "hi"})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeUnavailable, rp.Error.Code)

		// a full prompt queue refuses busy instead of parking the control lane
		rig.hub.setSubmitLine(func(string, string, string) error { return errPromptQueueFull })
		rp = c.call("b", ctl.MethodSubmit, map[string]any{"line": "hi"})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeBusy, rp.Error.Code)
		assert.Contains(t, rp.Error.Message, "retry")
	})

	t.Run("steers_and_interrupts", func(t *testing.T) {
		rig := newServeRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()
		attachControl(t, c, "driver")

		entered := make(chan struct{})
		release := make(chan struct{})
		prov := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: mainToolTurn()}}}
		wireControlPump(t, rig, prov, holdBash{release: release, entered: entered})

		// the first submit drives a turn pinned inside its tool call
		rp := c.call("s1", ctl.MethodSubmit, map[string]any{"line": "first"})
		require.Nil(t, rp.Error)
		select {
		case <-entered:
		case <-time.After(ctlWait):
			t.Fatal("the submitted prompt never started its turn")
		}

		// a second submit queues: the queued event echoes its correlation, never
		// a silent drop
		rp = c.call("s2", ctl.MethodSubmit, map[string]any{"line": "second", "pendingId": "ctl-row-1"})
		require.Nil(t, rp.Error)
		qd := c.collect(ctl.EventQueued, 1)[0]
		var q ctl.QueuedData
		require.NoError(t, qd.Decode(&q))
		assert.Equal(t, []ctl.QueueRow{{ID: "ctl-row-1", Text: "second"}}, q.Rows)

		// interrupt names the turn and drops the queued row by id: no editor
		// exists to recover it into. The drop publishes first, the emptied rows
		// beside it in the same seq window.
		rp = c.call("i1", ctl.MethodInterrupt, map[string]any{})
		require.Nil(t, rp.Error)
		var res ctl.InterruptResult
		require.NoError(t, rp.DecodeResult(&res))
		assert.Equal(t, "turn", res.Stopped)

		var droppedEv, cleared ctl.Event
		require.Eventually(t, func() bool {
			for _, ev := range c.takePending() {
				switch {
				case ev.Name == ctl.EventQueueDropped && droppedEv.Name == "":
					droppedEv = ev
				case ev.Name == ctl.EventQueued && droppedEv.Name != "" && cleared.Name == "":
					cleared = ev // the rows update beside the drop
				}
			}
			return droppedEv.Name != "" && cleared.Name != ""
		}, ctlWait, 5*time.Millisecond)

		var dd ctl.QueueDroppedData
		require.NoError(t, droppedEv.Decode(&dd))
		assert.Equal(t, []ctl.QueueRow{{ID: "ctl-row-1", Text: "second"}}, dd.Rows)
		assert.Equal(t, "interrupt", dd.Cause)

		require.NoError(t, cleared.Decode(&q))
		assert.Empty(t, q.Rows)
		c.collect(ctl.EventTurnEnd, 1) // the aborted turn closed

		// the submitted prompt reached the transcript carrying its controller
		// origin, byte-identical to the typed text: no marker rides the message
		var origins []string
		for _, e := range rigBranch(t, rig.rec) {
			if e.Type != session.TypeMessage {
				continue
			}
			var md session.MessageData
			require.NoError(t, json.Unmarshal(e.Data, &md))
			if md.Message.Role != llm.RoleUser || llm.OnlyToolResults(md.Message.Content) {
				continue // tool results ride user rows without being prompts
			}
			text, ok := md.Message.Content[0].(llm.TextBlock)
			require.True(t, ok)
			assert.Equal(t, "first", text.Text)
			origins = append(origins, md.Origin)
			assert.False(t, md.Injected) // groups and echoes like the user line it emulates
			// a direct dispatch never queued, so the pump minted the correlation
			// the committed entry carries for pending-row resolution
			require.Len(t, md.QueueIDs, 1)
		}
		assert.Equal(t, []string{submitOriginController}, origins)
	})
}

// wireControlPump wires the driver's submit path over a serve-shaped rig:
// router, steer queue, stager and the pump goroutine, so submitted lines
// classify, prompts steer or queue, and rows report through the hub.
func wireControlPump(t *testing.T, rig *controlRig, prov llm.Provider, turn agent.Tool) {
	t.Helper()

	toolReg, err := tools.Builtins(tools.Options{Cwd: t.TempDir(), SessionID: "ctl-pump"})
	require.NoError(t, err)
	st := &agent.State{Model: llm.Model{ID: "test"}, Tokens: tokens.New(llm.Model{ID: "test"})}
	ag := agent.New(st, agent.Options{
		Provider:  func(llm.Model) (llm.Provider, error) { return prov, nil },
		Sinks:     []agent.Sink{rig.rec.rec.Sink(rig.hub.sink())},
		OnMessage: []func(agent.MessageInfo){rig.rec.rec.Message},
		Tools:     singleToolSet{tool: turn},
		Env:       agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
	})
	rig.hub.ag = ag
	rig.hub.syncLedger()

	stager := command.NewStager(t.Context(), toolReg, agent.NopSink{})
	t.Cleanup(stager.Cancel)
	q := newSteerQueue(rig.hub.ui, func(int) {}, func() {}, stager.WaitReady, stager.TakeReady,
		rig.hub.queueDroppedCause)
	rig.hub.q = q
	rig.hub.stager = stager
	gate := &typingGate{idle: time.Hour, handoff: 10 * time.Millisecond,
		poll: 5 * time.Millisecond, pending: q.pending}
	pump := make(chan pumpLine, 16)
	router := &lineRouter{ui: rig.hub.ui, stager: stager, gate: gate,
		send: func(pl pumpLine) {
			select {
			case pump <- pl:
			case <-t.Context().Done():
			}
		},
		trySend: func(pl pumpLine) bool {
			select {
			case pump <- pl:
				return true
			default:
				return false
			}
		}}
	rig.hub.submitLine = router.submit

	expander := refs.NewExpander(toolReg, agent.NopSink{}, tools.PathPolicy{Cwd: t.TempDir()}, nil)
	console := &uiConsole{ui: rig.hub.ui, commands: command.NewRegistry()}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var once sync.Once
	var drain drainWaiter
	go runPump(ctx, pump, ag, console, stager, expander, true, rig.hub.ui,
		newStartState(false), func() {}, q, gate, st, nil, &once, func() {}, planHooks{}, &drain)
}

// TestControlInputSet pins input.set: the editor buffer takes the text on an
// interactive run, a dialog owning the input refuses naming its kind while the
// dialog itself still resolves, and a serve run without an editor errors.
func TestControlInputSet(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME

	t.Run("prefill", func(t *testing.T) {
		rig := newControlRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()
		attachControl(t, c, "p")

		rp := c.call("i1", ctl.MethodInputSet, map[string]any{"text": "draft me"})
		require.Nil(t, rp.Error)
		ev := c.collect(ctl.EventEditor, 1)[0]
		var ed ctl.EditorData
		require.NoError(t, ev.Decode(&ed))
		assert.Equal(t, "draft me", ed.Text)
	})

	t.Run("held_by_dialog", func(t *testing.T) {
		blocker := &pickBlocker{held: make(chan struct{}), release: make(chan struct{})}
		rig := newInteractiveRig(t, blocker)
		c := dialControl(t, rig.path)
		c.hello()
		attachControl(t, c, "p")
		t.Cleanup(func() { close(blocker.release) }) // the racer always ends

		picking := make(chan error, 1)
		go func() {
			_, perr := rig.broker.Pick(t.Context(), "pick one", []tui.PickItem{{Label: "a"}, {Label: "b"}}, tui.PickOptions{})
			picking <- perr
		}()
		<-blocker.held

		rp := c.call("i2", ctl.MethodInputSet, map[string]any{"text": "no"})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeUnavailable, rp.Error.Code)
		assert.Contains(t, rp.Error.Message, "pick")

		// the held dialog still resolves: cancel reaches its caller as cancelled
		pend := c.collect(ctl.EventInteractionPending, 1)[0]
		var pd ctl.InteractionPendingData
		require.NoError(t, pend.Decode(&pd))
		rp = c.call("r1", ctl.MethodResolve, map[string]any{"id": pd.ID, "cancel": true})
		require.Nil(t, rp.Error)
		assert.ErrorIs(t, <-picking, tui.ErrCancelled)
	})

	t.Run("serve_refused", func(t *testing.T) {
		rig := newServeRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()
		attachControl(t, c, "s")

		rp := c.call("i", ctl.MethodInputSet, map[string]any{"text": "x"})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeUnavailable, rp.Error.Code)
		assert.Contains(t, rp.Error.Message, "no editor")
	})
}

// TestControlQueueRecall pins queue.recall: the newest queued prompt pops and
// its text returns to the caller, empty when nothing is queued, refusals leave
// the item queued, and the removal announces with rows so attached surfaces
// resolve pending rows. No editor fill happens host-side — serve included.
func TestControlQueueRecall(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME

	t.Run("empty_queue", func(t *testing.T) {
		rig := newServeRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()
		attachControl(t, c, "r")

		rig.hub.q = newSteerQueue(rig.hub.ui, func(int) {}, func() {},
			func(context.Context) {}, func() []agent.MessageInfo { return nil }, rig.hub.queueDroppedCause)

		rp := c.call("q1", ctl.MethodQueueRecall, map[string]any{})
		require.Nil(t, rp.Error) // an idle queue is a no-op result, like interrupt with nothing stopped
		var res ctl.RecallResult
		require.NoError(t, rp.DecodeResult(&res))
		assert.Empty(t, res.Text)
	})

	t.Run("pops_and_announces", func(t *testing.T) {
		rig := newServeRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()
		attachControl(t, c, "r")

		// park two prompts as rows with their correlations, the way a gate hold
		// leaves them
		rig.hub.q = newSteerQueue(rig.hub.ui, func(int) {}, func() {},
			func(context.Context) {}, func() []agent.MessageInfo { return nil }, rig.hub.queueDroppedCause)
		rig.hub.q.hold(agent.Input{Text: "queued one", QueueIDs: []string{"row-1"}}, "queued one", 0)
		rig.hub.q.hold(agent.Input{Text: "queued two", QueueIDs: []string{"row-2"}}, "queued two", 0)

		rp := c.call("q2", ctl.MethodQueueRecall, map[string]any{}) // LIFO: the newest first
		require.Nil(t, rp.Error)
		var res ctl.RecallResult
		require.NoError(t, rp.DecodeResult(&res))
		assert.Equal(t, "queued two", res.Text)

		// the removal announces beside its rows, so attached surfaces resolve the
		// row by id; both ride the lane's publish before this reply
		all := c.collectEventsAny(t, 3)
		var dropped ctl.QueueDroppedData
		require.NoError(t, matchEvent(t, all, ctl.EventQueueDropped, "recalled").Decode(&dropped))
		assert.Equal(t, []ctl.QueueRow{{ID: "row-2", Text: "queued two"}}, dropped.Rows)

		var queued ctl.QueuedData
		require.NoError(t, lastEvent(t, all, ctl.EventQueued).Decode(&queued))
		assert.Equal(t, []ctl.QueueRow{{ID: "row-1", Text: "queued one"}}, queued.Rows)
	})

	t.Run("refusals_stay_queued", func(t *testing.T) {
		rig := newServeRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()
		attachControl(t, c, "r")

		rig.hub.q = newSteerQueue(rig.hub.ui, func(int) {}, func() {},
			func(context.Context) {}, func() []agent.MessageInfo { return nil }, rig.hub.queueDroppedCause)
		rig.hub.q.hold(agent.Input{Text: "secret work"}, "secret: hunter2", 0)

		rp := c.call("q3", ctl.MethodQueueRecall, map[string]any{})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeUnavailable, rp.Error.Code)
		assert.Contains(t, rp.Error.Message, "secret prefix")
		assert.Equal(t, 1, rig.hub.q.pending()) // refused in place

		// the redaction rule holds on the refusal path too: nothing leaked
		for _, ev := range c.takePending() {
			if ev.Name == ctl.EventQueueDropped || ev.Name == ctl.EventQueued {
				assert.NotContains(t, string(ev.Data), "hunter2")
			}
		}
	})
}

// pickBlocker holds one pick dialog open until released or cancelled, the
// interactive local racer for input-held tests.
type pickBlocker struct {
	scriptedLocal
	held    chan struct{}
	release chan struct{}
}

func (p *pickBlocker) PickContext(ctx context.Context, _ string, _ []tui.PickItem, _ tui.PickOptions) (int, error) {
	close(p.held)
	select {
	case <-p.release:
		return 0, nil
	case <-ctx.Done():
		return 0, tui.ErrCancelled
	}
}

// TestControlResolve pins interaction.resolve: every announced kind answers
// with its natural shape, wrong shapes and out-of-range indexes refuse
// legibly, stale ids error after End, the ask_user path holds for a remote
// answer, a local winner beats a late controller, and a controller dying
// mid-dialog leaves it open for whoever reattaches.
func TestControlResolve(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME

	t.Run("answer_shapes", func(t *testing.T) {
		rig := newServeRig(t, nil) // no local racer: dialogs hold for the controller
		c := dialControl(t, rig.path)
		c.hello()
		attachControl(t, c, "ans")

		open := func(t *testing.T, kind string) uint64 {
			t.Helper()
			switch kind {
			case "pick":
				go func() {
					_, _ = rig.broker.Pick(t.Context(), "p", []tui.PickItem{{Label: "a"}, {Label: "b"}}, tui.PickOptions{})
				}()
			case "multipick":
				go func() {
					_, _ = rig.broker.MultiPick(t.Context(), "m", []tui.PickItem{{Label: "x"}, {Label: "y"}}, tui.MultiPickOptions{})
				}()
			case "input":
				go func() { _, _ = rig.broker.Input(t.Context(), "label", "") }()
			case "secret":
				go func() { _, _ = rig.broker.InputSecret(t.Context(), "key", "") }()
			case "ask":
				go func() {
					_, _ = rig.broker.Ask(t.Context(), tui.Question{Text: "proceed?", Options: []tui.Option{{Label: "yes"}, {Label: "no"}}})
				}()
			}
			ev := c.collect(ctl.EventInteractionPending, 1)[0]
			var pd ctl.InteractionPendingData
			require.NoError(t, ev.Decode(&pd))
			return pd.ID
		}
		answer := func(t *testing.T, params map[string]any) ctl.Reply {
			t.Helper()
			return c.call("ans", ctl.MethodResolve, params)
		}
		settled := func(t *testing.T) ctl.InteractionSettledData {
			t.Helper()
			ev := c.collect(ctl.EventInteractionSettled, 1)[0]
			var sd ctl.InteractionSettledData
			require.NoError(t, ev.Decode(&sd))
			return sd
		}

		t.Run("index_answers_pick", func(t *testing.T) {
			id := open(t, "pick")
			rp := answer(t, map[string]any{"id": id, "index": 1})
			require.Nil(t, rp.Error)
			sd := settled(t)
			assert.Equal(t, id, sd.ID)
			assert.True(t, sd.Answered)
			assert.Equal(t, 1, sd.Index)
		})

		t.Run("indexes_answer_multipick", func(t *testing.T) {
			id := open(t, "multipick")
			rp := answer(t, map[string]any{"id": id, "indexes": []int{0, 1}})
			require.Nil(t, rp.Error)
			sd := settled(t)
			assert.Equal(t, []int{0, 1}, sd.Indexes)
		})

		t.Run("empty_indexes_confirm_multipick", func(t *testing.T) {
			id := open(t, "multipick")
			rp := answer(t, map[string]any{"id": id, "indexes": []int{}})
			require.Nil(t, rp.Error) // an empty list is a real answer: confirm none
			sd := settled(t)
			assert.True(t, sd.Answered)
			assert.Empty(t, sd.Indexes)
		})

		t.Run("ambiguous_shapes_refused", func(t *testing.T) {
			id := open(t, "input")
			rp := answer(t, map[string]any{"id": id, "text": "x", "cancel": true})
			require.NotNil(t, rp.Error)
			assert.Equal(t, ctl.CodeBadRequest, rp.Error.Code)

			rp = answer(t, map[string]any{"id": id, "cancel": true}) // clean up
			require.Nil(t, rp.Error)
		})

		t.Run("text_answers_input", func(t *testing.T) {
			id := open(t, "input")
			rp := answer(t, map[string]any{"id": id, "text": "typed"})
			require.Nil(t, rp.Error)
			sd := settled(t)
			assert.Equal(t, "typed", sd.Text)
		})

		t.Run("secret_text_never_settles_visible", func(t *testing.T) {
			id := open(t, "secret")
			rp := answer(t, map[string]any{"id": id, "text": "sk-live"})
			require.Nil(t, rp.Error)
			sd := settled(t)
			assert.Empty(t, sd.Text)
		})

		t.Run("cancel_answers_everywhere", func(t *testing.T) {
			id := open(t, "pick")
			rp := answer(t, map[string]any{"id": id, "cancel": true})
			require.Nil(t, rp.Error)
			sd := settled(t)
			assert.True(t, sd.Cancelled)
			assert.False(t, sd.Answered)
		})

		t.Run("chat_answers_ask", func(t *testing.T) {
			id := open(t, "ask")
			rp := answer(t, map[string]any{"id": id, "text": "in my own words", "chat": true})
			require.Nil(t, rp.Error)
			sd := settled(t)
			assert.True(t, sd.Answered)
			assert.Equal(t, "in my own words", sd.Text)
			assert.True(t, sd.Chat)
		})

		t.Run("optionless_ask_refuses_index", func(t *testing.T) {
			go func() {
				_, _ = rig.broker.Ask(t.Context(), tui.Question{Text: "why?"})
			}()
			ev := c.collect(ctl.EventInteractionPending, 1)[0]
			var pd ctl.InteractionPendingData
			require.NoError(t, ev.Decode(&pd)) // no options announced, so no index answer exists
			assert.Empty(t, pd.Options)

			rp := answer(t, map[string]any{"id": pd.ID, "index": 0})
			require.NotNil(t, rp.Error)
			assert.Equal(t, ctl.CodeBadRequest, rp.Error.Code)

			rp = answer(t, map[string]any{"id": pd.ID, "text": "because"})
			require.Nil(t, rp.Error)
			settled(t)
		})

		t.Run("empty_text_answers_optionless_ask", func(t *testing.T) {
			// an empty submit is a real (empty) answer, like the host terminal: the
			// deny-reason prompt closes instead of wedging both sessions
			go func() {
				r, _ := rig.broker.Ask(t.Context(), tui.Question{Text: "reason for denying"})
				assert.False(t, r.Declined)
			}()
			ev := c.collect(ctl.EventInteractionPending, 1)[0]
			var pd ctl.InteractionPendingData
			require.NoError(t, ev.Decode(&pd))

			rp := answer(t, map[string]any{"id": pd.ID, "text": ""})
			require.Nil(t, rp.Error)
			sd := settled(t)
			assert.True(t, sd.Answered)
			assert.Empty(t, sd.Text)
		})

		t.Run("wrong_shape_refused", func(t *testing.T) {
			id := open(t, "input")
			rp := answer(t, map[string]any{"id": id, "index": 0})
			require.NotNil(t, rp.Error)
			assert.Equal(t, ctl.CodeBadRequest, rp.Error.Code)
			assert.Contains(t, rp.Error.Message, "input")

			rp = answer(t, map[string]any{"id": id, "cancel": true}) // clean up
			require.Nil(t, rp.Error)
		})

		t.Run("out_of_range_refused", func(t *testing.T) {
			id := open(t, "pick")
			rp := answer(t, map[string]any{"id": id, "index": 2})
			require.NotNil(t, rp.Error)
			assert.Equal(t, ctl.CodeBadRequest, rp.Error.Code)
			assert.Contains(t, rp.Error.Message, "out of range")

			rp = answer(t, map[string]any{"id": id, "index": 0})
			require.Nil(t, rp.Error)
		})

		t.Run("no_shape_refused", func(t *testing.T) {
			id := open(t, "input")
			rp := answer(t, map[string]any{"id": id})
			require.NotNil(t, rp.Error)
			assert.Equal(t, ctl.CodeBadRequest, rp.Error.Code)

			rp = answer(t, map[string]any{"id": id, "cancel": true})
			require.Nil(t, rp.Error)
		})

		t.Run("unknown_id_not_found", func(t *testing.T) {
			rp := answer(t, map[string]any{"id": 999, "cancel": true})
			require.NotNil(t, rp.Error)
			assert.Equal(t, ctl.CodeNotFound, rp.Error.Code)
		})

		t.Run("stale_id_settled_after_end", func(t *testing.T) {
			id := open(t, "input")
			rp := answer(t, map[string]any{"id": id, "text": "once"})
			require.Nil(t, rp.Error)
			rp = answer(t, map[string]any{"id": id, "text": "twice"})
			require.NotNil(t, rp.Error)
			assert.Equal(t, ctl.CodeSettled, rp.Error.Code)
		})
	})

	t.Run("ask_holds_for_remote", func(t *testing.T) {
		rig := newServeRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()
		attachControl(t, c, "ask")

		type askReply struct {
			idx      int
			text     string
			declined bool
		}
		answered := make(chan askReply, 1)
		go func() {
			idx, text, declined, aerr := askUser(rig.broker)(t.Context(), "proceed?", []string{"yes", "no"})
			assert.NoError(t, aerr)
			answered <- askReply{idx: idx, text: text, declined: declined}
		}()

		// no local surface exists, so the question holds until answered remotely
		pend := c.collect(ctl.EventInteractionPending, 1)[0]
		var pd ctl.InteractionPendingData
		require.NoError(t, pend.Decode(&pd))
		assert.Equal(t, "ask", pd.Kind)
		assert.Equal(t, []string{"yes", "no"}, pd.Options)

		rp := c.call("a", ctl.MethodResolve, map[string]any{"id": pd.ID, "index": 0})
		require.Nil(t, rp.Error)
		ans := <-answered
		assert.Equal(t, 0, ans.idx) // chose the first option
		assert.Empty(t, ans.text)
		assert.False(t, ans.declined)
		c.collect(ctl.EventInteractionSettled, 1) // observers learn the outcome too
	})

	t.Run("local_answer_wins_race", func(t *testing.T) {
		local := &scriptedLocal{mode: tui.ModeAlt, picks: []int{0}}
		rig := newServeRig(t, local)
		c := dialControl(t, rig.path)
		c.hello()
		attachControl(t, c, "race")

		picked := make(chan int, 1)
		go func() {
			i, perr := rig.broker.Pick(t.Context(), "race", []tui.PickItem{{Label: "a"}, {Label: "b"}}, tui.PickOptions{})
			assert.NoError(t, perr)
			picked <- i
		}()
		pend := c.collect(ctl.EventInteractionPending, 1)[0]
		var pd ctl.InteractionPendingData
		require.NoError(t, pend.Decode(&pd))

		assert.Equal(t, 0, <-picked) // the local answer won

		rp := c.call("late", ctl.MethodResolve, map[string]any{"id": pd.ID, "index": 1})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeSettled, rp.Error.Code)

		ev := c.collect(ctl.EventInteractionSettled, 1)[0]
		var sd ctl.InteractionSettledData
		require.NoError(t, ev.Decode(&sd))
		assert.Equal(t, pd.ID, sd.ID)
		assert.True(t, sd.Answered)
		assert.Equal(t, 0, sd.Index) // the local answer, not the losing remote one
	})

	t.Run("decision_resolves_through_barrier", func(t *testing.T) {
		rig := newServeRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()
		attachControl(t, c, "gate")

		dlg, err := rig.broker.OpenDecision(tui.DecisionRequest{
			Prompt: "allow write?", Options: []tui.Option{{Label: "Allow"}, {Label: "Deny"}},
		})
		require.NoError(t, err)

		waited := make(chan int, 1)
		go func() {
			i, werr := dlg.Wait(t.Context())
			assert.NoError(t, werr)
			waited <- i
		}()

		pend := c.collect(ctl.EventInteractionPending, 1)[0]
		var pd ctl.InteractionPendingData
		require.NoError(t, pend.Decode(&pd))
		assert.Equal(t, "decision", pd.Kind)

		rp := c.call("allow", ctl.MethodResolve, map[string]any{"id": pd.ID, "index": 0})
		require.Nil(t, rp.Error)
		assert.Equal(t, 0, <-waited) // the barrier's Wait saw the remote answer

		// the mode-change path resolving after the remote answer loses cleanly,
		// the same settle-once guarantee reevaluateOpens relies on in the race
		dlg.Resolve(1)
		rp = c.call("again", ctl.MethodResolve, map[string]any{"id": pd.ID, "index": 1})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeSettled, rp.Error.Code)

		ev := c.collect(ctl.EventInteractionSettled, 1)[0]
		var sd ctl.InteractionSettledData
		require.NoError(t, ev.Decode(&sd))
		assert.Equal(t, pd.ID, sd.ID)
		assert.Equal(t, 0, sd.Index) // first resolution won
	})

	t.Run("disconnect_leaves_dialog_open", func(t *testing.T) {
		rig := newServeRig(t, nil)
		one := dialControl(t, rig.path)
		one.hello()
		attachControl(t, one, "gone")

		asking := make(chan string, 1)
		go func() {
			ans, perr := rig.broker.Input(t.Context(), "name", "")
			assert.NoError(t, perr)
			asking <- ans
		}()
		pend := one.collect(ctl.EventInteractionPending, 1)[0]
		var pd ctl.InteractionPendingData
		require.NoError(t, pend.Decode(&pd))

		require.NoError(t, one.conn.Close())
		rig.awaitSeatFree(t)
		select {
		case <-asking:
			t.Fatal("the dialog must hold after its controller died")
		default:
		}

		two := dialControl(t, rig.path)
		two.hello()
		attachControl(t, two, "back")
		rp := two.call("late", ctl.MethodResolve, map[string]any{"id": pd.ID, "text": "answered on reattach"})
		require.Nil(t, rp.Error)
		assert.Equal(t, "answered on reattach", <-asking)
	})
}

// TestControlRewind pins session.rewind: a named head rebuilds the same branch
// the double-Esc picker would, unknown heads error legibly, a running turn
// refuses with nothing moved, and the reply names the startup window before
// the driver wires the rewinder.
func TestControlRewind(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME

	t.Run("forks_branch", func(t *testing.T) {
		rig := newServeRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()
		attachControl(t, c, "rw")
		cw := wireControlRewind(t, rig)

		// two recorded turns to rewind between
		rec := rig.rec.rec
		rec.Message(agent.MessageInfo{Message: llm.Text(llm.RoleUser, "first")})
		rec.Message(agent.MessageInfo{Message: llm.Text(llm.RoleAssistant, "answer one")})
		rec.Message(agent.MessageInfo{Message: llm.Text(llm.RoleUser, "second")})

		entries, _, err := session.Read(rig.rec.w.Path())
		require.NoError(t, err)
		branch := session.Branch(entries, rig.rec.w.Head())
		firstAnswer := branch[2].ID // the assistant reply keeps its own row: the branch survives

		// an unknown head is not-found
		rp := c.call("rw1", ctl.MethodRewind, map[string]any{"head": "01NOPE"})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeNotFound, rp.Error.Code)
		assert.Contains(t, rp.Error.Message, "01NOPE")

		// the rewind lands: the reply names the new head, the head event fires,
		// the plan stopper ran, and the transcript branch rebuilt without the
		// second prompt
		rp = c.call("rw2", ctl.MethodRewind, map[string]any{"head": firstAnswer})
		require.Nil(t, rp.Error)
		var res ctl.RewindResult
		require.NoError(t, rp.DecodeResult(&res))
		assert.NotEmpty(t, res.Head)
		assert.True(t, *cw.stopped)

		ev := c.collect(ctl.EventBranchChanged, 1)[0]
		var hd ctl.BranchChangedData
		require.NoError(t, ev.Decode(&hd))
		assert.Equal(t, res.Head, hd.Head)
		assert.Equal(t, rewindCauseRewind, hd.Cause)

		assert.Equal(t, res.Head, rig.rec.w.Head()) // the branch rebuilt without the second prompt
		var texts []string
		for _, e := range rigBranch(t, rig.rec) {
			if e.Type != session.TypeMessage {
				continue
			}
			var md session.MessageData
			require.NoError(t, json.Unmarshal(e.Data, &md))
			if b, ok := md.Message.Content[0].(llm.TextBlock); ok {
				texts = append(texts, b.Text)
			}
		}
		assert.Equal(t, []string{"first", "answer one"}, texts) // "second" forked away

		// a running turn refuses with nothing moved. The held turn ends on the
		// context cancel even when an assertion fails first, so closing release
		// here never double-closes.
		turnDone := make(chan error, 1)
		go func() {
			turnDone <- rig.hub.ag.Prompt(t.Context(), agent.Input{Text: "hold"})
		}()
		<-cw.entered
		headBefore := rig.rec.w.Head()
		rp = c.call("rw3", ctl.MethodRewind, map[string]any{"head": firstAnswer})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeUnavailable, rp.Error.Code)
		assert.Contains(t, rp.Error.Message, "turn is running")
		assert.Equal(t, headBefore, rig.rec.w.Head()) // nothing moved
		close(cw.release)
		require.NoError(t, <-turnDone) // release unblocks it; the turn ends normally
	})

	t.Run("unavailable_while_starting", func(t *testing.T) {
		rig := newServeRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()
		attachControl(t, c, "early")

		rp := c.call("rw", ctl.MethodRewind, map[string]any{"head": "01ANY"})
		require.NotNil(t, rp.Error)
		assert.Equal(t, ctl.CodeUnavailable, rp.Error.Code)
	})
}

// wireControlRewind arms the rewind seam over one rig with a live agent and
// registry, a stopper standing in for the driver's plan-workflow ending, and
// a turn that pins inside its tool call for the mid-turn refusal.
type controlRewind struct {
	stopped *bool
	entered chan struct{}
	release chan struct{}
}

func wireControlRewind(t *testing.T, rig *controlRig) *controlRewind {
	t.Helper()

	reg, warnings := llm.NewRegistry(llm.File{
		Providers: map[string]llm.ProviderConfig{"p": {Models: []llm.ModelConfig{{ID: "a"}}}},
	}, nil, llm.RegistryOptions{})
	require.Empty(t, warnings)
	st := &agent.State{Model: reg.Active(), Tokens: tokens.New(reg.Active())}
	entered := make(chan struct{})
	release := make(chan struct{})
	prov := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: mainToolTurn()}, // the held call: pinned inside its tool
		{Events: textTurn("done")},
	}}
	ag := agent.New(st, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) { return prov, nil },
		Sinks:    []agent.Sink{rig.rec.rec.Sink(rig.hub.sink())},
		Tools:    singleToolSet{tool: holdBash{release: release, entered: entered}},
		Env:      agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
	})
	rig.hub.ag = ag
	rig.hub.syncLedger()
	var stopped bool
	rig.hub.rewindTo = func(head, cause string) (string, error) {
		stopped = true // the driver ends a plan workflow before switching
		return rig.rec.rewindTo(rig.hub.ui, ag, reg, head, cause)
	}
	return &controlRewind{stopped: &stopped, entered: entered, release: release}
}

// writeMessage records one message entry on the transcript.
func writeMessage(t *testing.T, rec *sessRec, text string) {
	t.Helper()
	rec.rec.Message(agent.MessageInfo{
		Message: llm.Message{Role: llm.RoleUser, Content: llm.BlockList{llm.TextBlock{Text: text}}},
	})
}

// TestControlState pins state.get truth: every field agrees with the hub's
// own snapshot on the same tick and tool toggles surface later, while the
// backfill sources stay honest — the head a transcript read branches by comes
// from hello's named path, and only post-attach appends announce.
func TestControlState(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME

	t.Run("snapshot_truth", func(t *testing.T) {
		rig := newControlRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()

		ui := rig.hub.ui
		ui.SetStatusSegment(segment(segMCP, "mcp 2/2", "2/2"))
		ui.SetActivity("scan", "reading pkg")
		ui.SetQueued([]ctl.QueueRow{{ID: "row-9", Text: "next prompt"}})

		reg, err := tools.Builtins(tools.Options{Cwd: t.TempDir(), SessionID: "ctl-test"})
		require.NoError(t, err)
		rig.hub.tools = reg

		var st ctl.State
		rp := c.call("a1", ctl.MethodState, map[string]any{})
		require.Nil(t, rp.Error)
		require.NoError(t, rp.DecodeResult(&st))

		snap := rig.hub.Snapshot()
		assert.Equal(t, snap.SessionID, st.Session.ID)
		assert.Equal(t, config.Cwd(), st.Workspace)
		assert.True(t, st.Idle)
		assert.False(t, st.Running)
		assert.Equal(t, []ctl.QueueRow{{ID: "row-9", Text: "next prompt"}}, st.QueuedRows)
		require.Len(t, st.Activity, 1)
		assert.Equal(t, ctl.ActivityRow{Key: "scan", Text: "reading pkg", Rank: activityUnranked}, st.Activity[0])
		require.Len(t, st.Status.Segments, len(snap.Status.Segments))
		assert.True(t, toolRow(t, st.Tools, tools.ToolRead).Enabled)
		assert.True(t, toolRow(t, st.Tools, tools.ToolRead).ReadOnly)

		// a /tools toggle shows up in the next snapshot: only the named set stays on
		reg.SetEnabled([]string{tools.ToolRead})
		rp = c.call("a2", ctl.MethodState, map[string]any{})
		require.NoError(t, rp.DecodeResult(&st))
		assert.True(t, toolRow(t, st.Tools, tools.ToolRead).Enabled)
		assert.False(t, toolRow(t, st.Tools, tools.ToolBash).Enabled)
	})

	t.Run("backfill_sources", func(t *testing.T) {
		rig := newControlRig(t, func(rec *sessRec) {
			writeMessage(t, rec, "recorded before attach")
		})
		c := dialControl(t, rig.path)
		c.hello()

		rp := c.call("s1", ctl.MethodState, map[string]any{})
		require.Nil(t, rp.Error)
		var st ctl.State
		require.NoError(t, rp.DecodeResult(&st))
		assert.Equal(t, rig.rec.w.Head(), st.Head)

		branch, err := client.ReadBranch(rig.rec.w.Path(), st.Head)
		require.NoError(t, err)
		require.Len(t, branch, 2) // the session entry and its recorded prompt
		last := branch[len(branch)-1]
		var md session.MessageData
		require.NoError(t, json.Unmarshal(last.Data, &md))
		assert.Equal(t, "recorded before attach", llm.JoinText(md.Message.Content))

		writeMessage(t, rig.rec, "written after attach")

		// exactly one append announced: the pre-attach entry never did
		appended := c.collect(ctl.EventEntryAppend, 1)
		var ed ctl.EntryData
		require.NoError(t, appended[0].Decode(&ed))
		assert.Equal(t, branch[len(branch)-1].ID, ed.Entry.ParentID) // appends extend the head
	})
}

func toolRow(t *testing.T, rows []ctl.ToolState, name string) ctl.ToolState {
	t.Helper()
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("tool %s missing from snapshot", name)
	return ctl.ToolState{}
}

// TestControlEvents pins the event stream contract: events at or below a
// reply's seq are superseded by the result while later ones apply exactly
// once, replay serves retained history and refuses trimmed history, and many
// observers each see the full sequence in order.
func TestControlEvents(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME

	t.Run("watermark_applies_once", func(t *testing.T) {
		rig := newControlRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()

		rig.hub.publish("marker", map[string]string{"v": "one"})
		rig.hub.publish("marker", map[string]string{"v": "two"})

		rp := c.call("w1", ctl.MethodState, map[string]any{})
		require.Nil(t, rp.Error)

		// everything the call waited through sits at or below the watermark
		waited := c.takePending()
		superseded := make([]uint64, 0, len(waited))
		for _, ev := range waited {
			require.Equal(t, "marker", ev.Name)
			assert.LessOrEqual(t, ev.Seq, rp.Seq)
			superseded = append(superseded, ev.Seq)
		}
		require.Len(t, superseded, 2)

		// an event past the watermark applies exactly once
		rig.hub.publish("marker", map[string]string{"v": "three"})
		ev := c.nextEvent()
		require.Equal(t, "marker", ev.Name)
		assert.Greater(t, ev.Seq, rp.Seq)
		var d struct {
			V string `json:"v"`
		}
		require.NoError(t, ev.Decode(&d))
		assert.Equal(t, "three", d.V)
	})

	t.Run("watermark_supersedes_own_effects", func(t *testing.T) {
		rig := newControlRig(t, nil)
		c := dialControl(t, rig.path)
		c.hello()

		// the state.get reply carries the seq its snapshot was taken at, so an
		// event racing the read can never sit below a reconciled watermark
		rp := c.call("s", ctl.MethodState, map[string]any{})
		require.Nil(t, rp.Error)
		snapSeq := rp.Seq // zero is legal: the hub has published nothing yet

		for range 3 {
			rig.hub.publish("marker", nil)
		}
		rp = c.call("s2", ctl.MethodState, map[string]any{})
		require.Nil(t, rp.Error)
		assert.Equal(t, snapSeq+3, rp.Seq) // exactly the three markers, nothing between
	})

	t.Run("concurrent_observers", func(t *testing.T) {
		rig := newControlRig(t, nil)
		clients := make([]*ctlClient, 0, 3)
		for range 3 {
			c := dialControl(t, rig.path)
			c.hello()
			clients = append(clients, c)
		}

		for i := range 5 {
			rig.hub.publish("marker", map[string]int{"i": i})
		}
		for _, c := range clients {
			rp := c.call("s", ctl.MethodState, map[string]any{})
			require.Nil(t, rp.Error)
		}

		for _, c := range clients {
			markers := c.collect("marker", 5)
			seqs := make([]uint64, 0, len(markers))
			for _, ev := range markers {
				seqs = append(seqs, ev.Seq)
			}
			assert.Equal(t, []uint64{1, 2, 3, 4, 5}, seqs)
		}
	})
}
