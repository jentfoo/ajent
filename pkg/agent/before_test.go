package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
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

// TestInputAfterAppendedBehindText asserts Input.After resolves once the user
// message has landed and its pairs follow it, so a rewind onto that message
// drops the context it asked for along with it.
func TestInputAfterAppendedBehindText(t *testing.T) {
	t.Parallel()

	var seen []MessageInfo
	var order []string
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textOnly("ok")}}}
	a := newTestAgent(nil, p, nil)
	a.opts.OnMessage = []func(MessageInfo){func(info MessageInfo) { seen = append(seen, info) }}

	after := func(context.Context) []llm.Message {
		order = append(order, "after")
		return []llm.Message{
			{Role: llm.RoleAssistant, Content: llm.BlockList{llm.ToolCallBlock{
				ID: "ref-1-a.go", Name: "read", Input: json.RawMessage(`{"path":"a.go"}`),
			}}},
			{Role: llm.RoleUser, Content: llm.BlockList{llm.ToolResultBlock{
				CallID: "ref-1-a.go", Content: llm.BlockList{llm.TextBlock{Text: "package a"}},
			}}},
		}
	}
	err := a.Prompt(t.Context(), Input{
		Text:      "explain @a.go",
		After:     after,
		Delivered: func() { order = append(order, "delivered") },
		Settled:   func() { order = append(order, "settled") },
	})
	require.NoError(t, err)

	// the user text, then its reads, then the assistant reply
	require.Len(t, seen, 4)
	tb, ok := seen[0].Message.Content[0].(llm.TextBlock)
	require.True(t, ok)
	assert.Equal(t, "explain @a.go", tb.Text)
	assert.False(t, seen[0].Injected)
	assert.Equal(t, llm.RoleAssistant, seen[1].Message.Role)
	assert.Equal(t, llm.RoleUser, seen[2].Message.Role)
	assert.True(t, seen[1].Injected)
	assert.True(t, seen[2].Injected)
	// the echo lands with the message so the reads render under it, and the reserve
	// is released only once they have been accounted
	assert.Equal(t, []string{"delivered", "after", "settled"}, order)
}

// TestInputAfterOnlyStillLands asserts an input carrying only After is not
// dropped as an empty steer.
func TestInputAfterOnlyStillLands(t *testing.T) {
	t.Parallel()

	var seen []llm.Message
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textOnly("ok")}}}
	a := newTestAgent(nil, p, nil)
	a.opts.OnMessage = []func(MessageInfo){func(info MessageInfo) { seen = append(seen, info.Message) }}

	err := a.Prompt(t.Context(), Input{After: func(context.Context) []llm.Message {
		return []llm.Message{{Role: llm.RoleUser, Content: llm.BlockList{llm.TextBlock{Text: "ctx"}}}}
	}})
	require.NoError(t, err)

	require.Len(t, seen, 2)
	tb, ok := seen[0].Content[0].(llm.TextBlock)
	require.True(t, ok)
	assert.Equal(t, "ctx", tb.Text)
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

// TestInputSettledSeesReadsAccounted asserts a host's submit reserve outlives the
// reads it was holding tokens for. Releasing it on Delivered dropped the whole
// reserve for the repaint between the two, so a large @ read flashed the bar down
// and straight back up as its pair landed.
func TestInputSettledSeesReadsAccounted(t *testing.T) {
	t.Parallel()

	model := llm.Model{ID: "test", ContextWindow: 100000}
	ledger := tokens.New(model)
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textOnly("ok")}}}
	a := newTestAgent(&State{Model: model, Tokens: ledger}, p, nil)

	read := llm.Message{Role: llm.RoleUser, Content: llm.BlockList{llm.ToolResultBlock{
		CallID: "ref-1-a.go", Content: llm.BlockList{llm.TextBlock{Text: strings.Repeat("x", 8000)}},
	}}}
	reserve := tokens.EstimateMessages([]llm.Message{read})
	ledger.SetSubmit(reserve) // the host holds the reads' tokens from submit

	var atDelivered, afterRelease int
	err := a.Prompt(t.Context(), Input{
		Text:      "explain @a.go",
		After:     func(context.Context) []llm.Message { return []llm.Message{read} },
		Delivered: func() { atDelivered = ledger.Context().Used },
		Settled: func() {
			ledger.SetSubmit(0)
			afterRelease = ledger.Context().Used
		},
	})
	require.NoError(t, err)

	require.NotZero(t, atDelivered)
	// releasing the reserve must not take the bar below where delivery left it:
	// pending has to have grown to cover the reads first
	assert.GreaterOrEqual(t, afterRelease, atDelivered)
}
