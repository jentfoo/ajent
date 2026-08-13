package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDetectCompat covers one case per vendor family, asserted both by provider
// name and base-URL spelling, plus the guards that keep detection sparse.
func TestDetectCompat(t *testing.T) {
	t.Parallel()

	type want struct {
		thinking        string // expected thinkingFormat value when set
		maxTokensField  string // "" means leave it to the flavor default
		supportsStore   bool
		reasoningEffort bool
		replayReasoning bool
	}
	tests := []struct {
		name     string
		provider string
		baseURL  string
		want     want
	}{
		{"deepseek", "deepseek", "", want{thinking: "deepseek",
			maxTokensField: fieldMaxTokens, reasoningEffort: true, replayReasoning: true}},
		{"zai", "zai", "", want{thinking: "zai", maxTokensField: fieldMaxTokens,
			reasoningEffort: false}},
		{"together", "together", "", want{maxTokensField: fieldMaxTokens,
			reasoningEffort: false}},
		{"ant_ling", "ant-ling", "", want{reasoningEffort: false}},
		{"openrouter", "openrouter", "", want{thinking: "openrouter", maxTokensField: fieldMaxCompletion,
			supportsStore: true}},
		{"moonshot", "moonshotai", "", want{maxTokensField: fieldMaxTokens,
			supportsStore: false, reasoningEffort: false}},
		{"nvidia", "nvidia", "", want{maxTokensField: fieldMaxTokens,
			reasoningEffort: false}},
		{"cerebras", "cerebras", "", want{supportsStore: false, reasoningEffort: true}},
		{"grok", "xai", "", want{supportsStore: false, reasoningEffort: false}},
		{"chutes_base_url", "custom", "https://api.chutes.ai/v1",
			want{maxTokensField: fieldMaxTokens, supportsStore: false}},
		{"opencode", "opencode", "", want{supportsStore: false, reasoningEffort: true}},
	}
	t.Run("opencode_sets_reasoning_content_field", func(t *testing.T) {
		got := detectCompat("opencode", "https://api.opencode.ai/v1", "")
		require.NotNil(t, got.ReasoningContentField)
		assert.Equal(t, fieldReasoningConten, *got.ReasoningContentField)

		// and the base-URL spelling agrees with the provider name
		byBase := detectCompat("custom", "https://opencode.ai", "")
		require.NotNil(t, byBase.ReasoningContentField)
		assert.Equal(t, fieldReasoningConten, *byBase.ReasoningContentField)
	})
	t.Run("cache_format_keys_on_literal_provider_not_base_url", func(t *testing.T) {
		// an openrouter base URL alone must not imply the anthropic cache format
		got := detectCompat("custom", "https://openrouter.ai/api/v1", "anthropic/claude-opus-4")
		assert.Nil(t, got.CacheControlFormat)

		named := detectCompat("openrouter", "", "anthropic/claude-opus-4")
		require.NotNil(t, named.CacheControlFormat)
		assert.Equal(t, "anthropic", *named.CacheControlFormat)
	})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := detectCompat(tc.provider, tc.baseURL, "")
			if tc.want.thinking != "" && got.ThinkingFormat != nil {
				assert.Equal(t, tc.want.thinking, *got.ThinkingFormat)
			}
			if tc.want.maxTokensField != "" && got.MaxTokensField != nil {
				assert.Equal(t, tc.want.maxTokensField, *got.MaxTokensField)
			}
			if got.SupportsStore != nil {
				assert.Equal(t, tc.want.supportsStore, *got.SupportsStore)
			}
			if got.RequiresReasoningContent != nil && tc.name == "deepseek" {
				assert.True(t, *got.RequiresReasoningContent)
			} else if got.RequiresReasoningContent != nil {
				assert.False(t, *got.RequiresReasoningContent)
			}
		})
	}

	t.Run("unmatched_provider_is_sparse", func(t *testing.T) {
		var zero Compat
		assert.Equal(t, zero, detectCompat("lmstudio", "http://localhost:1234/v1", ""))
		assert.Equal(t, zero, detectCompat("myproxy", "", ""))
	})
	t.Run("base_url_alone_detects_deepseek", func(t *testing.T) {
		got := detectCompat("custom", "https://api.deepseek.com", "")
		if got.ThinkingFormat != nil {
			assert.Equal(t, "deepseek", *got.ThinkingFormat)
		}
	})
	t.Run("openrouter_anthropic_prefix_sets_cache_format", func(t *testing.T) {
		got := detectCompat("openrouter", "", "anthropic/claude-opus-4")
		if got.CacheControlFormat != nil {
			assert.Equal(t, "anthropic", *got.CacheControlFormat)
		}
	})
	t.Run("detection_never_enables_reasoning", func(t *testing.T) {
		// a deepseek provider still needs "reasoning": true to actually reason
		base := flavorDefaults[FlavorGeneric].caps
		detected := detectCompat("deepseek", "https://api.deepseek.com", "")
		assert.False(t, applyCompat(base, &detected).Reasoning)
	})
}
