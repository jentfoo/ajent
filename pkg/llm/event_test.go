package llm

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUsageAdd(t *testing.T) {
	t.Parallel()

	t.Run("sums_every_field", func(t *testing.T) {
		u := Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4, Reasoning: 5}
		u.Add(Usage{Input: 10, Output: 20, CacheRead: 30, CacheWrite: 40, Reasoning: 50})
		assert.Equal(t, Usage{Input: 11, Output: 22, CacheRead: 33, CacheWrite: 44, Reasoning: 55}, u)
	})
	t.Run("zero_is_identity", func(t *testing.T) {
		u := Usage{Input: 7}
		u.Add(Usage{})
		assert.Equal(t, Usage{Input: 7}, u)
	})
}

func TestUsageMerge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		have Usage
		next Usage
		want Usage
	}{
		{
			name: "keeps_what_the_later_omits",
			have: Usage{Input: 412, Output: 3, CacheRead: 8},
			next: Usage{Output: 37},
			want: Usage{Input: 412, Output: 37, CacheRead: 8},
		},
		{
			name: "adds_a_new_breakdown",
			have: Usage{Input: 100, Output: 20},
			next: Usage{Output: 20, Reasoning: 12, CacheWrite: 5},
			want: Usage{Input: 100, Output: 20, Reasoning: 12, CacheWrite: 5},
		},
		{
			name: "later_wins_per_field",
			have: Usage{Output: 5},
			next: Usage{Output: 40},
			want: Usage{Output: 40},
		},
		{
			name: "zero_is_identity",
			have: Usage{Input: 7, Reasoning: 3},
			next: Usage{},
			want: Usage{Input: 7, Reasoning: 3},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.have
			got.Merge(tc.next)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestEventTypeString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		typ      EventType
		expected string
	}{
		{EventMessageStart, "message_start"},
		{EventThinkingStart, "thinking_start"},
		{EventThinkingDelta, "thinking_delta"},
		{EventThinkingEnd, "thinking_end"},
		{EventTextStart, "text_start"},
		{EventTextDelta, "text_delta"},
		{EventTextEnd, "text_end"},
		{EventToolCallStart, "tool_call_start"},
		{EventToolCallDelta, "tool_call_delta"},
		{EventToolCallEnd, "tool_call_end"},
		{EventUsage, "usage"},
		{EventDone, "done"},
		{EventType(200), "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.expected, func(t *testing.T) {
			assert.Equal(t, tc.expected, tc.typ.String())
		})
	}
}

func TestStopReasonString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		reason   StopReason
		expected string
	}{
		{StopEndTurn, "end_turn"},
		{StopToolUse, "tool_use"},
		{StopMaxTokens, "max_tokens"},
		{StopIncomplete, "incomplete"},
		{StopAborted, "aborted"},
		{StopError, "error"},
		{StopUnknown, "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.expected, func(t *testing.T) {
			assert.Equal(t, tc.expected, tc.reason.String())
		})
	}
}

func TestStopReasonMarshalRoundTrip(t *testing.T) {
	t.Parallel()

	reasons := []struct {
		name   string
		reason StopReason
	}{
		{"end_turn", StopEndTurn},
		{"tool_use", StopToolUse},
		{"max_tokens", StopMaxTokens},
		{"incomplete", StopIncomplete},
		{"aborted", StopAborted},
		{"error", StopError},
	}
	for _, tc := range reasons {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.reason)
			require.NoError(t, err)
			assert.Equal(t, `"`+tc.reason.String()+`"`, string(b))

			var back StopReason
			require.NoError(t, json.Unmarshal(b, &back))
			assert.Equal(t, tc.reason, back)
		})
	}
}

func TestParseStop(t *testing.T) {
	t.Parallel()

	got, ok := ParseStop("tool_use")
	require.True(t, ok)
	assert.Equal(t, StopToolUse, got)

	_, ok = ParseStop("TOOL_USE") // case insensitive
	require.True(t, ok)

	_, ok = ParseStop("bogus")
	assert.False(t, ok)
}

func TestUsageJSONTags(t *testing.T) {
	t.Parallel()

	u := Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4, Reasoning: 5}
	b, err := json.Marshal(u)
	require.NoError(t, err)
	assert.JSONEq(t, `{"input":1,"output":2,"cacheRead":3,"cacheWrite":4,"reasoning":5}`, string(b))
}
