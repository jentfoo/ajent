package agent

import (
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
