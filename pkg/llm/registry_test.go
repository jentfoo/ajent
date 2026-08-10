package llm

import (
	"testing"

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
		assert.Equal(t, ReasoningAnthropicBudget, m.Caps.Reasoning)
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
		assert.Equal(t, ReasoningInlineTags, m.Caps.Reasoning)
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
		got := mergeModels(declared, discovered)
		require.Len(t, got, 2) // "c" is not added
		assert.Equal(t, "a", got[0].ID)
		assert.Equal(t, "b", got[1].ID)
	})
	t.Run("declared_fields_win", func(t *testing.T) {
		got := mergeModels(declared, discovered)
		require.NotNil(t, got[0].ContextWindow)
		assert.Equal(t, 1000, *got[0].ContextWindow)
	})
	t.Run("discovery_fills_unset_fields", func(t *testing.T) {
		got := mergeModels(declared, discovered)
		assert.Equal(t, "Discovered A", got[0].Name)
		require.NotNil(t, got[1].ContextWindow)
		assert.Equal(t, 2000, *got[1].ContextWindow) // the real loaded window
	})
	t.Run("discovery_supplies_everything_when_nothing_declared", func(t *testing.T) {
		got := mergeModels(nil, discovered)
		assert.Len(t, got, 3)
	})
	t.Run("declared_only", func(t *testing.T) {
		got := mergeModels(declared, nil)
		assert.Len(t, got, 2)
	})
	t.Run("both_empty", func(t *testing.T) {
		assert.Empty(t, mergeModels(nil, nil))
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
