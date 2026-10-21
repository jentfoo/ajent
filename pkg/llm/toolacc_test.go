package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolAccumulatorDelta(t *testing.T) {
	t.Parallel()

	t.Run("announces_then_streams_fragments", func(t *testing.T) {
		a := newToolAccumulator(&blockCounter{})
		got := a.Delta(0, "call_1", "read", `{"pa`)
		require.Len(t, got, 2)
		assert.Equal(t, Event{Type: EventToolCallStart, Index: 0, ToolCallID: "call_1", ToolName: "read"}, got[0])
		assert.Equal(t, Event{Type: EventToolCallDelta, Index: 0, ToolCallID: "call_1", Text: `{"pa`}, got[1])

		got = a.Delta(0, "", "", `th":"main.go"}`)
		require.Len(t, got, 1)
		assert.Equal(t, EventToolCallDelta, got[0].Type)
	})

	t.Run("buffers_fragments_seen_before_the_name", func(t *testing.T) {
		a := newToolAccumulator(&blockCounter{})
		got := a.Delta(0, "call_1", "", `{"a`) // name not known yet
		assert.Empty(t, got)

		got = a.Delta(0, "", "read", `":1}`)
		require.Len(t, got, 3)
		assert.Equal(t, EventToolCallStart, got[0].Type)
		assert.Equal(t, `{"a`, got[1].Text) // replayed in order
		assert.Equal(t, `":1}`, got[2].Text)
	})

	t.Run("later_name_overwrites_an_empty_one", func(t *testing.T) {
		a := newToolAccumulator(&blockCounter{})
		a.Delta(0, "call_1", "", "")
		got := a.Delta(0, "", "bash", "")
		require.NotEmpty(t, got)
		assert.Equal(t, "bash", got[0].ToolName)
	})

	t.Run("parallel_calls_get_distinct_block_indexes", func(t *testing.T) {
		a := newToolAccumulator(&blockCounter{next: 1})
		first := a.Delta(0, "c1", "read", "{}")
		second := a.Delta(1, "c2", "write", "{}")

		assert.Equal(t, 1, first[0].Index)
		assert.Equal(t, 2, second[0].Index)
	})

	t.Run("claims_skip_preclaimed_indexes", func(t *testing.T) {
		// regions opened before the first call already consumed 0..4
		a := newToolAccumulator(&blockCounter{next: 5})
		got := a.Delta(0, "c1", "read", "")
		require.NotEmpty(t, got)
		assert.Equal(t, 5, got[0].Index)
	})

	t.Run("claims_share_the_counter_with_regions", func(t *testing.T) {
		blocks := &blockCounter{}
		a := newToolAccumulator(blocks)
		first := a.Delta(0, "c1", "read", "{}")
		region := blocks.alloc() // a region opening between announcements
		second := a.Delta(1, "c2", "write", "{}")

		assert.Equal(t, 0, first[0].Index)
		assert.Equal(t, 1, region)
		assert.Equal(t, 2, second[0].Index)
	})

	t.Run("repeated_identity_does_not_fork", func(t *testing.T) {
		// providers repeat the id and name on every chunk of one call
		a := newToolAccumulator(&blockCounter{})
		a.Delta(0, "c1", "read", `{"a":`)
		got := a.Delta(0, "c1", "read", "1}")

		assert.Equal(t, EventToolCallDelta, got[0].Type)
		ends := a.Close()
		require.Len(t, ends, 1)
		assert.JSONEq(t, `{"a":1}`, string(ends[0].Block.(ToolCallBlock).Input))
	})

	t.Run("late_id_or_name_does_not_fork", func(t *testing.T) {
		// a value arriving where none was recorded belongs to the open call
		a := newToolAccumulator(&blockCounter{})
		first := a.Delta(0, "", "read", "")
		require.Len(t, first, 1) // announced by its name alone
		assert.Empty(t, a.Delta(0, "c1", "", ""))

		ends := a.Close()
		require.Len(t, ends, 1)
		assert.Equal(t, "c1", ends[0].ToolCallID)
	})

	t.Run("fresh_id_on_used_index_forks", func(t *testing.T) {
		a := newToolAccumulator(&blockCounter{})
		a.Delta(0, "c1", "read", `{"a":1}`)

		got := a.Delta(0, "c2", "read", `{"b":2}`)
		require.Len(t, got, 3)
		assert.Equal(t, EventToolCallEnd, got[0].Type) // the first call ended
		assert.Equal(t, "c1", got[0].ToolCallID)
		block, ok := got[0].Block.(ToolCallBlock)
		require.True(t, ok)
		assert.JSONEq(t, `{"a":1}`, string(block.Input))
		assert.Equal(t, EventToolCallStart, got[1].Type) // a distinct second block
		assert.Equal(t, "c2", got[1].ToolCallID)
		assert.NotEqual(t, got[0].Index, got[1].Index)
		assert.Equal(t, EventToolCallDelta, got[2].Type)
	})

	t.Run("fresh_name_on_used_index_forks", func(t *testing.T) {
		a := newToolAccumulator(&blockCounter{})
		a.Delta(0, "c1", "read", "{}")

		got := a.Delta(0, "c2", "write", "{}")
		require.Len(t, got, 3)
		assert.Equal(t, "read", got[0].ToolName)
		assert.Equal(t, "write", got[1].ToolName)
	})
}

func TestToolAccumulatorClose(t *testing.T) {
	t.Parallel()

	t.Run("emits_end_with_parsed_input", func(t *testing.T) {
		a := newToolAccumulator(&blockCounter{})
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
		a := newToolAccumulator(&blockCounter{})
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
		a := newToolAccumulator(&blockCounter{})
		a.Delta(1, "c2", "write", "{}")
		a.Delta(0, "c1", "read", "{}")

		got := a.Close()
		require.Len(t, got, 2)
		assert.Equal(t, "c2", got[0].ToolCallID)
		assert.Equal(t, "c1", got[1].ToolCallID)
	})

	t.Run("empty_arguments_become_an_object", func(t *testing.T) {
		a := newToolAccumulator(&blockCounter{})
		a.Delta(0, "c1", "now", "")

		got := a.Close()
		require.Len(t, got, 1)
		require.NoError(t, got[0].Err)
		block := got[0].Block.(ToolCallBlock)
		assert.JSONEq(t, `{}`, string(block.Input))
	})

	t.Run("empty_string_arguments_become_an_object", func(t *testing.T) {
		a := newToolAccumulator(&blockCounter{})
		a.Delta(0, "c1", "now", `""`)

		got := a.Close()
		require.Len(t, got, 1)
		require.NoError(t, got[0].Err)
		block := got[0].Block.(ToolCallBlock)
		assert.JSONEq(t, `{}`, string(block.Input))
	})

	t.Run("malformed_arguments_fail_the_call_not_the_turn", func(t *testing.T) {
		a := newToolAccumulator(&blockCounter{})
		a.Delta(0, "c1", "read", `{"path":`)

		got := a.Close()
		require.Len(t, got, 1)
		assert.Equal(t, EventToolCallEnd, got[0].Type) // still emitted
		assert.ErrorIs(t, got[0].Err, ErrMalformedToolArgs)
	})

	t.Run("close_is_idempotent", func(t *testing.T) {
		a := newToolAccumulator(&blockCounter{})
		a.Delta(0, "c1", "read", "{}")
		require.Len(t, a.Close(), 1)
		assert.Empty(t, a.Close())
	})

	t.Run("no_calls", func(t *testing.T) {
		assert.Empty(t, newToolAccumulator(&blockCounter{}).Close())
	})

	t.Run("unattributed_reuse_fails_the_call_not_the_turn", func(t *testing.T) {
		// fragments arriving on an index whose call already closed, carrying no
		// id or name, cannot be attributed: the end event names it and fails only
		// that call rather than merging into the closed one
		a := newToolAccumulator(&blockCounter{})
		a.Delta(0, "c1", "read", `{"a":1}`)
		require.Len(t, a.Close(), 1)
		assert.Empty(t, a.Delta(0, "", "", `{"b":2}`)) // unnamed, nothing to announce

		ends := a.Close()
		require.Len(t, ends, 1)
		assert.Equal(t, EventToolCallEnd, ends[0].Type)
		require.ErrorIs(t, ends[0].Err, ErrReusedToolIndex)
		block, ok := ends[0].Block.(ToolCallBlock)
		require.True(t, ok)
		assert.Equal(t, `{"b":2}`, string(block.Input)) // the evidence rides along
	})

	t.Run("matching_identity_after_close_stays_unattributed", func(t *testing.T) {
		// the same id and name cannot distinguish a restart from a stray late
		// fragment, so the reuse still fails visibly
		a := newToolAccumulator(&blockCounter{})
		a.Delta(0, "c1", "read", "{}")
		require.Len(t, a.Close(), 1)
		a.Delta(0, "c1", "read", `{"b":2}`)

		ends := a.Close()
		require.Len(t, ends, 1)
		assert.ErrorIs(t, ends[0].Err, ErrReusedToolIndex)
	})

	t.Run("empty_entry_after_close_is_ignored", func(t *testing.T) {
		// a phantom empty entry carries nothing to attribute or fail on
		a := newToolAccumulator(&blockCounter{})
		a.Delta(0, "c1", "read", "{}")
		require.Len(t, a.Close(), 1)

		assert.Empty(t, a.Delta(0, "", "", ""))
		assert.Empty(t, a.Close())
	})

	t.Run("fresh_identity_claims_fragments_buffered_after_close", func(t *testing.T) {
		// the id rides a later chunk: once it differs from the closed call's, the
		// fragments belong to the new call it names, and nothing fails
		a := newToolAccumulator(&blockCounter{})
		a.Delta(0, "c1", "read", "{}")
		require.Len(t, a.Close(), 1)
		claimed := a.Delta(0, "", "read", `{"b":`) // name matches: ambiguous
		require.NotEmpty(t, claimed)               // announced as a probable new call
		got := a.Delta(0, "c2", "", `2}`)
		require.Len(t, got, 1)
		assert.Equal(t, "c2", got[0].ToolCallID)

		ends := a.Close()
		require.Len(t, ends, 1)
		require.NoError(t, ends[0].Err)
		assert.JSONEq(t, `{"b":2}`, string(ends[0].Block.(ToolCallBlock).Input))
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
