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

		// the fixture's build reports stream usage support, so discovery enables it
		require.NotNil(t, got[0].Compat)
		assert.True(t, *got[0].Compat.SupportsUsageInStreaming)
	})

	t.Run("falls_back_to_per_slot_context", func(t *testing.T) {
		got, err := parseLlamaProps([]byte(`{"model_path":"/m/x.gguf","n_ctx_per_slot":4096}`))
		require.NoError(t, err)
		require.NotNil(t, got[0].ContextWindow)
		assert.Equal(t, 4096, *got[0].ContextWindow)
	})

	t.Run("missing_model_path_is_a_router", func(t *testing.T) {
		got, err := parseLlamaProps([]byte(`{"default_generation_settings":{"n_ctx":2048}}`))
		require.NoError(t, err)
		assert.Empty(t, got) // nothing to name, so discovery falls back to /v1/models
	})

	t.Run("none_model_path_is_a_router", func(t *testing.T) {
		got, err := parseLlamaProps([]byte(`{"model_path":"none"}`))
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("degenerate_base_name_yields_nothing", func(t *testing.T) {
		got, err := parseLlamaProps([]byte(`{"model_path":"/"}`))
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("unknown_context_is_left_unset", func(t *testing.T) {
		got, err := parseLlamaProps([]byte(`{"model_path":"/m/x.gguf"}`))
		require.NoError(t, err)
		assert.Nil(t, got[0].ContextWindow)
	})

	t.Run("absent_build_info_keeps_stream_usage_off", func(t *testing.T) {
		got, err := parseLlamaProps([]byte(`{"model_path":"/m/x.gguf"}`))
		require.NoError(t, err)
		assert.Nil(t, got[0].Compat)
	})

	t.Run("malformed_body_errors", func(t *testing.T) {
		_, err := parseLlamaProps([]byte("nope"))
		assert.Error(t, err)
	})
}

func TestLlamaSupportsStreamUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		buildInfo string
		expected  bool
	}{
		{"current_build", "b10700-d07e71ede", true},
		{"first_supported_build", "b6508-abcdef012", true},
		{"one_below_the_cutoff", "b6507-abcdef012", false},
		{"bare_number_without_prefix", "6508", true},
		{"empty_build_info", "", false},
		{"commit_only_names_nothing", "bd07e71ede", false},
		{"junk_text_is_unsupported", "unknown", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, llamaSupportsStreamUsage(tc.buildInfo))
		})
	}
}

func TestLlamaCppStreamUsageResolution(t *testing.T) {
	t.Parallel()

	base := flavorDefaults[FlavorLlamaCpp].caps
	ctx := modelContext{provider: "llamacpp", dialect: DialectOpenAICompletions,
		baseURL: flavorDefaults[FlavorLlamaCpp].baseURL}

	data, err := os.ReadFile(filepath.Join("testdata", "llamacpp", "props.json"))
	require.NoError(t, err)
	discovered, err := parseLlamaProps(data)
	require.NoError(t, err)
	require.Len(t, discovered, 1)

	t.Run("discovery_turns_stream_usage_on", func(t *testing.T) {
		m := resolveModel(ctx, base, nil, discovered[0])
		assert.True(t, m.Caps.StreamUsage)
	})

	t.Run("declared_compat_beats_discovery", func(t *testing.T) {
		// a user turning it back off must win over the probed default
		merged, _ := mergeModels([]ModelConfig{{ID: discovered[0].ID,
			Compat: &Compat{SupportsUsageInStreaming: ptr(false)}}}, discovered, nil)
		m := resolveModel(ctx, base, nil, merged[0])
		assert.False(t, m.Caps.StreamUsage)
	})

	t.Run("provider_compat_fills_where_discovery_is_silent", func(t *testing.T) {
		// an old build sets nothing, so a hand-written enablement survives
		old, err := parseLlamaProps([]byte(`{"model_path":"/m/x.gguf","build_info":"b6000-old"}`))
		require.NoError(t, err)
		m := resolveModel(ctx, base, &Compat{SupportsUsageInStreaming: ptr(true)}, old[0])
		assert.True(t, m.Caps.StreamUsage)
	})
}

func TestParseOpenAIModels(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "openaimodels", "models.json"))
	require.NoError(t, err)

	got, err := parseOpenAIModels(data)
	require.NoError(t, err)
	require.Len(t, got, 3) // no status field: every model stays available
	assert.Equal(t, []Modality{ModalityText}, got[0].Input)
	assert.Equal(t, []Modality{ModalityText, ModalityImage}, got[1].Input)

	t.Run("router_drops_unloaded_keeps_context", func(t *testing.T) {
		data, err := os.ReadFile(filepath.Join("testdata", "openaimodels", "models_router.json"))
		require.NoError(t, err)

		got, err := parseOpenAIModels(data)
		require.NoError(t, err)
		require.Len(t, got, 1) // only the loaded model survives
		assert.Equal(t, "unsloth/GLM-5.3-Flash-GGUF:Q6_K_XL", got[0].ID)
		require.NotNil(t, got[0].ContextWindow)
		assert.Equal(t, 667392, *got[0].ContextWindow) // meta.n_ctx
	})

	t.Run("unloaded_without_meta_is_dropped", func(t *testing.T) {
		body := []byte(`{"data":[{"id":"a","status":{"value":"unloaded"}}]}`)
		got, err := parseOpenAIModels(body)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("malformed_body_errors", func(t *testing.T) {
		_, err := parseOpenAIModels([]byte("not json"))
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
