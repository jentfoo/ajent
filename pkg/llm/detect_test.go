package llm

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsOpenRouterEndpoint(t *testing.T) {
	t.Parallel()

	assert.True(t, isOpenRouterEndpoint("openrouter", ""))
	assert.True(t, isOpenRouterEndpoint("OpenRouter", ""))
	assert.True(t, isOpenRouterEndpoint("custom", "https://OpenRouter.AI/api/v1"))
	assert.False(t, isOpenRouterEndpoint("custom", "https://example.com/v1"))
}

func TestDetectCompat(t *testing.T) {
	t.Parallel()

	type want struct {
		thinking       string // expected ThinkingFormat, "" means don't care
		maxTokensField string // expected MaxTokensField, "" means don't care
		supportsStore  bool   // every matched provider gets a non-nil SupportsStore
	}
	tests := []struct {
		name     string
		provider string
		baseURL  string
		want     want
	}{
		{"deepseek", "deepseek", "", want{thinking: "deepseek",
			maxTokensField: fieldMaxTokens}},
		{"zai", "zai", "", want{thinking: "zai", maxTokensField: fieldMaxTokens}},
		{"together", "together", "", want{thinking: "together",
			maxTokensField: fieldMaxTokens}},
		{"ant_ling", "ant-ling", "", want{maxTokensField: fieldMaxTokens}},
		{"openrouter", "openrouter", "", want{thinking: "openrouter", maxTokensField: fieldMaxCompletion,
			supportsStore: true}},
		{"openai", "openai", "", want{thinking: "openai", maxTokensField: fieldMaxCompletion,
			supportsStore: true}},
		{"moonshot", "moonshotai", "", want{maxTokensField: fieldMaxTokens}},
		{"nvidia", "nvidia", "", want{maxTokensField: fieldMaxTokens}},
		{"cerebras", "cerebras", "", want{}},
		{"grok", "xai", "", want{}},
		{"chutes_base_url", "custom", "https://api.chutes.ai/v1",
			want{maxTokensField: fieldMaxTokens}},
		{"opencode", "opencode", "", want{}},
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

			// nils here would be a regression
			require.NotNil(t, got.ThinkingFormat)
			if tc.want.thinking != "" {
				assert.Equal(t, tc.want.thinking, *got.ThinkingFormat)
			}
			require.NotNil(t, got.MaxTokensField)
			if tc.want.maxTokensField != "" {
				assert.Equal(t, tc.want.maxTokensField, *got.MaxTokensField)
			}
			require.NotNil(t, got.SupportsStore)
			assert.Equal(t, tc.want.supportsStore, *got.SupportsStore)

			// only deepseek maps reasoning onto the dedicated content field
			require.NotNil(t, got.RequiresReasoningContent)
			assert.Equal(t, tc.name == "deepseek", *got.RequiresReasoningContent)
		})
	}

	t.Run("mixed_case_matches_lowercase_spelling", func(t *testing.T) {
		for _, tc := range []struct{ name, provider, baseURL string }{
			{"deepseek_url", "custom", "https://Api.DeepSeek.Com/v1"},
			{"together_url", "custom", "https://API.Together.XYZ/v1"},
			{"openrouter_url", "custom", "https://OpenRouter.AI/api/v1"},
			{"chutes_url", "custom", "https://Chutes.Ai/v1"},
			{"grok_name", "XAI", ""},
			{"deepseek_name", "DeepSeek", ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := detectCompat(tc.provider, tc.baseURL, "")
				lower := detectCompat(strings.ToLower(tc.provider), strings.ToLower(tc.baseURL), "")
				assert.Equal(t, lower, got)
				assert.NotEqual(t, Compat{}, got)
			})
		}
	})

	t.Run("unmatched_provider_is_sparse", func(t *testing.T) {
		var zero Compat
		assert.Equal(t, zero, detectCompat("lmstudio", "http://localhost:1234/v1", ""))
		assert.Equal(t, zero, detectCompat("myproxy", "", ""))
	})

	t.Run("base_url_alone_detects_deepseek", func(t *testing.T) {
		got := detectCompat("custom", "https://api.deepseek.com", "")
		require.NotNil(t, got.ThinkingFormat)
		assert.Equal(t, "deepseek", *got.ThinkingFormat)
	})

	t.Run("openrouter_anthropic_prefix_sets_cache_format", func(t *testing.T) {
		got := detectCompat("openrouter", "", "anthropic/claude-opus-4")
		require.NotNil(t, got.CacheControlFormat)
		assert.Equal(t, "anthropic", *got.CacheControlFormat)
	})

	t.Run("detection_never_enables_reasoning", func(t *testing.T) {
		// a deepseek provider still needs "reasoning": true to actually reason
		base := flavorDefaults[FlavorGeneric].caps
		detected := detectCompat("deepseek", "https://api.deepseek.com", "")
		assert.False(t, applyCompat(base, &detected).Reasoning)
	})

	t.Run("stream_usage_left_to_the_flavor_baseline", func(t *testing.T) {
		// usage is baseline-on everywhere, so detection has nothing to say about it
		got := detectCompat("deepseek", "https://api.deepseek.com", "")
		assert.Nil(t, got.SupportsStreamUsage)
		assert.Nil(t, got.SupportsUsageInStreaming)
	})
}
