package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProvidersProviderFor(t *testing.T) {
	t.Parallel()

	// one gateway serving two dialects, which is what per-model api is for
	f := File{Providers: map[string]ProviderConfig{
		"gateway": {
			BaseURL: "https://gw.example.com/v1",
			APIKey:  "k",
			Models: []ModelConfig{
				{ID: "chat", API: DialectOpenAICompletions},
				{ID: "claude", API: DialectAnthropic},
				{ID: "elsewhere", API: DialectOpenAICompletions, BaseURL: "https://other.example.com/v1"},
			},
		},
	}}
	reg, warnings := NewRegistry(f, nil, RegistryOptions{})
	require.Empty(t, warnings)
	cache := NewProviders(reg)

	get := func(id string) Provider {
		m, err := reg.Resolve("gateway/" + id)
		require.NoError(t, err)
		p, err := cache.ProviderFor(m)
		require.NoError(t, err)
		return p
	}

	t.Run("same_model_is_cached", func(t *testing.T) {
		first := get("chat")
		assert.Same(t, first, get("chat"))
	})
	t.Run("differing_dialect_gets_its_own_adapter", func(t *testing.T) {
		assert.NotSame(t, get("chat"), get("claude"))
		assert.IsType(t, &anthropicProvider{}, get("claude"))
		assert.IsType(t, &compatProvider{}, get("chat"))
	})
	t.Run("differing_base_url_gets_its_own_adapter", func(t *testing.T) {
		assert.NotSame(t, get("chat"), get("elsewhere"))
	})
}
