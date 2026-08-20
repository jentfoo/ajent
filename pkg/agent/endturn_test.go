package agent

import (
	"context"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// endTool returns a canned result carrying the EndTurn / IsError combination
// under test.
type endTool struct {
	name     string
	endTurn  bool
	isError  bool
	parallel bool
}

func (t *endTool) Name() string           { return t.name }
func (t *endTool) Label(ToolCall) string  { return t.name }
func (t *endTool) Description() string    { return "control tool" }
func (t *endTool) Schema() llm.ToolSchema { return llm.ToolSchema{Name: t.name} }
func (t *endTool) Mode() ExecutionMode {
	if t.parallel {
		return ModeParallel
	}
	return ModeSerial
}

func (t *endTool) Execute(context.Context, ToolCall, Output) (ToolResult, error) {
	return ToolResult{
		Content: llm.BlockList{llm.TextBlock{Text: "recorded"}},
		IsError: t.isError,
		EndTurn: t.endTurn,
	}, nil
}

func TestToolResultEndTurn(t *testing.T) {
	t.Parallel()

	t.Run("ends_the_turn", func(t *testing.T) {
		p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
			{Events: toolCallEvents("1", "dev")},
			{Events: textOnly("should never stream")},
		}}
		catch := &resultCatcher{}
		a := newTestAgent(nil, p, catch)
		a.opts.Tools = &mapSet{tools: map[string]Tool{"dev": &endTool{name: "dev", endTurn: true}}}

		require.NoError(t, a.Prompt(t.Context(), Input{Text: "x"}))
		assert.Len(t, p.Requests(), 1) // the queued second turn is never reached
		assert.Equal(t, llm.StopEndTurn, catch.result.Stop)
	})

	t.Run("without_flag_continues", func(t *testing.T) {
		p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
			{Events: toolCallEvents("1", "dev")},
			{Events: textOnly("done")},
		}}
		a := newTestAgent(nil, p, nil)
		a.opts.Tools = &mapSet{tools: map[string]Tool{"dev": &endTool{name: "dev"}}}

		require.NoError(t, a.Prompt(t.Context(), Input{Text: "x"}))
		assert.Len(t, p.Requests(), 2)
	})

	// the v1 regression: a rejected control call must leave the model free to
	// correct itself inside the same turn.
	t.Run("error_result_does_not_end", func(t *testing.T) {
		p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
			{Events: toolCallEvents("1", "dev")},
			{Events: textOnly("retrying")},
		}}
		a := newTestAgent(nil, p, nil)
		a.opts.Tools = &mapSet{tools: map[string]Tool{
			"dev": &endTool{name: "dev", endTurn: true, isError: true},
		}}

		require.NoError(t, a.Prompt(t.Context(), Input{Text: "x"}))
		assert.Len(t, p.Requests(), 2)
	})

	t.Run("parallel_batch_ends", func(t *testing.T) {
		p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
			{Events: append(twoToolCalls("1", "read", "2", "dev"), doneEvent())},
			{Events: textOnly("should never stream")},
		}}
		a := newTestAgent(nil, p, nil)
		a.opts.Tools = &mapSet{tools: map[string]Tool{
			"read": &endTool{name: "read", parallel: true},
			"dev":  &endTool{name: "dev", parallel: true, endTurn: true},
		}}
		a.state.Model.Caps.ParallelTools = true

		require.NoError(t, a.Prompt(t.Context(), Input{Text: "x"}))
		assert.Len(t, p.Requests(), 1)
	})
}
