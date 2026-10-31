package llm

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStopReasonFrom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		reason   string
		expected StopReason
	}{
		{"stop", "stop", StopEndTurn},
		{"end_turn_spelling", "end_turn", StopEndTurn},
		{"tool_calls", "tool_calls", StopToolUse},
		{"function_call", "function_call", StopToolUse},
		{"length", "length", StopMaxTokens},
		{"max_tokens_field", fieldMaxTokens, StopMaxTokens},
		{"content_filter_is_incomplete", "content_filter", StopIncomplete},
		{"mixed_case_stop", "STOP", StopEndTurn},
		{"mixed_case_tool_calls", "Tool_Calls", StopToolUse},
		{"mixed_case_length", "Length", StopMaxTokens},
		{"mixed_case_content_filter", "Content_Filter", StopIncomplete},
		{"empty_is_unknown", "", StopUnknown},
		{"unknown_defaults_end_turn", "some_future_reason", StopEndTurn},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, stopReasonFrom(tc.reason))
		})
	}
}

func TestCompatUsageToUsage(t *testing.T) {
	t.Parallel()

	decode := func(raw string) Usage {
		var u compatUsage
		require.NoError(t, json.Unmarshal([]byte(raw), &u))
		return u.toUsage()
	}

	t.Run("openai_cached_tokens", func(t *testing.T) {
		got := decode(`{"prompt_tokens":100,"completion_tokens":20,
			"prompt_tokens_details":{"cached_tokens":30}}`)
		assert.Equal(t, Usage{Input: 70, Output: 20, CacheRead: 30}, got)
	})

	t.Run("deepseek_prompt_cache_hit", func(t *testing.T) {
		got := decode(`{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":30}`)
		assert.Equal(t, Usage{Input: 70, Output: 20, CacheRead: 30}, got)
	})

	t.Run("present_zero_wins_over_fallback", func(t *testing.T) {
		got := decode(`{"prompt_tokens":100,"completion_tokens":20,
			"prompt_cache_hit_tokens":30,"prompt_tokens_details":{"cached_tokens":0}}`)
		assert.Equal(t, Usage{Input: 100, Output: 20}, got)
	})

	t.Run("cache_write_subtracted", func(t *testing.T) {
		got := decode(`{"prompt_tokens":100,"completion_tokens":20,
			"prompt_tokens_details":{"cached_tokens":30,"cache_write_tokens":10}}`)
		assert.Equal(t, Usage{Input: 60, Output: 20, CacheRead: 30, CacheWrite: 10}, got)
	})

	t.Run("reasoning_tokens", func(t *testing.T) {
		got := decode(`{"prompt_tokens":100,"completion_tokens":20,
			"completion_tokens_details":{"reasoning_tokens":12}}`)
		assert.Equal(t, Usage{Input: 100, Output: 20, Reasoning: 12}, got)
	})
}

func TestCompatErrObjCodeString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"string_code", `{"code":"context_length_exceeded"}`, "context_length_exceeded"},
		{"numeric_code", `{"code":502}`, "502"},
		{"float_code", `{"code":529.0}`, "529"},
		{"missing_code", `{}`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var e compatErrObj
			require.NoError(t, json.Unmarshal([]byte(tc.raw), &e))
			assert.Equal(t, tc.want, e.codeString())
		})
	}
}
