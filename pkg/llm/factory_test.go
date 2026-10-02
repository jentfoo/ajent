package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProfileFor(t *testing.T) {
	t.Parallel()

	t.Run("openrouter_base_url_wins_over_key", func(t *testing.T) {
		p := profileFor("myrouter", FlavorGeneric, "https://openrouter.ai/api/v1", ProviderConfig{})
		assert.NotNil(t, p.decorate)
		assert.NotNil(t, p.extra) // reasoning_details capture, what caps already detected
		assert.Empty(t, p.path)   // the base appends the default path verbatim
	})

	t.Run("openrouter_key_with_foreign_base_keeps_flavor", func(t *testing.T) {
		p := profileFor("myrouter", FlavorOpenRouter, "https://proxy.internal", ProviderConfig{})
		assert.NotNil(t, p.decorate)
		assert.NotNil(t, p.extra)
	})

	t.Run("openrouter_detection_beats_a_local_flavor", func(t *testing.T) {
		// a pathological pairing still resolves to the detected endpoint
		p := profileFor("myserver", FlavorLlamaCpp, "https://openrouter.ai/api/v1", ProviderConfig{})
		assert.Empty(t, p.path)
	})

	t.Run("local_flavors_pin_the_v1_path", func(t *testing.T) {
		for _, flavor := range []Flavor{FlavorLlamaCpp, FlavorLMStudio} {
			for _, base := range []string{"http://localhost:8080", "http://localhost:8080/v1"} {
				p := profileFor("myserver", flavor, base, ProviderConfig{})
				assert.Equal(t, localChatPath, p.path, "%s %s", flavor, base)
			}
		}
	})

	t.Run("generic_bare_host_appends_verbatim", func(t *testing.T) {
		// an external models.json resolves by appending /chat/completions verbatim,
		// so a configuration written elsewhere drops in unchanged
		p := profileFor("myserver", FlavorGeneric, "http://localhost:8080", ProviderConfig{})
		assert.Empty(t, p.path)
	})

	t.Run("hosted_flavors_append_verbatim", func(t *testing.T) {
		for _, flavor := range []Flavor{FlavorGeneric, FlavorDeepSeek, FlavorZAI, FlavorOpenAI} {
			p := profileFor("myserver", flavor, "http://localhost:8080", ProviderConfig{})
			assert.Empty(t, p.path, flavor.String())
		}
	})
}

func TestNewProviderLocalChatPath(t *testing.T) {
	t.Parallel()

	t.Run("llamacpp_v1_base_never_doubles", func(t *testing.T) {
		// a user-typed /v1 is stripped then the chat path pins the prefix, so both
		// spellings of the server URL hit /v1/chat/completions exactly once
		srv, req := sseServer(t, "compat/text.sse")
		p, err := NewProvider("llamacpp", ProviderConfig{Flavor: FlavorLlamaCpp},
			FlavorLlamaCpp, ProviderOptions{
				Dialect: DialectOpenAICompletions, BaseURL: srv.URL + "/v1",
			})
		require.NoError(t, err)

		s, err := p.Stream(t.Context(), Request{Model: compatModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		assert.Equal(t, localChatPath, req.Path)
	})

	t.Run("llamacpp_bare_host_pins_v1", func(t *testing.T) {
		srv, req := sseServer(t, "compat/text.sse")
		p, err := NewProvider("llamacpp", ProviderConfig{Flavor: FlavorLlamaCpp},
			FlavorLlamaCpp, ProviderOptions{
				Dialect: DialectOpenAICompletions, BaseURL: srv.URL,
			})
		require.NoError(t, err)

		s, err := p.Stream(t.Context(), Request{Model: compatModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		assert.Equal(t, localChatPath, req.Path)
	})

	t.Run("generic_bare_host_appends_default", func(t *testing.T) {
		srv, req := sseServer(t, "compat/text.sse")
		p, err := NewProvider("myserver", ProviderConfig{}, FlavorGeneric, ProviderOptions{
			Dialect: DialectOpenAICompletions, BaseURL: srv.URL,
		})
		require.NoError(t, err)

		s, err := p.Stream(t.Context(), Request{Model: compatModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		assert.Equal(t, "/chat/completions", req.Path)
	})
}

func TestStripV1Suffix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
		want string
	}{
		{"bare_host", "http://localhost:8080", "http://localhost:8080"},
		{"v1_base", "http://localhost:8080/v1", "http://localhost:8080"},
		{"v1_base_slash", "http://localhost:8080/v1/", "http://localhost:8080"},
		{"double_slash", "http://localhost:8080//v1", "http://localhost:8080/"},
		{"v1beta_kept", "https://host/v1beta", "https://host/v1beta"},
		{"deep_path_kept", "https://host/api/coding/v4", "https://host/api/coding/v4"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, stripV1Suffix(tc.url))
		})
	}
}
