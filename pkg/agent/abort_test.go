package agent

import (
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wellFormed reports whether every ToolCallBlock in messages has a matching
// ToolResultBlock, which is what keeps the next Anthropic request valid.
func wellFormed(msgs []llm.Message) bool {
	calls := 0
	for _, m := range msgs {
		for _, b := range m.Content {
			switch blk := b.(type) {
			case llm.ToolCallBlock:
				calls++
			case llm.ToolResultBlock:
				if blk.CallID == "" {
					return false // an unanswered tool_use would 400 the next request
				}
				calls--
			}
		}
	}
	return calls == 0
}

// TestInterruptMidStream cancels while a turn is streaming text and checks the
// partial assistant message stands with StopAborted.
func TestInterruptMidStream(t *testing.T) {
	t.Parallel()

	gp := &hangProvider{turn: textOnly("hello ")}
	catch := &resultCatcher{}
	a := newTestAgent(nil, gp, catch)

	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), Input{Text: "x"}) }()

	require.Eventually(t, func() bool { return gp.current() != nil }, defaultTimeout, pollInterval,
		"the stream must be created before cancelling")
	a.Interrupt()

	require.NoError(t, <-errCh) // an interrupt is a clean stop reason, not an error
	assert.Equal(t, llm.StopAborted, catch.result.Stop)
}

// TestInterruptDuringToolExecution verifies synthetic results fill unanswered
// calls so the transcript stays well formed.
func TestInterruptDuringToolExecution(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	set := &mapSet{tools: map[string]Tool{"bash": &stubTool{name: "bash", result: "ok", block: block}}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: toolCallEvents("c1", "bash")},
	}}
	catch := &resultCatcher{}
	a := newTestAgent(nil, p, catch)
	a.opts.Tools = set

	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), Input{Text: "x"}) }()
	require.Eventually(t, func() bool {
		return set.tools["bash"].(*stubTool).callCount() == 1
	}, defaultTimeout, pollInterval, "the tool must be executing before the interrupt")

	a.Interrupt()

	require.NoError(t, <-errCh)
	assert.Equal(t, llm.StopAborted, catch.result.Stop)
	assert.True(t, wellFormed(a.state.Messages), "every tool call needs a matching result")
}

// TestInterruptTwoUnansweredCalls interrupts while two tools are mid-execution
// and asserts both get synthetic results.
func TestInterruptTwoUnansweredCalls(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	set := &mapSet{tools: map[string]Tool{
		"a": &stubTool{name: "a", result: "ra", block: block, parallel: true},
		"b": &stubTool{name: "b", result: "rb", block: block, parallel: true},
	}}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: twoToolCalls("c1", "a", "c2", "b")},
	}}
	a := newTestAgent(nil, p, nil)
	a.opts.Tools = set
	a.state.Model.Caps.ParallelTools = true

	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), Input{Text: "x"}) }()
	require.Eventually(t, func() bool {
		aTool := set.tools["a"].(*stubTool)
		bTool := set.tools["b"].(*stubTool)
		return aTool.callCount() == 1 && bTool.callCount() == 1
	}, defaultTimeout, pollInterval, "both tools must start before the interrupt")

	a.Interrupt()

	require.NoError(t, <-errCh)
	assert.True(t, wellFormed(a.state.Messages), "every tool call needs a matching result")
}
