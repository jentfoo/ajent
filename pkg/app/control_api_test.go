package app

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
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
	require.Nil(t, rp.Error, "attach should claim the seat: %v", rp.Error)
	var res ctl.AttachResult
	require.NoError(t, rp.DecodeResult(&res))
	return res
}

// TestControlSeatSingleHolder pins the seat contract: one controller claims,
// a second claim refuses naming the holder, and every write method refuses on
// an observer connection naming whoever holds it.
func TestControlSeatSingleHolder(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
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
		ctl.MethodInputSet, ctl.MethodRewind, ctl.MethodResolve} {
		rp = two.call("obs-"+m, m, map[string]any{})
		require.NotNil(t, rp.Error, "%s must refuse an observer", m)
		assert.Equal(t, ctl.CodeObserver, rp.Error.Code, "%s", m)
		assert.Contains(t, rp.Error.Message, "ci-runner", "%s names the holder", m)
	}

	// read methods stay open to the observer
	rp = two.call("st", ctl.MethodState, map[string]any{})
	require.Nil(t, rp.Error)
}

// TestControlSeatReleasedOnDisconnect pins release-on-EOF: the holder going
// away frees the seat at once, so a waiting controller attaches immediately
// without any cooperating call.
func TestControlSeatReleasedOnDisconnect(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newControlRig(t, nil)
	one := dialControl(t, rig.path)
	one.hello()
	two := dialControl(t, rig.path)
	two.hello()
	attachControl(t, one, "first")

	require.NoError(t, one.conn.Close())
	require.Eventually(t, func() bool {
		rig.srv.mu.Lock()
		defer rig.srv.mu.Unlock()
		return rig.srv.seat == nil
	}, ctlWait*2, 5*time.Millisecond, "the seat never released after the holder disconnected")

	attachControl(t, two, "second")
}

// TestControlSeatRetake pins re-claim and takeover: the holder's own attach is
// idempotent, a second live claim still refuses naming the holder, and force
// moves the seat so the displaced holder's writes refuse naming the new one.
func TestControlSeatRetake(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
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

	// the displaced holder writes no more: refusals name the new holder
	rp = one.call("w", ctl.MethodInterrupt, map[string]any{})
	require.NotNil(t, rp.Error)
	assert.Equal(t, ctl.CodeObserver, rp.Error.Code)
	assert.Contains(t, rp.Error.Message, "second")
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

// TestControlModeCycleMatchesShiftTab pins mode.cycle: the reply names the new
// mode, the hub's barrier stepped in the asked direction, and the permission
// notice reached observers.
func TestControlModeCycleMatchesShiftTab(t *testing.T) {
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

	spy.mu.Lock()
	defer spy.mu.Unlock()
	assert.Equal(t, []string{"cycle", "prev"}, spy.calls)
}

// TestControlSubmitRefusals pins the honest-refusal rules: an empty line
// errors rather than dispatching, and the startup window before the router
// exists is named, never a silent no-op.
func TestControlSubmitRefusals(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
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
	q := newSteerQueue(rig.hub.ui, func(int) {}, func() {}, stager.WaitReady, stager.TakeReady)
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

// TestControlSubmitSteersAndInterrupts pins the steer rules over the socket
// with the real pump: a submitted prompt drives a turn carrying its controller
// origin onto the transcript, a submit while it runs queues truthfully, and
// interrupt stops the turn naming it, dropping the queued rows in a serve run.
func TestControlSubmitSteersAndInterrupts(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
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
	require.Eventually(t, func() bool {
		select {
		case <-entered:
			return true
		default:
			return false
		}
	}, ctlWait, 5*time.Millisecond, "the submitted prompt never started its turn")

	// a second submit queues: the queued event reflects it, never a silent drop
	rp = c.call("s2", ctl.MethodSubmit, map[string]any{"line": "second"})
	require.Nil(t, rp.Error)
	qd := c.collect(ctl.EventQueued, 1)[0]
	var q ctl.QueuedData
	require.NoError(t, qd.Decode(&q))
	assert.Equal(t, []string{"second"}, q.Texts)

	// interrupt names the turn and drops the queued row: no editor exists to
	// recover it into
	rp = c.call("i1", ctl.MethodInterrupt, map[string]any{})
	require.Nil(t, rp.Error)
	var res ctl.InterruptResult
	require.NoError(t, rp.DecodeResult(&res))
	assert.Equal(t, "turn", res.Stopped)

	qd = c.collect(ctl.EventQueued, 1)[0]
	require.NoError(t, qd.Decode(&q))
	assert.Empty(t, q.Texts)
	c.collect(ctl.EventTurnEnd, 1) // the aborted turn closed

	// the submitted prompt reached the transcript carrying its controller
	// origin, byte-identical to the typed text: no marker rides the message
	rp = c.call("tr", ctl.MethodTranscript, map[string]any{})
	require.Nil(t, rp.Error)
	var tr ctl.TranscriptResult
	require.NoError(t, rp.DecodeResult(&tr))
	var origins []string
	for _, e := range tr.Entries {
		if e.Type != string(session.TypeMessage) {
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
	}
	assert.Equal(t, []string{submitOriginController}, origins)
}

// TestControlInputSetPrefill pins input.set on an interactive run: the editor
// buffer takes the text and the draft surfaces as an editor event.
func TestControlInputSetPrefill(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
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

// TestControlInputSetHeldByDialog pins the refusal: a dialog owning the input
// refuses a prefill naming its kind, and the dialog itself still resolves.
func TestControlInputSetHeldByDialog(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
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
}

// TestControlInputSetServeRefused pins the serve rule: no editor exists, so a
// prefill is an error rather than a silent drop.
func TestControlInputSetServeRefused(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newServeRig(t, nil)
	c := dialControl(t, rig.path)
	c.hello()
	attachControl(t, c, "s")

	rp := c.call("i", ctl.MethodInputSet, map[string]any{"text": "x"})
	require.NotNil(t, rp.Error)
	assert.Equal(t, ctl.CodeUnavailable, rp.Error.Code)
	assert.Contains(t, rp.Error.Message, "no editor")
}

// TestControlAskUserHoldsForRemote pins the ask_user path over the socket: a
// serve run announces the question and holds until the controller answers it,
// by index or in its own words.
func TestControlAskUserHoldsForRemote(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
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
}

// TestControlResolveAnswerShapes pins the interaction.resolve contract: every
// announced kind answers with its natural shape, wrong shapes and out-of-range
// indexes refuse legibly, and stale ids error after End.
func TestControlResolveAnswerShapes(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
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
}

// TestControlDialogRaceLocalFirst pins the phase 33 race over the socket: a
// local answer winning leaves the controller's resolve settled, and the
// outcome every observer sees is the one first answer, idempotent.
func TestControlDialogRaceLocalFirst(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
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
}

// TestControlDecisionResolvesThroughBarrier pins the barrier path: an approval
// decision answers remotely through the same settle the mode-change path
// uses, so whichever resolves first wins and the loser reads settled.
func TestControlDecisionResolvesThroughBarrier(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
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
}

// TestControlDisconnectLeavesDialogOpen pins the hold-on-dialog rule: a
// controller dying mid-dialog neither denies nor resolves it; the dialog stays
// open and a reattached controller answers it.
func TestControlDisconnectLeavesDialogOpen(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
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
	require.Eventually(t, func() bool {
		rig.srv.mu.Lock()
		defer rig.srv.mu.Unlock()
		return rig.srv.seat == nil
	}, ctlWait*2, 5*time.Millisecond, "the seat never released after the holder disconnected")
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
	rig.hub.rewindTo = func(head string) (string, error) {
		stopped = true // the driver ends a plan workflow before switching
		return rig.rec.rewindTo(rig.hub.ui, ag, reg, head)
	}
	return &controlRewind{stopped: &stopped, entered: entered, release: release}
}

// TestControlRewindForksBranch pins session.rewind: a named head rebuilds the
// same branch the double-Esc picker would, unknown heads error legibly, and a
// running turn refuses with nothing moved.
func TestControlRewindForksBranch(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
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

	ev := c.collect(ctl.EventHead, 1)[0]
	var hd ctl.HeadData
	require.NoError(t, ev.Decode(&hd))
	assert.Equal(t, res.Head, hd.ID)

	rp = c.call("tr", ctl.MethodTranscript, map[string]any{})
	require.Nil(t, rp.Error)
	var tr ctl.TranscriptResult
	require.NoError(t, rp.DecodeResult(&tr))
	assert.Equal(t, res.Head, tr.Head)
	var texts []string
	for _, e := range tr.Entries {
		if e.Type != string(session.TypeMessage) {
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
}

// TestControlRewindUnavailableWhileStarting pins the startup window: before
// the driver wires the rewinder, the reply names it rather than no-op.
func TestControlRewindUnavailableWhileStarting(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newServeRig(t, nil)
	c := dialControl(t, rig.path)
	c.hello()
	attachControl(t, c, "early")

	rp := c.call("rw", ctl.MethodRewind, map[string]any{"head": "01ANY"})
	require.NotNil(t, rp.Error)
	assert.Equal(t, ctl.CodeUnavailable, rp.Error.Code)
}
