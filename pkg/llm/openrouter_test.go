package llm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openRouterModel returns an openrouter model with optional capability tweaks.
func openRouterModel(fn func(*Capabilities)) Model {
	caps := flavorDefaults[FlavorOpenRouter].caps
	if fn != nil {
		fn(&caps)
	}
	return Model{Provider: "openrouter", ID: "anthropic/claude-opus-4-5",
		ContextWindow: 200000, MaxOutput: 64000, Caps: caps}
}

func newOpenRouterTestProvider(t *testing.T, url string, routing *Routing) *compatProvider {
	t.Helper()

	c, err := newHTTPClient(clientOptions{provider: "openrouter", baseURL: url})
	require.NoError(t, err)
	return &compatProvider{client: c, profile: profileFor("openrouter", FlavorOpenRouter,
		ProviderConfig{Routing: routing})}
}

func TestOpenRouterStream(t *testing.T) {
	t.Parallel()

	t.Run("reasoning_details_captured_verbatim", func(t *testing.T) {
		srv, _ := sseServer(t, "openrouter/reasoning_details.sse")
		p := newOpenRouterTestProvider(t, srv.URL, nil)

		s, err := p.Stream(t.Context(), Request{Model: openRouterModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		msg, usage, err := Accumulate(s)
		require.NoError(t, err)
		require.Len(t, msg.Content, 2)

		think, ok := msg.Content[0].(ThinkingBlock)
		require.True(t, ok)
		assert.Equal(t, "weighing options", think.Text)
		assert.JSONEq(t,
			`[{"type":"reasoning.text","text":"weighing options","signature":"sig-abc"}]`,
			string(think.Details))
		assert.Equal(t, TextBlock{Text: "the answer"}, msg.Content[1])
		assert.Equal(t, Usage{Input: 30, Output: 9}, usage)
	})
	t.Run("details_round_trip_into_the_next_request", func(t *testing.T) {
		// this is what keeps signatures intact when openrouter routes to anthropic
		srv, _ := sseServer(t, "openrouter/reasoning_details.sse")
		p := newOpenRouterTestProvider(t, srv.URL, nil)

		s, err := p.Stream(t.Context(), Request{Model: openRouterModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		msg, _, err := Accumulate(s)
		require.NoError(t, err)

		body, err := buildCompatBody(Request{
			Model:     openRouterModel(nil),
			Messages:  []Message{Text(RoleUser, "q"), msg},
			Reasoning: ReasoningConfig{Retain: RetainAll},
		}, profileFor("openrouter", FlavorOpenRouter, ProviderConfig{}))
		require.NoError(t, err)
		assert.Contains(t, string(body), `"sig-abc"`)
		assert.Contains(t, string(body), `"reasoning_details"`)
	})
}

func TestDecorateOpenRouter(t *testing.T) {
	t.Parallel()

	build := func(t *testing.T, req Request, routing *Routing) map[string]any {
		t.Helper()

		body, err := buildCompatBody(req, compatProfile{decorate: decorateOpenRouter(routing)})
		require.NoError(t, err)

		var m map[string]any
		require.NoError(t, json.Unmarshal(body, &m))
		return m
	}
	baseReq := func() Request {
		return Request{Model: openRouterModel(nil), Messages: []Message{Text(RoleUser, "hi")}}
	}

	t.Run("requests_usage_reporting", func(t *testing.T) {
		m := build(t, baseReq(), nil)
		assert.Equal(t, true, m["usage"].(map[string]any)["include"])
	})
	t.Run("effort_from_the_level", func(t *testing.T) {
		req := baseReq()
		req.Reasoning = ReasoningConfig{Level: LevelHigh}

		m := build(t, req, nil)
		assert.Equal(t, "high", m["reasoning"].(map[string]any)["effort"])
		assert.NotContains(t, m, "reasoning_effort") // carried in the object instead
	})
	t.Run("explicit_budget_becomes_max_tokens", func(t *testing.T) {
		req := baseReq()
		req.Reasoning = ReasoningConfig{Level: LevelHigh, Budget: 4096}

		m := build(t, req, nil)
		assert.InDelta(t, 4096, m["reasoning"].(map[string]any)["max_tokens"], 0.001)
	})
	t.Run("level_off_excludes_reasoning", func(t *testing.T) {
		req := baseReq()
		req.Reasoning = ReasoningConfig{Level: LevelOff}

		m := build(t, req, nil)
		assert.Equal(t, true, m["reasoning"].(map[string]any)["exclude"])
	})
	t.Run("routing_preference_passed_through", func(t *testing.T) {
		m := build(t, baseReq(), &Routing{
			Order: []string{"anthropic"}, AllowFallbacks: ptr(false), DataCollection: "deny",
		})
		provider := m["provider"].(map[string]any)
		assert.Equal(t, []any{"anthropic"}, provider["order"])
		assert.Equal(t, false, provider["allow_fallbacks"])
		assert.Equal(t, "deny", provider["data_collection"])
	})
	t.Run("no_provider_block_without_routing", func(t *testing.T) {
		assert.NotContains(t, build(t, baseReq(), nil), "provider")
	})
}

func TestParseOpenRouterModels(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "openrouter", "models.json"))
	require.NoError(t, err)

	got, err := parseOpenRouterModels(data)
	require.NoError(t, err)
	require.Len(t, got, 2)

	t.Run("context_and_output_limits", func(t *testing.T) {
		require.NotNil(t, got[0].ContextWindow)
		assert.Equal(t, 200000, *got[0].ContextWindow)
		require.NotNil(t, got[0].MaxTokens)
		assert.Equal(t, 64000, *got[0].MaxTokens)
	})
	t.Run("input_modalities", func(t *testing.T) {
		assert.Equal(t, []Modality{ModalityText, ModalityImage}, got[0].Input)
		assert.Equal(t, []Modality{ModalityText}, got[1].Input)
	})
	t.Run("reasoning_support_detected", func(t *testing.T) {
		require.NotNil(t, got[0].Reasoning)
		assert.Equal(t, ReasoningOpenRouter, *got[0].Reasoning)
		assert.Nil(t, got[1].Reasoning)
	})
	t.Run("tool_support_detected", func(t *testing.T) {
		require.NotNil(t, got[0].Compat)
		assert.True(t, *got[0].Compat.SupportsToolChoice)
		assert.Nil(t, got[1].Compat)
	})
	t.Run("no_pricing_is_carried", func(t *testing.T) {
		// pricing is out of scope, so the response fields are dropped
		encoded, err := json.Marshal(got)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), "pricing")
		assert.NotContains(t, string(encoded), "cost")
	})
	t.Run("malformed_body_errors", func(t *testing.T) {
		_, err := parseOpenRouterModels([]byte("nope"))
		assert.Error(t, err)
	})
}

func TestOpenRouterDiscovery(t *testing.T) {
	t.Parallel()

	srv, req := jsonServer(t, "openrouter/models.json")
	c, _, _ := testClient(t, srv.URL)

	got, err := discoverProvider(t.Context(), c, "/models", parseOpenRouterModels, CacheEntry{}, testNow)
	require.NoError(t, err)

	assert.Equal(t, "/models", req.Path)
	assert.Len(t, got.Models, 2)
}
