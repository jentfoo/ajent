package agent

import (
	"encoding/json"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInputBeforeAppendedAheadOfText asserts synthetic messages in Input.Before
// land in transcript order ahead of the user's own text, so a staged tool call
// + result pair reads as context for what the user then said.
func TestInputBeforeAppendedAheadOfText(t *testing.T) {
	t.Parallel()

	var seen []llm.Message
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textOnly("ok")}}}
	a := newTestAgent(nil, p, nil)
	a.opts.OnMessage = func(info MessageInfo) { seen = append(seen, info.Message) }

	before := []llm.Message{
		{Role: llm.RoleAssistant, Content: llm.BlockList{llm.ToolCallBlock{
			ID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"echo hi"}`),
		}}},
		{Role: llm.RoleUser, Content: llm.BlockList{llm.ToolResultBlock{
			CallID: "c1", Content: llm.BlockList{llm.TextBlock{Text: "hi"}},
		}}},
	}
	err := a.Prompt(t.Context(), Input{Before: before, Text: "what was that?"})
	require.NoError(t, err)

	// before pair, then the user text, then the assistant reply
	require.GreaterOrEqual(t, len(seen), 3)
	assert.Equal(t, llm.RoleAssistant, seen[0].Role) // staged tool call
	assert.Equal(t, llm.RoleUser, seen[1].Role)      // staged tool result
	assert.Equal(t, llm.RoleUser, seen[2].Role)      // the user's actual text
	tb, ok := seen[2].Content[0].(llm.TextBlock)
	require.True(t, ok)
	assert.Equal(t, "what was that?", tb.Text)
}

// TestNewOutputForwardsToSink asserts NewOutput streams writes and diffs to the
// sink under the call id, so a host-run tool displays like an agent-run one.
func TestNewOutputForwardsToSink(t *testing.T) {
	t.Parallel()

	sink := &recordingSink{}
	out := NewOutput(sink, "c1")

	_, err := out.Write([]byte("hello"))
	require.NoError(t, err)
	out.Diff("a.go", "before", "after")

	assert.Contains(t, sink.calls, "tool_output")
	assert.Contains(t, sink.calls, "diff")
}
