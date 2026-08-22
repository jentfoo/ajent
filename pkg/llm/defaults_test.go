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
	t.Run("thinking_format_selects_the_encoding", func(t *testing.T) {
		tests := []struct {
			format   string
			expected ThinkingFormat
		}{
			{"anthropic", ThinkingAnthropic},
			{"openai", ThinkingOpenAI},
			{"reasoning_effort", ThinkingOpenAI},
			{"openrouter", ThinkingOpenRouter},
			{"qwen-chat-template", ThinkingQwenChatTemplate},
			{"together", ThinkingTogether},
			{"deepseek", ThinkingDeepSeek},
			{"none", ThinkingNone},
		}
		for _, tc := range tests {
			t.Run(tc.format, func(t *testing.T) {
				got := resolveCaps(base, nil, &Compat{ThinkingFormat: ptr(tc.format)})
				assert.Equal(t, tc.expected, got.Thinking)
			})
		}
	})
	t.Run("unknown_thinking_format_leaves_the_encoding", func(t *testing.T) {
		got := resolveCaps(base, nil, &Compat{ThinkingFormat: ptr("invented")})
		assert.Equal(t, base.Thinking, got.Thinking)
	})
	t.Run("supports_reasoning_effort_is_a_gate_not_a_clobber", func(t *testing.T) {
		// the clobber bug: supportsReasoningEffort must not overwrite thinkingFormat
		got := resolveCaps(base, nil, &Compat{
			ThinkingFormat:          ptr("deepseek"),
			SupportsReasoningEffort: ptr(true),
		})
		assert.Equal(t, ThinkingDeepSeek, got.Thinking)
		assert.True(t, got.SupportsReasoningEffort)
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
	tagCtx := modelContext{
		provider: "lmstudio", dialect: DialectOpenAICompletions,
		baseURL: resolveBaseURL("http://192.168.1.100:1111/v1", flavorDefaults[FlavorLMStudio].baseURL),
	}

	t.Run("copies_declared_fields", func(t *testing.T) {
		got := resolveModel(tagCtx, base, nil, ModelConfig{
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
	t.Run("reasoning_true_keeps_the_dialect_default", func(t *testing.T) {
		got := resolveModel(tagCtx, base, nil,
			ModelConfig{ID: "m1", Reasoning: ptr(true)})
		assert.True(t, got.Caps.Reasoning)
	})
	t.Run("reasoning_false_disables_replay", func(t *testing.T) {
		got := resolveModel(modelContext{provider: "anthropic", dialect: DialectAnthropic},
			flavorDefaults[FlavorAnthropic].caps, nil,
			ModelConfig{ID: "m1", Reasoning: ptr(false)})
		assert.False(t, got.Caps.Reasoning)
		assert.False(t, got.Caps.ReasoningReplay)
	})
	t.Run("level_map_carried_onto_caps", func(t *testing.T) {
		got := resolveModel(tagCtx, base, nil, ModelConfig{
			ID: "m1", LevelMap: map[Level]*string{LevelMax: ptr("high"), LevelOff: nil},
		})
		require.NotNil(t, got.Caps.LevelMap[LevelMax])
		assert.Equal(t, "high", *got.Caps.LevelMap[LevelMax])
		assert.Contains(t, got.Caps.LevelMap, LevelOff)
		assert.Nil(t, got.Caps.LevelMap[LevelOff])
	})
	t.Run("thinking_budgets_overlay_the_ladder", func(t *testing.T) {
		reasoning := true
		got := resolveModel(tagCtx, base, nil, ModelConfig{
			ID: "m1", Reasoning: &reasoning,
			ThinkingBudgets: map[Level]int{LevelHigh: 5000},
		})
		// the configured rung wins and the rest of the ladder survives
		assert.Equal(t, 5000, got.Caps.Budgets[LevelHigh])
		assert.Contains(t, got.Caps.Budgets, LevelLow) // untouched rungs remain
	})
	t.Run("input_defaults_to_text", func(t *testing.T) {
		got := resolveModel(tagCtx, Capabilities{}, nil, ModelConfig{ID: "m1"})
		assert.Equal(t, []Modality{ModalityText}, got.Input)
	})
	t.Run("input_includes_image_when_supported", func(t *testing.T) {
		got := resolveModel(tagCtx, Capabilities{Images: true}, nil, ModelConfig{ID: "m1"})
		assert.Equal(t, []Modality{ModalityText, ModalityImage}, got.Input)
	})
	t.Run("declared_input_wins", func(t *testing.T) {
		got := resolveModel(tagCtx, Capabilities{Images: true}, nil,
			ModelConfig{ID: "m1", Input: []Modality{ModalityText}})
		assert.Equal(t, []Modality{ModalityText}, got.Input)
	})
	t.Run("per_model_headers_carried_onto_the_model", func(t *testing.T) {
		got := resolveModel(tagCtx, base, nil, ModelConfig{
			ID: "m1", Headers: map[string]string{"X-Org": "acme"},
		})
		assert.Equal(t, map[string]string{"X-Org": "acme"}, got.Headers)
	})
	t.Run("per_model_api_overrides_the_provider", func(t *testing.T) {
		got := resolveModel(tagCtx, base, nil, ModelConfig{ID: "m1", API: DialectAnthropic})
		assert.Equal(t, DialectAnthropic, got.Caps.Dialect)
	})
	t.Run("per_model_base_url_overrides_the_provider", func(t *testing.T) {
		got := resolveModel(tagCtx, base, nil, ModelConfig{ID: "m1", BaseURL: "http://other:9/v1"})
		assert.Equal(t, "http://other:9/v1", got.BaseURL)
	})
	t.Run("base_url_falls_back_to_the_provider", func(t *testing.T) {
		got := resolveModel(tagCtx, base, nil, ModelConfig{ID: "m1"})
		assert.Equal(t, "http://192.168.1.100:1111/v1", got.BaseURL)
	})
	t.Run("sampling_params_fold_into_the_body_escape_hatch", func(t *testing.T) {
		got := resolveModel(tagCtx, base, nil, ModelConfig{
			ID: "m1", SamplingParams: map[string]any{"temperature": 0.2, "seed": 7},
		})
		require.NotNil(t, got.Caps.ExtraBody)
		assert.JSONEq(t, `0.2`, string(got.Caps.ExtraBody["temperature"]))
		assert.JSONEq(t, `7`, string(got.Caps.ExtraBody["seed"]))
	})
}

func TestApplyModelDefaults(t *testing.T) {
	t.Parallel()

	base := flavorDefaults[FlavorLMStudio].caps
	ctx := modelContext{provider: "lmstudio", dialect: DialectOpenAICompletions}

	t.Run("id_only_entry_is_complete", func(t *testing.T) {
		got := resolveModel(ctx, base, nil, ModelConfig{ID: "llama3.1:8b"})
		assert.Equal(t, "llama3.1:8b", got.Name)
		assert.Equal(t, defaultContextWindow, got.ContextWindow)
		assert.Equal(t, defaultMaxTokens, got.MaxOutput)
		assert.Equal(t, []Modality{ModalityText}, got.Input)
	})
	t.Run("reasoning_defaults_off", func(t *testing.T) {
		// unset reasoning takes the flavor's opinion, which is false when it has none
		got := resolveModel(ctx, Capabilities{}, nil, ModelConfig{ID: "m1"})
		assert.False(t, got.Caps.Reasoning)
	})
	t.Run("configured_values_win", func(t *testing.T) {
		got := resolveModel(ctx, base, nil, ModelConfig{
			ID: "m1", Name: "Model One", ContextWindow: ptr(400000), MaxTokens: ptr(40000),
		})
		assert.Equal(t, "Model One", got.Name)
		assert.Equal(t, 400000, got.ContextWindow)
		assert.Equal(t, 40000, got.MaxOutput)
	})
	t.Run("discovered_window_beats_the_default", func(t *testing.T) {
		// defaults run after the merge, so a real loaded window is never replaced
		merged, _ := mergeModels(nil, []ModelConfig{{ID: "m1", ContextWindow: ptr(8192)}}, nil)
		got := resolveModel(ctx, base, nil, merged[0])
		assert.Equal(t, 8192, got.ContextWindow)
	})
	t.Run("defaulted_max_tokens_caps_the_ladder", func(t *testing.T) {
		got := resolveModel(ctx, base, nil, ModelConfig{ID: "m1", Reasoning: ptr(true)})
		assert.Equal(t, defaultMaxTokens-1, got.Caps.Budgets[LevelMax])
	})
}

// TestModelLayering pins the whole precedence chain in one case, each layer
// visibly winning over the one below it. Overrides participate only where the
// provider declares nothing, since a declared list is the whole list.
func TestModelLayering(t *testing.T) {
	t.Parallel()

	providerCompat := &Compat{SupportsToolChoice: ptr(true), SupportsStore: ptr(true)}
	discovered := []ModelConfig{{
		ID: "m1", Name: "Discovered", ContextWindow: ptr(1000), MaxTokens: ptr(100),
		Compat: &Compat{SupportsStore: ptr(false), SupportsImages: ptr(true)},
	}}
	overrides := map[string]ModelOverride{"m1": {
		Name: "Overridden", ContextWindow: ptr(2000),
		Compat: &Compat{SupportsImages: ptr(false), SupportsPromptCache: ptr(true)},
	}}

	merged, warnings := mergeModels(nil, discovered, overrides)
	require.Len(t, merged, 1)
	assert.Empty(t, warnings)
	got := resolveModel(modelContext{provider: "p", dialect: DialectOpenAICompletions},
		flavorDefaults[FlavorGeneric].caps, providerCompat, merged[0])

	assert.Equal(t, "Overridden", got.Name)  // override beats discovery
	assert.Equal(t, 2000, got.ContextWindow) // override beats discovery
	assert.Equal(t, 100, got.MaxOutput)      // discovery beats the schema default
	assert.True(t, got.Caps.ToolChoice)      // provider compat survives
	assert.False(t, got.Caps.Store)          // discovered compat beats provider compat
	assert.False(t, got.Caps.Images)         // override compat beats discovered compat
	assert.True(t, got.Caps.PromptCache)     // override adds what nothing below set
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
	t.Run("anthropic_defaults_eager_streaming_and_tool_cache", func(t *testing.T) {
		// pi defaults both on; an unset entry must behave the same
		caps := flavorDefaults[FlavorAnthropic].caps
		assert.True(t, caps.EagerToolInputStreaming)
		assert.True(t, caps.CacheControlOnTools)
	})
	t.Run("explicit_false_overrides_the_anthropic_defaults", func(t *testing.T) {
		caps := resolveCaps(flavorDefaults[FlavorAnthropic].caps, &Compat{
			EagerToolInputStreaming:     ptr(false),
			SupportsCacheControlOnTools: ptr(false),
		})
		assert.False(t, caps.EagerToolInputStreaming)
		assert.False(t, caps.CacheControlOnTools)
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
	t.Run("chat_completions_flavors_default_finish_reason_and_strict", func(t *testing.T) {
		// pi's chat-completions default emits strict:false on tools and treats a
		// stream that ends without finish_reason as truncated, so every flavor that
		// can reach the compat builder carries both gates.
		for _, f := range []Flavor{FlavorGeneric, FlavorOpenRouter, FlavorLMStudio, FlavorLlamaCpp} {
			assert.True(t, flavorDefaults[f].caps.SupportsFinishReason, f.String())
			assert.True(t, flavorDefaults[f].caps.SupportsStrict, f.String())
		}
	})
}
