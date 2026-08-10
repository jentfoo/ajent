package llm

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// textTurn is a complete assistant turn with thinking, text and one tool call.
func textTurn() []Event {
	return []Event{
		{Type: EventMessageStart, Meta: &StreamMeta{Model: "m1", RequestID: "req1"}},
		{Type: EventThinkingStart, Index: 0},
		{Type: EventThinkingDelta, Index: 0, Text: "pon"},
		{Type: EventThinkingDelta, Index: 0, Text: "dering"},
		{Type: EventThinkingEnd, Index: 0, Block: ThinkingBlock{Text: "pondering", Signature: "sig"}},
		{Type: EventTextStart, Index: 1},
		{Type: EventTextDelta, Index: 1, Text: "Chec"},
		{Type: EventTextDelta, Index: 1, Text: "king"},
		{Type: EventTextEnd, Index: 1, Block: TextBlock{Text: "Checking"}},
		{Type: EventToolCallStart, Index: 2, ToolCallID: "c1", ToolName: "read"},
		{Type: EventToolCallDelta, Index: 2, Text: `{"pa`},
		{Type: EventToolCallDelta, Index: 2, Text: `th":"main.go"}`},
		{Type: EventToolCallEnd, Index: 2, ToolCallID: "c1", ToolName: "read",
			Block: ToolCallBlock{ID: "c1", Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)}},
		{Type: EventUsage, Usage: Usage{Input: 100, Output: 5}},
		{Type: EventUsage, Usage: Usage{Input: 412, Output: 37, CacheRead: 8}},
		{Type: EventDone, StopReason: StopToolUse},
	}
}

func TestAccumulate(t *testing.T) {
	t.Parallel()

	t.Run("full_turn", func(t *testing.T) {
		msg, usage, err := Accumulate(&SliceStream{Events: textTurn()})
		require.NoError(t, err)

		assert.Equal(t, RoleAssistant, msg.Role)
		assert.Equal(t, BlockList{
			ThinkingBlock{Text: "pondering", Signature: "sig"},
			TextBlock{Text: "Checking"},
			ToolCallBlock{ID: "c1", Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)},
		}, msg.Content)
		assert.Equal(t, Usage{Input: 412, Output: 37, CacheRead: 8}, usage) // last wins
	})
	t.Run("empty_stream", func(t *testing.T) {
		msg, usage, err := Accumulate(&SliceStream{})
		require.NoError(t, err)
		assert.Equal(t, Message{Role: RoleAssistant}, msg)
		assert.Zero(t, usage)
	})
	t.Run("stream_error_surfaces", func(t *testing.T) {
		want := errors.New("boom")
		_, _, err := Accumulate(&SliceStream{Events: textTurn(), Error: want})
		assert.ErrorIs(t, err, want)
	})
	t.Run("done_error_surfaces", func(t *testing.T) {
		want := errors.New("mid stream")
		_, _, err := Accumulate(&SliceStream{Events: []Event{
			{Type: EventTextStart, Index: 0},
			{Type: EventDone, StopReason: StopError, Err: want},
		}})
		assert.ErrorIs(t, err, want)
	})
	t.Run("aborted_keeps_partial_blocks", func(t *testing.T) {
		evs := textTurn()[:8] // cut before the text end event
		msg, _, err := Accumulate(&SliceStream{Events: evs})
		require.NoError(t, err)

		assert.Equal(t, BlockList{
			ThinkingBlock{Text: "pondering", Signature: "sig"},
			TextBlock{Text: "Checking"},
		}, msg.Content)
	})
	t.Run("aborted_tool_args_normalize", func(t *testing.T) {
		msg, _, err := Accumulate(&SliceStream{Events: []Event{
			{Type: EventToolCallStart, Index: 0, ToolCallID: "c1", ToolName: "read"},
			{Type: EventToolCallDelta, Index: 0, Text: `{"pa`},
		}})
		require.NoError(t, err)

		require.Len(t, msg.Content, 1)
		call, ok := msg.Content[0].(ToolCallBlock)
		require.True(t, ok)
		assert.JSONEq(t, `{}`, string(call.Input)) // unparseable partial is not passed on
	})
}

func TestAccumulatorAdd(t *testing.T) {
	t.Parallel()

	t.Run("reports_meta_and_stop", func(t *testing.T) {
		var a Accumulator
		for _, ev := range textTurn() {
			a.Add(ev)
		}
		assert.Equal(t, StreamMeta{Model: "m1", RequestID: "req1"}, a.Meta())
		assert.Equal(t, StopToolUse, a.StopReason())
		assert.NoError(t, a.Err())
	})
	t.Run("message_is_repeatable", func(t *testing.T) {
		var a Accumulator
		for _, ev := range textTurn()[:8] {
			a.Add(ev)
		}
		first := a.Message()
		second := a.Message()
		assert.Equal(t, first, second)
	})
	t.Run("delta_for_unopened_index_ignored", func(t *testing.T) {
		var a Accumulator
		a.Add(Event{Type: EventTextDelta, Index: 9, Text: "orphan"})
		assert.Empty(t, a.Message().Content)
	})
}

func TestSliceStream(t *testing.T) {
	t.Parallel()

	t.Run("drains_in_order", func(t *testing.T) {
		s := &SliceStream{Events: []Event{{Type: EventTextDelta, Text: "a"}, {Type: EventDone}}}
		var got []EventType
		for ev, ok := s.Next(); ok; ev, ok = s.Next() {
			got = append(got, ev.Type)
		}
		assert.Equal(t, []EventType{EventTextDelta, EventDone}, got)
		assert.NoError(t, s.Err())
	})
	t.Run("close_stops_iteration", func(t *testing.T) {
		s := &SliceStream{Events: textTurn()}
		_, ok := s.Next()
		require.True(t, ok)
		require.NoError(t, s.Close())

		_, ok = s.Next()
		assert.False(t, ok)
	})
}
