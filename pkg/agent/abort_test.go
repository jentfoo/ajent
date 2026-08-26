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

func TestInterrupt(t *testing.T) {
	t.Parallel()

	// cancelling while a turn is streaming text: the partial assistant message stands with StopAborted.
	t.Run("mid_stream", func(t *testing.T) {
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
		// ending the turn closes the model stream: zero incoming tokens after interrupt
		s := gp.current()
		require.NotNil(t, s)
		require.Eventually(t, s.isClosed, defaultTimeout, pollInterval,
			"the model stream must be closed when the turn ends")
	})

	// synthetic results fill unanswered calls so the transcript stays well formed.
	t.Run("during_tool_execution", func(t *testing.T) {
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
		assert.True(t, wellFormed(a.state.Messages)) // every tool call has a matching result
	})

	// two tools mid-execution both get synthetic results.
	t.Run("two_unanswered_calls", func(t *testing.T) {
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
		assert.True(t, wellFormed(a.state.Messages)) // every tool call has a matching result
	})
}

// TestAbortResults covers the pure mapping that keeps an interrupted transcript
// well formed: real results pass through in call order, missing calls get a
// synthetic error, empty-call-id results are ignored.
func TestAbortResults(t *testing.T) {
	t.Parallel()

	call := func(id string) llm.Block { return llm.ToolCallBlock{ID: id, Name: "bash"} }

	t.Run("real_results_preserve_call_order", func(t *testing.T) {
		// completion order c2 then c1 must come back in call order.
		out := abortResults(llm.Message{Content: llm.BlockList{
			call("c1"), call("c2"),
		}}, []llm.ToolResultBlock{{CallID: "c2"}, {CallID: "c1"}})
		assert.Equal(t, []string{"c1", "c2"}, resultIDs(out))
	})

	t.Run("missing_call_gets_synthetic_error", func(t *testing.T) {
		out := abortResults(llm.Message{Content: llm.BlockList{
			call("c1"), call("c2"),
		}}, []llm.ToolResultBlock{{CallID: "c2"}})
		require.Len(t, out, 2) // both calls filled in
		assert.Equal(t, "c1", out[0].CallID)
		assert.True(t, out[0].IsError)
		tb := out[0].Content[0].(llm.TextBlock)
		assert.Equal(t, InterruptedText, tb.Text)
	})

	t.Run("empty_call_id_result_ignored", func(t *testing.T) {
		out := abortResults(llm.Message{Content: llm.BlockList{
			call("c1"),
		}}, []llm.ToolResultBlock{{CallID: ""}})
		require.Len(t, out, 1)
		assert.True(t, out[0].IsError) // the empty-id result was not matched
	})
}

// resultIDs extracts call ids in order for assertions.
func resultIDs(blocks []llm.ToolResultBlock) []string {
	out := make([]string, len(blocks))
	for i, b := range blocks {
		out[i] = b.CallID
	}
	return out
}
