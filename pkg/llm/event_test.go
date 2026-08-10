package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
