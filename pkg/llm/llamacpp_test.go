package llm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLlamaProps(t *testing.T) {
	t.Parallel()

	t.Run("reports_the_real_loaded_context", func(t *testing.T) {
		data, err := os.ReadFile(filepath.Join("testdata", "llamacpp", "props.json"))
		require.NoError(t, err)

		got, err := parseLlamaProps(data)
		require.NoError(t, err)
		require.Len(t, got, 1)

		// only the server knows this, which is why discovery beats configuration
		require.NotNil(t, got[0].ContextWindow)
		assert.Equal(t, 32768, *got[0].ContextWindow)
		assert.Equal(t, "qwen3.6-27b-Q4_K_M.gguf", got[0].ID)
	})
	t.Run("falls_back_to_per_slot_context", func(t *testing.T) {
		got, err := parseLlamaProps([]byte(`{"model_path":"/m/x.gguf","n_ctx_per_slot":4096}`))
		require.NoError(t, err)
		require.NotNil(t, got[0].ContextWindow)
		assert.Equal(t, 4096, *got[0].ContextWindow)
	})
	t.Run("missing_model_path_still_yields_an_entry", func(t *testing.T) {
		got, err := parseLlamaProps([]byte(`{"default_generation_settings":{"n_ctx":2048}}`))
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "llamacpp", got[0].ID)
	})
	t.Run("unknown_context_is_left_unset", func(t *testing.T) {
		got, err := parseLlamaProps([]byte(`{"model_path":"/m/x.gguf"}`))
		require.NoError(t, err)
		assert.Nil(t, got[0].ContextWindow)
	})
	t.Run("malformed_body_errors", func(t *testing.T) {
		_, err := parseLlamaProps([]byte("nope"))
		assert.Error(t, err)
	})
}

func TestDecorateLlamaCpp(t *testing.T) {
	t.Parallel()

	model := func(tags bool) Model {
		caps := flavorDefaults[FlavorLlamaCpp].caps
		if tags {
			caps.ThinkOpen, caps.ThinkClose = thinkOpenTag, thinkCloseTag
		} else {
			caps.ThinkOpen, caps.ThinkClose = "", ""
		}
		return Model{ID: "m1", Caps: caps}
	}

	t.Run("enables_thinking_above_off", func(t *testing.T) {
		var body compatRequest
		decorateLlamaCpp(&body, Request{
			Model: model(true), Reasoning: ReasoningConfig{Level: LevelMedium},
		})
		assert.Equal(t, true, body.ChatTemplateKwarg["enable_thinking"])
		require.NotNil(t, body.CachePrompt)
		assert.True(t, *body.CachePrompt)
	})
	t.Run("disables_thinking_at_off", func(t *testing.T) {
		var body compatRequest
		decorateLlamaCpp(&body, Request{
			Model: model(true), Reasoning: ReasoningConfig{Level: LevelOff},
		})
		assert.Equal(t, false, body.ChatTemplateKwarg["enable_thinking"])
	})
	t.Run("no_template_kwarg_for_non_tag_models", func(t *testing.T) {
		var body compatRequest
		decorateLlamaCpp(&body, Request{Model: model(false)})
		assert.Empty(t, body.ChatTemplateKwarg)
	})
	t.Run("older_builds_get_no_stream_options", func(t *testing.T) {
		// an unknown key is rejected outright by older servers
		assert.False(t, flavorDefaults[FlavorLlamaCpp].caps.StreamUsage)
	})
}

func TestCompatProviderCountTokens(t *testing.T) {
	t.Parallel()

	t.Run("returns_the_exact_count", func(t *testing.T) {
		srv, req := jsonServer(t, "llamacpp/tokenize.json")
		p := newCompatTestProvider(t, srv.URL)

		caps := flavorDefaults[FlavorLlamaCpp].caps
		got, err := p.CountTokens(t.Context(), Request{
			Model:    Model{ID: "m1", Caps: caps},
			System:   BlockList{TextBlock{Text: "sys"}},
			Messages: []Message{Text(RoleUser, "hello")},
		})
		require.NoError(t, err)
		assert.Equal(t, 7, got)
		assert.Equal(t, "/tokenize", req.Path)

		var sent map[string]string
		require.NoError(t, json.Unmarshal(req.Body, &sent))
		assert.Equal(t, "syshello", sent["content"])
	})
	t.Run("refused_without_a_tokenizer_endpoint", func(t *testing.T) {
		p := newCompatTestProvider(t, "http://127.0.0.1:1")
		caps := flavorDefaults[FlavorLMStudio].caps

		_, err := p.CountTokens(t.Context(), Request{Model: Model{ID: "m1", Caps: caps}})
		assert.ErrorIs(t, err, ErrNoTokenizer)
	})
}
