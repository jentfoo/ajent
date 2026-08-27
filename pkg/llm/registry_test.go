package llm

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ptr[T any](v T) *T { return &v }

// testFile is two providers with declared models, for resolution tests.
func testFile() File {
	return File{
		Providers: map[string]ProviderConfig{
			"anthropic": {
				APIKeyEnv: "ANTHROPIC_API_KEY",
				Models: []ModelConfig{
					{ID: "claude-opus-4-5", Name: "Claude Opus 4.5", Aliases: []string{"opus"},
						ContextWindow: ptr(200000), MaxTokens: ptr(64000)},
					{ID: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5", Aliases: []string{"sonnet"},
						ContextWindow: ptr(200000), MaxTokens: ptr(64000)},
				},
			},
			"lmstudio": {
				Models: []ModelConfig{
					{ID: "qwen3.6-27b-mtp", Name: "Qwen 3.6 27B", ContextWindow: ptr(200000)},
				},
			},
		},
	}
}

func TestNewRegistry(t *testing.T) {
	t.Parallel()

	t.Run("unknown_flavor_names_the_one_that_won", func(t *testing.T) {
		// the provider key still names a known flavor, so "generic" would be a lie
		f := File{Providers: map[string]ProviderConfig{
			"lmstudio": {Flavor: FlavorUnknown, Models: []ModelConfig{{ID: "m1"}}},
		}}
		r, warnings := NewRegistry(f, nil, RegistryOptions{})
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "unknown flavor ignored, using lmstudio")

		m, err := r.Resolve("lmstudio/m1")
		require.NoError(t, err)
		assert.Equal(t, flavorDefaults[FlavorLMStudio].baseURL, m.BaseURL)
	})
	t.Run("unknown_flavor_falls_back_to_generic", func(t *testing.T) {
		f := File{Providers: map[string]ProviderConfig{
			"proxy": {Flavor: FlavorUnknown, BaseURL: "http://x/v1", Models: []ModelConfig{{ID: "m1"}}},
		}}
		_, warnings := NewRegistry(f, nil, RegistryOptions{})
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "unknown flavor ignored, using generic")
	})
	t.Run("sorts_by_provider_then_id", func(t *testing.T) {
		r, warnings := NewRegistry(testFile(), nil, RegistryOptions{})
		assert.Empty(t, warnings)

		models := r.Models()
		keys := make([]string, len(models))
		for i, m := range models {
			keys[i] = m.Key()
		}
		assert.Equal(t, []string{
			"anthropic/claude-opus-4-5",
			"anthropic/claude-sonnet-4-5",
			"lmstudio/qwen3.6-27b-mtp",
		}, keys)
	})
	t.Run("first_model_is_active_without_a_default", func(t *testing.T) {
		r, _ := NewRegistry(testFile(), nil, RegistryOptions{})
		assert.Equal(t, "anthropic/claude-opus-4-5", r.Active().Key())
	})
	t.Run("default_model_is_honoured", func(t *testing.T) {
		f := testFile()
		f.DefaultModel = "sonnet"
		r, warnings := NewRegistry(f, nil, RegistryOptions{})
		assert.Empty(t, warnings)
		assert.Equal(t, "anthropic/claude-sonnet-4-5", r.Active().Key())
	})
	t.Run("unknown_default_model_warns", func(t *testing.T) {
		f := testFile()
		f.DefaultModel = "nonexistent"
		_, warnings := NewRegistry(f, nil, RegistryOptions{})
		require.NotEmpty(t, warnings)
		assert.Contains(t, warnings[0], "nonexistent")
	})
	t.Run("disabled_provider_is_skipped", func(t *testing.T) {
		f := testFile()
		e := f.Providers["lmstudio"]
		e.Disabled = true
		f.Providers["lmstudio"] = e

		r, _ := NewRegistry(f, nil, RegistryOptions{})
		assert.Len(t, r.Models(), 2)
		assert.Equal(t, []string{"anthropic"}, r.ProviderNames())
	})
	t.Run("provider_with_no_models_warns", func(t *testing.T) {
		f := File{Providers: map[string]ProviderConfig{"openrouter": {}}}
		r, warnings := NewRegistry(f, nil, RegistryOptions{})
		assert.Empty(t, r.Models())
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "openrouter")
	})
	t.Run("flavor_defaults_from_the_provider_key", func(t *testing.T) {
		r, _ := NewRegistry(testFile(), nil, RegistryOptions{})
		m, err := r.Resolve("opus")
		require.NoError(t, err)

		_, flavor, ok := r.ProviderConfigFor(m)
		require.True(t, ok)
		assert.Equal(t, FlavorAnthropic, flavor)
		assert.True(t, m.Caps.Reasoning)
		assert.Equal(t, ThinkingAnthropic, m.Caps.Thinking)
		assert.True(t, m.Caps.ReasoningReplay)
	})
	t.Run("unknown_provider_key_is_generic", func(t *testing.T) {
		f := File{Providers: map[string]ProviderConfig{
			"myproxy": {Models: []ModelConfig{{ID: "m1"}}},
		}}
		r, _ := NewRegistry(f, nil, RegistryOptions{})
		m, err := r.Resolve("m1")
		require.NoError(t, err)

		_, flavor, _ := r.ProviderConfigFor(m)
		assert.Equal(t, FlavorGeneric, flavor)
	})
	t.Run("explicit_flavor_overrides_the_key", func(t *testing.T) {
		f := File{Providers: map[string]ProviderConfig{
			"myproxy": {Flavor: FlavorLMStudio, Models: []ModelConfig{{ID: "m1"}}},
		}}
		r, _ := NewRegistry(f, nil, RegistryOptions{})
		m, _ := r.Resolve("m1")

		_, flavor, _ := r.ProviderConfigFor(m)
		assert.Equal(t, FlavorLMStudio, flavor)
		assert.Equal(t, ThinkingThinkTags, m.Caps.Thinking)
	})
}

func TestRegistryResolve(t *testing.T) {
	t.Parallel()

	r, _ := NewRegistry(testFile(), nil, RegistryOptions{})

	tests := []struct {
		name     string
		query    string
		expected string
	}{
		{"by_alias", "opus", "anthropic/claude-opus-4-5"},
		{"alias_case_insensitive", "OPUS", "anthropic/claude-opus-4-5"},
		{"by_provider_and_id", "anthropic/claude-sonnet-4-5", "anthropic/claude-sonnet-4-5"},
		{"by_bare_id", "claude-sonnet-4-5", "anthropic/claude-sonnet-4-5"},
		{"by_unique_suffix", "sonnet-4-5", "anthropic/claude-sonnet-4-5"},
		{"by_key_substring", "lmstudio/qwen", "lmstudio/qwen3.6-27b-mtp"},
		{"by_name_substring", "Qwen 3.6", "lmstudio/qwen3.6-27b-mtp"},
		{"trims_whitespace", "  opus  ", "anthropic/claude-opus-4-5"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.Resolve(tc.query)
			require.NoError(t, err)
			assert.Equal(t, tc.expected, got.Key())
		})
	}

	t.Run("unknown_name", func(t *testing.T) {
		_, err := r.Resolve("gpt-5")
		assert.ErrorIs(t, err, ErrUnknownModel)
	})
	t.Run("empty_name", func(t *testing.T) {
		_, err := r.Resolve("")
		assert.ErrorIs(t, err, ErrUnknownModel)
	})
	t.Run("ambiguous_names_the_candidates", func(t *testing.T) {
		_, err := r.Resolve("claude")

		var ae *ErrAmbiguousModel
		require.ErrorAs(t, err, &ae)
		assert.ElementsMatch(t,
			[]string{"anthropic/claude-opus-4-5", "anthropic/claude-sonnet-4-5"}, ae.Candidates)
	})
	t.Run("alias_beats_a_substring_match", func(t *testing.T) {
		got, err := r.Resolve("opus")
		require.NoError(t, err)
		assert.Equal(t, "anthropic/claude-opus-4-5", got.Key())
	})
}

func TestMergeModels(t *testing.T) {
	t.Parallel()

	declared := []ModelConfig{{ID: "a", ContextWindow: ptr(1000)}, {ID: "b"}}
	discovered := []ModelConfig{
		{ID: "a", Name: "Discovered A", ContextWindow: ptr(9999)},
		{ID: "b", Name: "Discovered B", ContextWindow: ptr(2000)},
		{ID: "c", Name: "Discovered C"},
	}

	t.Run("declared_list_is_the_whole_list", func(t *testing.T) {
		got, _ := mergeModels(declared, discovered, nil)
		require.Len(t, got, 2) // "c" is not added
		assert.Equal(t, "a", got[0].ID)
		assert.Equal(t, "b", got[1].ID)
	})
	t.Run("declared_fields_win", func(t *testing.T) {
		got, _ := mergeModels(declared, discovered, nil)
		require.NotNil(t, got[0].ContextWindow)
		assert.Equal(t, 1000, *got[0].ContextWindow)
	})
	t.Run("discovery_fills_unset_fields", func(t *testing.T) {
		got, _ := mergeModels(declared, discovered, nil)
		assert.Equal(t, "Discovered A", got[0].Name)
		require.NotNil(t, got[1].ContextWindow)
		assert.Equal(t, 2000, *got[1].ContextWindow) // the real loaded window
	})
	t.Run("thinking_budgets_fill_from_discovery", func(t *testing.T) {
		d := []ModelConfig{{ID: "b"}}
		disc := []ModelConfig{{ID: "b", ThinkingBudgets: map[Level]int{LevelHigh: 7777}}}
		got, _ := mergeModels(d, disc, nil)
		assert.Equal(t, 7777, got[0].ThinkingBudgets[LevelHigh])
	})
	t.Run("discovery_supplies_everything_when_nothing_declared", func(t *testing.T) {
		got, _ := mergeModels(nil, discovered, nil)
		assert.Len(t, got, 3)
	})
	t.Run("declared_only", func(t *testing.T) {
		got, _ := mergeModels(declared, nil, nil)
		assert.Len(t, got, 2)
	})
	t.Run("both_empty", func(t *testing.T) {
		got, _ := mergeModels(nil, nil, nil)
		assert.Empty(t, got)
	})
	t.Run("override_adjusts_a_discovered_entry", func(t *testing.T) {
		got, w := mergeModels(nil, discovered,
			map[string]ModelOverride{"c": {Name: "Renamed C", ContextWindow: ptr(4096)}})
		require.Len(t, got, 3)
		assert.Empty(t, w)
		assert.Equal(t, "Renamed C", got[2].Name)
		require.NotNil(t, got[2].ContextWindow)
		assert.Equal(t, 4096, *got[2].ContextWindow)
	})
	t.Run("override_never_adds_a_model", func(t *testing.T) {
		got, w := mergeModels(nil, discovered, map[string]ModelOverride{"nope": {Name: "X"}})
		assert.Len(t, got, 3)
		assert.Empty(t, w) // an id discovery has not returned yet is not a mistake
	})
	t.Run("override_on_an_excluded_id_warns", func(t *testing.T) {
		// "c" is discovered but the declared list is the whole list, so the
		// override is inert; silence here would look like it applied
		_, w := mergeModels(declared, discovered, map[string]ModelOverride{"c": {Name: "X"}})
		require.Len(t, w, 1)
		assert.Contains(t, w[0], `modelOverrides "c" is not in models`)
	})
	t.Run("override_on_a_declared_id_warns", func(t *testing.T) {
		got, w := mergeModels(declared, discovered, map[string]ModelOverride{"a": {Name: "X"}})
		require.Len(t, w, 1)
		assert.Contains(t, w[0], `modelOverrides "a"`)
		assert.Equal(t, "Discovered A", got[0].Name) // the declaration still wins
	})
	t.Run("override_does_not_mutate_the_cache", func(t *testing.T) {
		disc := []ModelConfig{{ID: "a", Name: "Discovered A"}}
		_, _ = mergeModels(nil, disc, map[string]ModelOverride{"a": {Name: "Renamed"}})
		assert.Equal(t, "Discovered A", disc[0].Name)
	})
}

func TestApplyOverride(t *testing.T) {
	t.Parallel()

	base := ModelConfig{
		ID: "m1", Name: "Base", ContextWindow: ptr(1000), MaxTokens: ptr(100),
		Reasoning:      ptr(false),
		Input:          []Modality{ModalityText},
		Headers:        map[string]string{"X-Base": "1", "X-Both": "base"},
		SamplingParams: map[string]any{"temperature": 0.2, "top_p": 0.9},
		Compat:         &Compat{SupportsStore: ptr(true), SupportsImages: ptr(true)},
	}

	t.Run("scalars_replace", func(t *testing.T) {
		got := applyOverride(base, ModelOverride{
			Name: "Over", ContextWindow: ptr(2000), MaxTokens: ptr(200), Reasoning: ptr(true),
			Input: []Modality{ModalityText, ModalityImage},
		})
		assert.Equal(t, "Over", got.Name)
		assert.Equal(t, 2000, *got.ContextWindow)
		assert.Equal(t, 200, *got.MaxTokens)
		assert.True(t, *got.Reasoning)
		assert.Equal(t, []Modality{ModalityText, ModalityImage}, got.Input)
	})
	t.Run("unset_fields_leave_the_base", func(t *testing.T) {
		got := applyOverride(base, ModelOverride{Name: "Over"})
		assert.Equal(t, 1000, *got.ContextWindow)
		assert.False(t, *got.Reasoning)
	})
	t.Run("headers_merge_per_key", func(t *testing.T) {
		got := applyOverride(base, ModelOverride{
			Headers: map[string]string{"X-Both": "over", "X-New": "2"},
		})
		assert.Equal(t, map[string]string{"X-Base": "1", "X-Both": "over", "X-New": "2"}, got.Headers)
	})
	t.Run("sampling_params_merge_per_key", func(t *testing.T) {
		got := applyOverride(base, ModelOverride{SamplingParams: map[string]any{"temperature": 1.0}})
		assert.InDelta(t, 1.0, got.SamplingParams["temperature"], 1e-9)
		assert.InDelta(t, 0.9, got.SamplingParams["top_p"], 1e-9)
	})
	t.Run("compat_merges_per_field", func(t *testing.T) {
		got := applyOverride(base, ModelOverride{
			Compat: &Compat{SupportsImages: ptr(false), SupportsPromptCache: ptr(true)},
		})
		require.NotNil(t, got.Compat)
		assert.True(t, *got.Compat.SupportsStore)       // untouched
		assert.False(t, *got.Compat.SupportsImages)     // replaced
		assert.True(t, *got.Compat.SupportsPromptCache) // added
	})
	t.Run("level_map_replaces", func(t *testing.T) {
		got := applyOverride(base, ModelOverride{LevelMap: map[Level]*string{LevelOff: nil}})
		assert.Contains(t, got.LevelMap, LevelOff)
	})
	t.Run("does_not_mutate_the_base_maps", func(t *testing.T) {
		_ = applyOverride(base, ModelOverride{
			Headers:        map[string]string{"X-Both": "over"},
			SamplingParams: map[string]any{"temperature": 1.0},
			Compat:         &Compat{SupportsStore: ptr(false)},
		})
		assert.Equal(t, "base", base.Headers["X-Both"])
		assert.InDelta(t, 0.2, base.SamplingParams["temperature"], 1e-9)
		assert.True(t, *base.Compat.SupportsStore)
	})
}

func TestRegistrySetActive(t *testing.T) {
	t.Parallel()

	r, _ := NewRegistry(testFile(), nil, RegistryOptions{})
	m, err := r.Resolve("sonnet")
	require.NoError(t, err)

	r.SetActive(m)
	assert.Equal(t, "anthropic/claude-sonnet-4-5", r.Active().Key())
}

func TestRegistryWithCache(t *testing.T) {
	t.Parallel()

	t.Run("discovery_fills_an_undeclared_provider", func(t *testing.T) {
		f := File{Providers: map[string]ProviderConfig{"openrouter": {}}}
		cache := map[string]CacheEntry{"openrouter": {Models: []ModelConfig{
			{ID: "z-ai/glm-5.2", Name: "GLM 5.2", ContextWindow: ptr(800000)},
		}}}

		r, warnings := NewRegistry(f, cache, RegistryOptions{})
		assert.Empty(t, warnings)
		require.Len(t, r.Models(), 1)
		assert.Equal(t, "openrouter/z-ai/glm-5.2", r.Models()[0].Key())
	})
	t.Run("declared_provider_ignores_extra_discovered_models", func(t *testing.T) {
		f := File{Providers: map[string]ProviderConfig{
			"openrouter": {Models: []ModelConfig{{ID: "z-ai/glm-5.2"}}},
		}}
		cache := map[string]CacheEntry{"openrouter": {Models: []ModelConfig{
			{ID: "z-ai/glm-5.2", ContextWindow: ptr(800000)},
			{ID: "some/other-model"},
		}}}

		r, _ := NewRegistry(f, cache, RegistryOptions{})
		require.Len(t, r.Models(), 1)
		assert.Equal(t, 800000, r.Models()[0].ContextWindow) // still enriched
	})
}

func TestRegistryRefresh(t *testing.T) {
	t.Parallel()

	t.Run("adds_discovered_models", func(t *testing.T) {
		var hits int
		srv := discoveryServer(t, "/models", "openrouter/models.json", &hits)
		f := File{Providers: map[string]ProviderConfig{"openrouter": {BaseURL: srv.URL}}}

		r, _ := NewRegistry(f, nil, RegistryOptions{Env: func(string) string { return "" }})
		assert.Empty(t, r.Models())

		cache, warnings := r.Refresh(t.Context(), DiscoverOptions{
			Env: func(string) string { return "" },
			Now: func() time.Time { return testNow },
		})
		assert.Empty(t, warnings)
		assert.Len(t, r.Models(), 2)
		assert.Len(t, cache["openrouter"].Models, 2)
	})
	t.Run("keeps_the_active_model", func(t *testing.T) {
		var hits int
		srv := discoveryServer(t, "/models", "openrouter/models.json", &hits)
		f := File{Providers: map[string]ProviderConfig{
			"openrouter": {BaseURL: srv.URL, Models: []ModelConfig{{ID: "z-ai/glm-5.2"}}},
		}}

		r, _ := NewRegistry(f, nil, RegistryOptions{Env: func(string) string { return "" }})
		before := r.Active().Key()
		require.NotEmpty(t, before)

		_, _ = r.Refresh(t.Context(), DiscoverOptions{
			Env: func(string) string { return "" },
			Now: func() time.Time { return testNow },
		})
		assert.Equal(t, before, r.Active().Key())
	})
	t.Run("declared_models_survive_a_refresh", func(t *testing.T) {
		var hits int
		srv := discoveryServer(t, "/models", "openrouter/models.json", &hits)
		f := File{Providers: map[string]ProviderConfig{
			"openrouter": {BaseURL: srv.URL, Models: []ModelConfig{{ID: "z-ai/glm-5.2"}}},
		}}

		r, _ := NewRegistry(f, nil, RegistryOptions{Env: func(string) string { return "" }})
		_, _ = r.Refresh(t.Context(), DiscoverOptions{
			Env: func(string) string { return "" },
			Now: func() time.Time { return testNow },
		})

		// the declared list stays the whole list, but gains what discovery knows
		require.Len(t, r.Models(), 1)
		assert.Equal(t, 800000, r.Models()[0].ContextWindow)
	})
}

func TestRegistrySetCompactDefault(t *testing.T) {
	t.Parallel()

	build := func(t *testing.T) *Registry {
		t.Helper()
		f := File{Providers: map[string]ProviderConfig{
			"anthropic": {Flavor: FlavorAnthropic, Models: []ModelConfig{
				{ID: "declared", ContextWindow: ptr(200000), CompactThreshold: ptr(0.5)},
				{ID: "bare", ContextWindow: ptr(200000)},
			}},
		}}
		r, _ := NewRegistry(f, nil, RegistryOptions{})
		return r
	}

	thresholdOf := func(t *testing.T, r *Registry, id string) float64 {
		t.Helper()
		m, err := r.Resolve("anthropic/" + id)
		require.NoError(t, err)
		return m.CompactThreshold
	}

	t.Run("applies_to_undeclared_models", func(t *testing.T) {
		r := build(t)
		r.SetCompactDefault(0.7)
		assert.InDelta(t, 0.7, thresholdOf(t, r, "bare"), 1e-09)
	})

	t.Run("never_overrides_declared", func(t *testing.T) {
		r := build(t)
		r.SetCompactDefault(0.7)
		assert.InDelta(t, 0.5, thresholdOf(t, r, "declared"), 1e-09)
	})

	t.Run("reapply_replaces_previous_default", func(t *testing.T) {
		r := build(t)
		r.SetCompactDefault(0.7)
		r.SetCompactDefault(0.6) // must not mistake the previous default for a declaration
		assert.InDelta(t, 0.6, thresholdOf(t, r, "bare"), 1e-09)
		assert.InDelta(t, 0.5, thresholdOf(t, r, "declared"), 1e-09)
	})

	t.Run("zero_restores_builtin_default", func(t *testing.T) {
		r := build(t)
		r.SetCompactDefault(0.7)
		r.SetCompactDefault(0)
		assert.Zero(t, thresholdOf(t, r, "bare"))
	})

	t.Run("survives_rebuild", func(t *testing.T) {
		r := build(t)
		r.SetCompactDefault(0.7)
		r.Refresh(t.Context(), DiscoverOptions{Env: func(string) string { return "" }})
		assert.InDelta(t, 0.7, thresholdOf(t, r, "bare"), 1e-09)
		assert.InDelta(t, 0.5, thresholdOf(t, r, "declared"), 1e-09)
	})
}
