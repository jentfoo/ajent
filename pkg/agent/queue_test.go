package agent

import (
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
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

// TestInterruptIdleIsNoop verifies Interrupt on an idle agent clears nothing and
// does not error.
func TestInterruptIdleIsNoop(t *testing.T) {
	t.Parallel()

	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textOnly("hi")}}}
	a := newTestAgent(nil, p, nil)

	assert.NotPanics(t, a.Interrupt)
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
	assert.Eventually(t, func() bool { return a.Running() }, defaultTimeout, pollInterval,
		"running must be true while the tool is in flight")

	close(block) // let it finish
	requireNoError := <-errCh
	if requireNoError != nil {
		t.Fatalf("prompt: %v", requireNoError)
	}
	assert.False(t, a.Running())
}
