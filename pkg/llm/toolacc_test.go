package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolAccumulatorDelta(t *testing.T) {
	t.Parallel()

	t.Run("announces_then_streams_fragments", func(t *testing.T) {
		a := newToolAccumulator(0)
		got := a.Delta(0, "call_1", "read", `{"pa`)
		require.Len(t, got, 2)
		assert.Equal(t, Event{Type: EventToolCallStart, Index: 0, ToolCallID: "call_1", ToolName: "read"}, got[0])
		assert.Equal(t, Event{Type: EventToolCallDelta, Index: 0, ToolCallID: "call_1", Text: `{"pa`}, got[1])

		got = a.Delta(0, "", "", `th":"main.go"}`)
		require.Len(t, got, 1)
		assert.Equal(t, EventToolCallDelta, got[0].Type)
	})
	t.Run("buffers_fragments_seen_before_the_name", func(t *testing.T) {
		a := newToolAccumulator(0)
		got := a.Delta(0, "call_1", "", `{"a`) // name not known yet
		assert.Empty(t, got)

		got = a.Delta(0, "", "read", `":1}`)
		require.Len(t, got, 3)
		assert.Equal(t, EventToolCallStart, got[0].Type)
		assert.Equal(t, `{"a`, got[1].Text) // replayed in order
		assert.Equal(t, `":1}`, got[2].Text)
	})
	t.Run("later_name_overwrites_an_empty_one", func(t *testing.T) {
		a := newToolAccumulator(0)
		a.Delta(0, "call_1", "", "")
		got := a.Delta(0, "", "bash", "")
		require.NotEmpty(t, got)
		assert.Equal(t, "bash", got[0].ToolName)
	})
	t.Run("parallel_calls_get_distinct_block_indexes", func(t *testing.T) {
		a := newToolAccumulator(1)
		first := a.Delta(0, "c1", "read", "{}")
		second := a.Delta(1, "c2", "write", "{}")

		assert.Equal(t, 1, first[0].Index)
		assert.Equal(t, 2, second[0].Index)
	})
	t.Run("base_offsets_the_block_index", func(t *testing.T) {
		a := newToolAccumulator(5)
		got := a.Delta(0, "c1", "read", "")
		require.NotEmpty(t, got)
		assert.Equal(t, 5, got[0].Index)
	})
}

func TestToolAccumulatorClose(t *testing.T) {
	t.Parallel()

	t.Run("emits_end_with_parsed_input", func(t *testing.T) {
		a := newToolAccumulator(0)
		a.Delta(0, "call_1", "read", `{"pa`)
		a.Delta(0, "", "", `th":"main.go"}`)

		got := a.Close()
		require.Len(t, got, 1)
		assert.Equal(t, EventToolCallEnd, got[0].Type)
		require.NoError(t, got[0].Err)

		block, ok := got[0].Block.(ToolCallBlock)
		require.True(t, ok)
		assert.JSONEq(t, `{"path":"main.go"}`, string(block.Input))
	})
	t.Run("does_not_complete_on_early_parseable_json", func(t *testing.T) {
		// {"a":1} parses long before ,"b":2 arrives, so only the boundary ends it
		a := newToolAccumulator(0)
		a.Delta(0, "c1", "read", `{"a":1`)
		got := a.Delta(0, "", "", `,"b":2}`)
		for _, ev := range got {
			assert.NotEqual(t, EventToolCallEnd, ev.Type)
		}

		closed := a.Close()
		require.Len(t, closed, 1)
		block := closed[0].Block.(ToolCallBlock)
		assert.JSONEq(t, `{"a":1,"b":2}`, string(block.Input))
	})
	t.Run("closes_in_arrival_order", func(t *testing.T) {
		a := newToolAccumulator(0)
		a.Delta(1, "c2", "write", "{}")
		a.Delta(0, "c1", "read", "{}")

		got := a.Close()
		require.Len(t, got, 2)
		assert.Equal(t, "c2", got[0].ToolCallID)
		assert.Equal(t, "c1", got[1].ToolCallID)
	})
	t.Run("empty_arguments_become_an_object", func(t *testing.T) {
		a := newToolAccumulator(0)
		a.Delta(0, "c1", "now", "")

		got := a.Close()
		require.Len(t, got, 1)
		require.NoError(t, got[0].Err)
		block := got[0].Block.(ToolCallBlock)
		assert.JSONEq(t, `{}`, string(block.Input))
	})
	t.Run("empty_string_arguments_become_an_object", func(t *testing.T) {
		a := newToolAccumulator(0)
		a.Delta(0, "c1", "now", `""`)

		got := a.Close()
		require.Len(t, got, 1)
		require.NoError(t, got[0].Err)
		block := got[0].Block.(ToolCallBlock)
		assert.JSONEq(t, `{}`, string(block.Input))
	})
	t.Run("malformed_arguments_fail_the_call_not_the_turn", func(t *testing.T) {
		a := newToolAccumulator(0)
		a.Delta(0, "c1", "read", `{"path":`)

		got := a.Close()
		require.Len(t, got, 1)
		assert.Equal(t, EventToolCallEnd, got[0].Type) // still emitted
		assert.ErrorIs(t, got[0].Err, ErrMalformedToolArgs)
	})
	t.Run("close_is_idempotent", func(t *testing.T) {
		a := newToolAccumulator(0)
		a.Delta(0, "c1", "read", "{}")
		require.Len(t, a.Close(), 1)
		assert.Empty(t, a.Close())
	})
	t.Run("no_calls", func(t *testing.T) {
		assert.Empty(t, newToolAccumulator(0).Close())
	})
}

func TestFinishToolInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		raw      string
		expected string
		wantErr  bool
	}{
		{"object", `{"a":1}`, `{"a":1}`, false},
		{"whitespace_trimmed", "  {\"a\":1}\n", `{"a":1}`, false},
		{"empty", "", `{}`, false},
		{"empty_json_string", `""`, `{}`, false},
		{"truncated", `{"a":`, `{"a":`, true},
		{"unbalanced_brace", `{"a":1`, `{"a":1`, true},
		{"not_json", `oops`, `oops`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := finishToolInput(tc.raw)
			if tc.wantErr {
				require.ErrorIs(t, err, ErrMalformedToolArgs)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.expected, string(got))
		})
	}
}
