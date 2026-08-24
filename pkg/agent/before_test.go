package agent

import (
	"encoding/json"
	"strings"
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
	a.opts.OnMessage = []func(MessageInfo){func(info MessageInfo) { seen = append(seen, info.Message) }}

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
	require.Len(t, seen, 4)
	assert.Equal(t, llm.RoleAssistant, seen[0].Role) // staged tool call
	assert.Equal(t, llm.RoleUser, seen[1].Role)      // staged tool result
	assert.Equal(t, llm.RoleUser, seen[2].Role)      // the user's actual text
	tb, ok := seen[2].Content[0].(llm.TextBlock)
	require.True(t, ok)
	assert.Equal(t, "what was that?", tb.Text)
}

// TestInputBeforeMarkedInjected asserts Input.Before messages are appended as
// injected context so prompt recall (Ctrl+R / up-arrow) excludes them.
func TestInputBeforeMarkedInjected(t *testing.T) {
	t.Parallel()

	var infos []MessageInfo
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textOnly("ok")}}}
	a := newTestAgent(nil, p, nil)
	a.opts.OnMessage = []func(MessageInfo){func(info MessageInfo) { infos = append(infos, info) }}

	before := []llm.Message{
		{Role: llm.RoleUser, Content: llm.BlockList{llm.TextBlock{Text: "User Ran: echo hi"}}},
	}
	err := a.Prompt(t.Context(), Input{Before: before, Text: "next?"})
	require.NoError(t, err)

	// the Before message and the typed prompt both land; only the former is injected.
	var gotInjected *MessageInfo
	for i := range infos {
		tb, ok := infos[i].Message.Content[0].(llm.TextBlock)
		if ok && strings.Contains(tb.Text, "User Ran: echo hi") {
			gotInjected = &infos[i]
		}
	}
	require.NotNil(t, gotInjected)
	assert.True(t, gotInjected.Injected)
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

	// writes stream as tool output, then the diff lands after it.
	assert.Equal(t, []string{"tool_output", "diff"}, sink.calls)
}
