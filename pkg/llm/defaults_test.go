package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFlavorFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		key      string
		cfg      ProviderConfig
		expected Flavor
	}{
		{"key_names_a_known_flavor", "lmstudio", ProviderConfig{}, FlavorLMStudio},
		{"anthropic_key", "anthropic", ProviderConfig{}, FlavorAnthropic},
		{"llamacpp_key", "llamacpp", ProviderConfig{}, FlavorLlamaCpp},
		{"unknown_key_is_generic", "myproxy", ProviderConfig{}, FlavorGeneric},
		{"explicit_flavor_wins", "myproxy", ProviderConfig{Flavor: FlavorOpenRouter}, FlavorOpenRouter},
		{"explicit_beats_a_matching_key", "lmstudio", ProviderConfig{Flavor: FlavorGeneric}, FlavorGeneric},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, flavorFor(tc.key, tc.cfg))
		})
	}
}

func TestResolveCaps(t *testing.T) {
	t.Parallel()

	base := flavorDefaults[FlavorLMStudio].caps

	t.Run("defaults_pass_through_untouched", func(t *testing.T) {
		assert.Equal(t, base, resolveCaps(base, nil, nil))
	})
	t.Run("provider_layer_applies", func(t *testing.T) {
		got := resolveCaps(base, &Compat{SupportsToolChoice: ptr(false)}, nil)
		assert.False(t, got.ToolChoice)
		assert.True(t, got.Temperature) // untouched
	})
	t.Run("model_layer_beats_provider", func(t *testing.T) {
		got := resolveCaps(base,
			&Compat{MaxTokensField: ptr(fieldMaxCompletion)},
			&Compat{MaxTokensField: ptr(fieldMaxTokens)})
		assert.Equal(t, fieldMaxTokens, got.MaxTokensField)
	})
	t.Run("explicit_false_is_distinct_from_unset", func(t *testing.T) {
		unset := resolveCaps(base, &Compat{}, nil)
		assert.True(t, unset.Temperature)

		explicit := resolveCaps(base, &Compat{SupportsTemperature: ptr(false)}, nil)
		assert.False(t, explicit.Temperature)
	})
	t.Run("thinking_format_selects_the_style", func(t *testing.T) {
		tests := []struct {
			format   string
			expected ReasoningStyle
		}{
			{"anthropic", ReasoningAnthropicBudget},
			{"reasoning_effort", ReasoningOpenAIEffort},
			{"openrouter", ReasoningOpenRouter},
			{"qwen-chat-template", ReasoningInlineTags},
			{"together", ReasoningInlineTags},
			{"deepseek", ReasoningContentField},
			{"none", ReasoningNone},
		}
		for _, tc := range tests {
			t.Run(tc.format, func(t *testing.T) {
				got := resolveCaps(base, nil, &Compat{ThinkingFormat: ptr(tc.format)})
				assert.Equal(t, tc.expected, got.Reasoning)
			})
		}
	})
	t.Run("unknown_thinking_format_leaves_the_style", func(t *testing.T) {
		got := resolveCaps(base, nil, &Compat{ThinkingFormat: ptr("invented")})
		assert.Equal(t, base.Reasoning, got.Reasoning)
	})
	t.Run("supports_reasoning_effort_forces_effort_style", func(t *testing.T) {
		got := resolveCaps(base, nil, &Compat{SupportsReasoningEffort: ptr(true)})
		assert.Equal(t, ReasoningOpenAIEffort, got.Reasoning)
	})
	t.Run("custom_think_tags", func(t *testing.T) {
		got := resolveCaps(base, nil, &Compat{ThinkTags: []string{"<reasoning>", "</reasoning>"}})
		assert.Equal(t, "<reasoning>", got.ThinkOpen)
		assert.Equal(t, "</reasoning>", got.ThinkClose)
	})
	t.Run("malformed_think_tags_ignored", func(t *testing.T) {
		got := resolveCaps(base, nil, &Compat{ThinkTags: []string{"<only>"}})
		assert.Equal(t, thinkOpenTag, got.ThinkOpen)
	})
	t.Run("tokenizer_override", func(t *testing.T) {
		got := resolveCaps(base, nil, &Compat{Tokenizer: ptr("remote_tokenize")})
		assert.Equal(t, TokenizerRemoteTokenize, got.Tokenizer)
	})
	t.Run("long_cache_retention_flag", func(t *testing.T) {
		// an existing models.json carries this, so it must be recognized not warned about
		got := resolveCaps(base, nil, &Compat{SupportsLongCache: ptr(true)})
		assert.True(t, got.LongCache)
	})
	t.Run("reasoning_content_replay_flag", func(t *testing.T) {
		got := resolveCaps(base, nil, &Compat{RequiresReasoningContent: ptr(true)})
		assert.True(t, got.ReplayReasoning)
	})
}

func TestResolveModel(t *testing.T) {
	t.Parallel()

	base := flavorDefaults[FlavorLMStudio].caps

	t.Run("copies_declared_fields", func(t *testing.T) {
		got := resolveModel("lmstudio", base, nil, ModelConfig{
			ID: "m1", Name: "Model One", Aliases: []string{"m"},
			ContextWindow: ptr(1000), MaxTokens: ptr(200),
		})
		assert.Equal(t, "lmstudio", got.Provider)
		assert.Equal(t, "m1", got.ID)
		assert.Equal(t, "Model One", got.Name)
		assert.Equal(t, []string{"m"}, got.Aliases)
		assert.Equal(t, 1000, got.ContextWindow)
		assert.Equal(t, 200, got.MaxOutput)
	})
	t.Run("reasoning_true_keeps_the_dialect_style", func(t *testing.T) {
		got := resolveModel("lmstudio", base, nil,
			ModelConfig{ID: "m1", Reasoning: ptr(ReasoningUnset)})
		assert.Equal(t, ReasoningInlineTags, got.Caps.Reasoning)
	})
	t.Run("reasoning_false_disables_replay", func(t *testing.T) {
		got := resolveModel("anthropic", flavorDefaults[FlavorAnthropic].caps, nil,
			ModelConfig{ID: "m1", Reasoning: ptr(ReasoningNone)})
		assert.Equal(t, ReasoningNone, got.Caps.Reasoning)
		assert.False(t, got.Caps.ReasoningReplay)
	})
	t.Run("explicit_style_wins", func(t *testing.T) {
		got := resolveModel("lmstudio", base, nil,
			ModelConfig{ID: "m1", Reasoning: ptr(ReasoningContentField)})
		assert.Equal(t, ReasoningContentField, got.Caps.Reasoning)
	})
	t.Run("level_map_carried_onto_caps", func(t *testing.T) {
		got := resolveModel("lmstudio", base, nil, ModelConfig{
			ID: "m1", LevelMap: map[Level]*string{LevelMax: ptr("high"), LevelOff: nil},
		})
		require.NotNil(t, got.Caps.LevelMap[LevelMax])
		assert.Equal(t, "high", *got.Caps.LevelMap[LevelMax])
		assert.Contains(t, got.Caps.LevelMap, LevelOff)
		assert.Nil(t, got.Caps.LevelMap[LevelOff])
	})
	t.Run("input_defaults_to_text", func(t *testing.T) {
		got := resolveModel("lmstudio", Capabilities{}, nil, ModelConfig{ID: "m1"})
		assert.Equal(t, []Modality{ModalityText}, got.Input)
	})
	t.Run("input_includes_image_when_supported", func(t *testing.T) {
		got := resolveModel("lmstudio", Capabilities{Images: true}, nil, ModelConfig{ID: "m1"})
		assert.Equal(t, []Modality{ModalityText, ModalityImage}, got.Input)
	})
	t.Run("declared_input_wins", func(t *testing.T) {
		got := resolveModel("lmstudio", Capabilities{Images: true}, nil,
			ModelConfig{ID: "m1", Input: []Modality{ModalityText}})
		assert.Equal(t, []Modality{ModalityText}, got.Input)
	})
	t.Run("per_model_headers_carried_onto_the_model", func(t *testing.T) {
		got := resolveModel("lmstudio", base, nil, ModelConfig{
			ID: "m1", Headers: map[string]string{"X-Org": "acme"},
		})
		assert.Equal(t, map[string]string{"X-Org": "acme"}, got.Headers)
	})
	t.Run("sampling_params_fold_into_the_body_escape_hatch", func(t *testing.T) {
		got := resolveModel("lmstudio", base, nil, ModelConfig{
			ID: "m1", SamplingParams: map[string]any{"temperature": 0.2, "seed": 7},
		})
		require.NotNil(t, got.Caps.ExtraBody)
		assert.JSONEq(t, `0.2`, string(got.Caps.ExtraBody["temperature"]))
		assert.JSONEq(t, `7`, string(got.Caps.ExtraBody["seed"]))
	})
}

func TestFlavorDefaults(t *testing.T) {
	t.Parallel()

	t.Run("no_flavor_ships_models", func(t *testing.T) {
		// a compiled in model list would go stale in a way that silently
		// corrupts the context bar, so there must never be one
		for f, d := range flavorDefaults {
			assert.NotEmpty(t, d.dialect.String(), f.String())
		}
	})
	t.Run("local_flavors_disable_the_idle_timeout", func(t *testing.T) {
		for _, f := range []Flavor{FlavorLMStudio, FlavorLlamaCpp} {
			d := flavorDefaults[f]
			require.NotNil(t, d.timeouts.Idle, f.String())
			assert.Zero(t, durOr(d.timeouts.Idle, defaultIdleTimeout), f.String())
		}
	})
	t.Run("lmstudio_disables_the_header_timeout", func(t *testing.T) {
		// a just in time model load holds the headers for minutes
		d := flavorDefaults[FlavorLMStudio]
		require.NotNil(t, d.timeouts.Header)
		assert.Zero(t, durOr(d.timeouts.Header, defaultHeaderTimeout))
	})
	t.Run("anthropic_requires_reasoning_replay", func(t *testing.T) {
		assert.True(t, flavorDefaults[FlavorAnthropic].caps.ReasoningReplay)
		assert.True(t, flavorDefaults[FlavorOpenAI].caps.ReasoningReplay)
	})
	t.Run("llamacpp_starts_without_stream_usage", func(t *testing.T) {
		assert.False(t, flavorDefaults[FlavorLlamaCpp].caps.StreamUsage)
	})
	t.Run("hosted_flavors_carry_a_key_variable", func(t *testing.T) {
		for _, f := range []Flavor{FlavorAnthropic, FlavorOpenAI, FlavorOpenRouter} {
			assert.NotEmpty(t, flavorDefaults[f].apiKeyEnv, f.String())
		}
	})
	t.Run("local_flavors_need_no_key", func(t *testing.T) {
		for _, f := range []Flavor{FlavorLMStudio, FlavorLlamaCpp} {
			assert.Empty(t, flavorDefaults[f].apiKeyEnv, f.String())
		}
	})
}
