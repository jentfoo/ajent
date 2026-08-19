package agent

import (
	"sync"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSteerIdleRejected verifies Steer/FollowUp refuse work when no turn runs,
// so the caller knows to call Prompt instead.
func TestSteerIdleRejected(t *testing.T) {
	t.Parallel()

	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textOnly("hi")}}}
	a := newTestAgent(nil, p, nil)

	assert.False(t, a.Steer(Input{Text: "x"}))
	assert.False(t, a.FollowUp(Input{Text: "x"}))
}

// TestRunningReportsState verifies the running flag flips for the duration of a
// turn and clears after.
func TestRunningReportsState(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	set := &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", result: "ok", block: block}}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: toolCallEvents("c1", "bash")},
		{Events: textOnly("done")},
	}}
	a := newTestAgent(nil, p, nil)
	a.opts.Tools = set

	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), Input{Text: "x"}) }()
	assert.Eventually(t, func() bool { return a.Running() }, defaultTimeout, pollInterval)

	close(block) // let it finish
	require.NoError(t, <-errCh)
	assert.False(t, a.Running())
}

// TestSteerDeliveredFiresWhenLanded asserts an input's Delivered hook runs once
// the steer actually lands in state at a step boundary.
func TestSteerDeliveredFiresWhenLanded(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	set := &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", result: "ok", block: block}}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: toolCallEvents("c1", "bash")},
		{Events: textOnly("done")},
	}}
	a := newTestAgent(nil, p, nil)
	a.opts.Tools = set

	var delivered int
	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), Input{Text: "x"}) }()
	assert.Eventually(t, func() bool { return a.Running() }, defaultTimeout, pollInterval)

	require.True(t, a.Steer(Input{Text: "nudge", Delivered: func() { delivered++ }}))
	close(block) // let the tool finish so the steer lands at the next step boundary
	require.NoError(t, <-errCh)
	assert.Equal(t, 1, delivered)
}

// TestOnBoundaryPullsAtStepBoundary verifies the OnBoundary hook hands queued
// inputs to drainSteer at a step boundary mid-turn: they land as one user message
// after the tool result and before the assistant's next call, with their Delivered
// hooks fired.
func TestOnBoundaryPullsAtStepBoundary(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	set := &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", result: "ok", block: block}}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: toolCallEvents("c1", "bash")},
		{Events: textOnly("done")},
	}}
	a := newTestAgent(nil, p, nil)
	a.opts.Tools = set

	// the callback is armed mid-turn (once bash blocks) so its input lands at the
	// step boundary after the tool result rather than riding turn one. The mutex
	// keeps arming race-free against the loop goroutine reading it.
	var mu sync.Mutex
	pending := []Input{}
	a.opts.OnBoundary = func() []Input {
		mu.Lock()
		defer mu.Unlock()
		if len(pending) == 0 {
			return nil
		}
		out := pending
		pending = nil
		return out
	}

	var delivered int
	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), Input{Text: "x"}) }()
	assert.Eventually(t, func() bool { return a.Running() }, defaultTimeout, pollInterval)

	// wait until bash is executing (blocked), then arm and release it.
	tool := set.tools["bash"].(*stubTool)
	assert.Eventually(t, func() bool { return tool.callCount() >= 1 }, defaultTimeout, pollInterval)
	mu.Lock()
	pending = []Input{{Text: "nudge", Delivered: func() { delivered++ }}}
	mu.Unlock()

	close(block)
	require.NoError(t, <-errCh)
	assert.Equal(t, 1, delivered)

	reqs := p.Requests()
	require.Len(t, reqs, 2)
	last := reqs[1].Messages[len(reqs[1].Messages)-1]
	require.Equal(t, llm.RoleUser, last.Role)
	tb, ok := last.Content[0].(llm.TextBlock)
	require.True(t, ok)
	assert.Equal(t, "nudge", tb.Text)
}

// TestOnBoundaryEmptyIsNoop verifies a nil-returning OnBoundary leaves the turn
// unaffected.
func TestOnBoundaryEmptyIsNoop(t *testing.T) {
	t.Parallel()

	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textOnly("hi")}}}
	a := newTestAgent(nil, p, nil)
	called := 0
	a.opts.OnBoundary = func() []Input {
		called++
		return nil
	}

	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), Input{Text: "x"}) }()
	require.NoError(t, <-errCh)
	assert.Equal(t, 1, called) // a single-step turn hits exactly one boundary
}

// TestSteerEmptyNeverDelivered asserts an empty steer that would inject a blank
// user turn is skipped and its Delivered hook never fires.
func TestSteerEmptyNeverDelivered(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	set := &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", result: "ok", block: block}}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: toolCallEvents("c1", "bash")},
		{Events: textOnly("done")},
	}}
	a := newTestAgent(nil, p, nil)
	a.opts.Tools = set

	var delivered int
	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), Input{Text: "x"}) }()
	assert.Eventually(t, func() bool { return a.Running() }, defaultTimeout, pollInterval)

	require.True(t, a.Steer(Input{Delivered: func() { delivered++ }}))
	close(block)
	require.NoError(t, <-errCh)
	assert.Equal(t, 0, delivered) // nothing landed, so no confirmation
}
