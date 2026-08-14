package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// anthropicModel returns a Messages API model with optional capability tweaks.
func anthropicModel(fn func(*Capabilities)) Model {
	caps := flavorDefaults[FlavorAnthropic].caps
	if fn != nil {
		fn(&caps)
	}
	if caps.Reasoning && len(caps.Budgets) == 0 {
		caps.Budgets = defaultBudgets(64000)
	}
	return Model{Provider: "anthropic", ID: "claude-opus-4-5",
		ContextWindow: 200000, MaxOutput: 64000, Caps: caps}
}

// buildBody marshals an anthropic request, failing the test on error.
func buildBody(t *testing.T, req Request) []byte {
	t.Helper()
	body, err := buildAnthropicBody(req)
	require.NoError(t, err)
	return body
}

func newAnthropicTestProvider(t *testing.T, url string) *anthropicProvider {
	t.Helper()

	c, err := newHTTPClient(clientOptions{provider: "anthropic", baseURL: url})
	require.NoError(t, err)
	return newAnthropicProvider("anthropic", c)
}

func TestAnthropicProviderStream(t *testing.T) {
	t.Parallel()

	t.Run("text_only", func(t *testing.T) {
		srv, _ := sseServer(t, "anthropic/text.sse")
		p := newAnthropicTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: anthropicModel(nil)})
		require.NoError(t, err)
		events := collect(t, s)

		assert.Equal(t, "Hello there", textOf(events))
		assert.Equal(t, []string{
			"message_start", "usage", "text_start", "text_delta", "text_delta",
			"text_end", "usage", "done",
		}, eventKinds(events))
	})
	t.Run("reports_model_and_request_id", func(t *testing.T) {
		srv, _ := sseServer(t, "anthropic/text.sse")
		p := newAnthropicTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: anthropicModel(nil)})
		require.NoError(t, err)
		events := collect(t, s)

		require.NotNil(t, events[0].Meta)
		assert.Equal(t, "claude-opus-4-5", events[0].Meta.Model)
		assert.Equal(t, "msg_01", events[0].Meta.RequestID)
	})
	t.Run("usage_merges_input_and_output", func(t *testing.T) {
		srv, _ := sseServer(t, "anthropic/text.sse")
		p := newAnthropicTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: anthropicModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		_, usage, err := Accumulate(s)
		require.NoError(t, err)
		assert.Equal(t, Usage{Input: 412, Output: 7}, usage)
	})
	t.Run("thinking_accumulates_its_signature", func(t *testing.T) {
		srv, _ := sseServer(t, "anthropic/thinking_text.sse")
		p := newAnthropicTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: anthropicModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		msg, _, err := Accumulate(s)
		require.NoError(t, err)
		require.Len(t, msg.Content, 2)

		think, ok := msg.Content[0].(ThinkingBlock)
		require.True(t, ok)
		assert.Equal(t, "let me consider", think.Text)
		assert.Equal(t, "sig-part-1sig-part-2", think.Signature) // joined across deltas
		assert.Equal(t, TextBlock{Text: "the answer"}, msg.Content[1])
	})
	t.Run("redacted_thinking_kept_verbatim", func(t *testing.T) {
		srv, _ := sseServer(t, "anthropic/redacted_thinking.sse")
		p := newAnthropicTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: anthropicModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		msg, _, err := Accumulate(s)
		require.NoError(t, err)
		require.Len(t, msg.Content, 1)

		think, ok := msg.Content[0].(ThinkingBlock)
		require.True(t, ok)
		assert.Equal(t, "cmVkYWN0ZWQtcGF5bG9hZA==", think.Redacted) // base64 untouched
	})
	t.Run("tool_arguments_split_mid_token", func(t *testing.T) {
		srv, _ := sseServerChunked(t, "anthropic/tool_args_split.sse", 17)
		p := newAnthropicTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: anthropicModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		msg, usage, err := Accumulate(s)
		require.NoError(t, err)
		require.Len(t, msg.Content, 2)

		call, ok := msg.Content[1].(ToolCallBlock)
		require.True(t, ok)
		assert.Equal(t, "toolu_01", call.ID)
		assert.Equal(t, "read", call.Name)
		assert.JSONEq(t, `{"path":"main.go"}`, string(call.Input))
		assert.Equal(t, Usage{Input: 412, Output: 37, CacheRead: 8}, usage)
	})
	t.Run("stop_reason_tool_use", func(t *testing.T) {
		srv, _ := sseServer(t, "anthropic/tool_args_split.sse")
		p := newAnthropicTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: anthropicModel(nil)})
		require.NoError(t, err)
		events := collect(t, s)

		last := events[len(events)-1]
		assert.Equal(t, StopToolUse, last.StopReason)
	})
	t.Run("mid_stream_error_surfaces_the_partial", func(t *testing.T) {
		srv, _ := sseServer(t, "anthropic/error_midstream.sse")
		p := newAnthropicTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: anthropicModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		var events []Event
		for ev, ok := s.Next(); ok; ev, ok = s.Next() {
			events = append(events, ev)
		}
		require.Error(t, s.Err())
		assert.Equal(t, "partial", textOf(events))
		assert.Equal(t, StopError, events[len(events)-1].StopReason)
	})
	t.Run("close_mid_stream_is_not_an_error", func(t *testing.T) {
		srv, _ := sseServer(t, "anthropic/text.sse")
		p := newAnthropicTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: anthropicModel(nil)})
		require.NoError(t, err)

		_, ok := s.Next()
		require.True(t, ok)
		require.NoError(t, s.Close())

		_, ok = s.Next()
		assert.False(t, ok)
		assert.NoError(t, s.Err())
	})
}

func TestBuildAnthropicBody(t *testing.T) {
	t.Parallel()

	baseReq := func() Request {
		return Request{
			Model:     anthropicModel(nil),
			System:    BlockList{TextBlock{Text: "be terse"}},
			Messages:  []Message{Text(RoleUser, "hi")},
			MaxTokens: 16000,
		}
	}
	decode := func(t *testing.T, body []byte) map[string]any {
		t.Helper()
		var m map[string]any
		require.NoError(t, json.Unmarshal(body, &m))
		return m
	}

	t.Run("system_is_a_top_level_field", func(t *testing.T) {
		body, err := buildAnthropicBody(baseReq())
		require.NoError(t, err)
		assert.JSONEq(t, `{
			"model": "claude-opus-4-5",
			"max_tokens": 16000,
			"stream": true,
			"system": [{"type": "text", "text": "be terse"}],
			"thinking": {"type": "disabled"},
			"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}]
		}`, string(body))
	})
	t.Run("max_tokens_falls_back_to_the_model", func(t *testing.T) {
		req := baseReq()
		req.MaxTokens = 0

		body, err := buildAnthropicBody(req)
		require.NoError(t, err)
		assert.InDelta(t, 64000, decode(t, body)["max_tokens"], 0.001)
	})
	t.Run("thinking_budget_from_the_level", func(t *testing.T) {
		req := baseReq()
		req.Reasoning = ReasoningConfig{Level: LevelMedium}

		body, err := buildAnthropicBody(req)
		require.NoError(t, err)

		thinking := decode(t, body)["thinking"].(map[string]any)
		assert.Equal(t, "enabled", thinking["type"])
		assert.InDelta(t, 8192, thinking["budget_tokens"], 0.001)
	})
	t.Run("explicit_budget_overrides_the_level", func(t *testing.T) {
		req := baseReq()
		req.Reasoning = ReasoningConfig{Level: LevelLow, Budget: 5000}

		body, err := buildAnthropicBody(req)
		require.NoError(t, err)

		thinking := decode(t, body)["thinking"].(map[string]any)
		assert.InDelta(t, 5000, thinking["budget_tokens"], 0.001)
	})
	t.Run("temperature_dropped_when_thinking", func(t *testing.T) {
		// the API rejects any temperature but the default alongside thinking
		req := baseReq()
		temp := 0.7
		req.Temperature = &temp
		req.Reasoning = ReasoningConfig{Level: LevelHigh}

		body, err := buildAnthropicBody(req)
		require.NoError(t, err)

		m := decode(t, body)
		assert.NotContains(t, m, "temperature")
		assert.Contains(t, m, "thinking")
	})
	t.Run("temperature_kept_without_thinking", func(t *testing.T) {
		req := baseReq()
		temp := 0.7
		req.Temperature = &temp

		body, err := buildAnthropicBody(req)
		require.NoError(t, err)
		assert.InDelta(t, 0.7, decode(t, body)["temperature"], 0.001)
	})
	t.Run("small_max_tokens_is_inflated_for_thinking", func(t *testing.T) {
		// part C inflates max_tokens by the budget so a small request still leaves
		// room for both reasoning and a full answer
		req := baseReq()
		req.MaxTokens = 1000
		req.Reasoning = ReasoningConfig{Level: LevelMedium}

		body, err := buildAnthropicBody(req)
		require.NoError(t, err)

		m := decode(t, body)
		thinking := m["thinking"].(map[string]any)
		assert.Equal(t, "enabled", thinking["type"])
	})
	t.Run("off_emits_the_disabled_shape", func(t *testing.T) {
		req := baseReq()
		req.Reasoning = ReasoningConfig{Level: LevelOff}

		body, err := buildAnthropicBody(req)
		require.NoError(t, err)

		m := decode(t, body)
		assert.Contains(t, m, "thinking")
		assert.Equal(t, "disabled", m["thinking"].(map[string]any)["type"])
	})
	t.Run("tool_results_ride_on_a_user_message", func(t *testing.T) {
		req := baseReq()
		req.Messages = []Message{
			Text(RoleUser, "read it"),
			{Role: RoleAssistant, Content: BlockList{
				ToolCallBlock{ID: "toolu_1", Name: "read", Input: json.RawMessage(`{"p":1}`)},
			}},
			{Role: RoleTool, Content: BlockList{
				ToolResultBlock{CallID: "toolu_1", Content: BlockList{TextBlock{Text: "body"}}},
			}},
		}
		body, err := buildAnthropicBody(req)
		require.NoError(t, err)

		msgs := decode(t, body)["messages"].([]any)
		require.Len(t, msgs, 3)
		last := msgs[2].(map[string]any)
		assert.Equal(t, "user", last["role"]) // never a tool role

		blocks := last["content"].([]any)
		assert.Equal(t, "tool_result", blocks[0].(map[string]any)["type"])
		assert.Equal(t, "toolu_1", blocks[0].(map[string]any)["tool_use_id"])
	})
	t.Run("consecutive_same_role_messages_merge", func(t *testing.T) {
		req := baseReq()
		req.Messages = []Message{
			Text(RoleUser, "one"),
			Text(RoleUser, "two"),
		}
		body, err := buildAnthropicBody(req)
		require.NoError(t, err)

		msgs := decode(t, body)["messages"].([]any)
		require.Len(t, msgs, 1)
		assert.Len(t, msgs[0].(map[string]any)["content"].([]any), 2)
	})
	t.Run("thinking_replayed_with_its_signature", func(t *testing.T) {
		req := baseReq()
		req.Reasoning.Retain = RetainAll
		req.Messages = sameOrigin(req.Model, []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{
				ThinkingBlock{Text: "because", Signature: "sig"},
				TextBlock{Text: "answer"},
			}},
		})
		body, err := buildAnthropicBody(req)
		require.NoError(t, err)
		assert.Contains(t, string(body), `"signature":"sig"`)
		assert.Contains(t, string(body), `"thinking":"because"`)
	})
	t.Run("unsigned_thinking_is_not_replayed", func(t *testing.T) {
		req := baseReq()
		req.Reasoning.Retain = RetainAll
		req.Messages = sameOrigin(req.Model, []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{
				ThinkingBlock{Text: "no signature"},
				TextBlock{Text: "answer"},
			}},
		})
		body, err := buildAnthropicBody(req)
		require.NoError(t, err)
		assert.NotContains(t, string(body), "no signature")
	})
	t.Run("cache_breakpoints_on_system_and_tools", func(t *testing.T) {
		req := baseReq()
		req.Cache = CachePolicy{Enabled: true}
		req.Tools = []ToolSchema{{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}}

		body, err := buildAnthropicBody(req)
		require.NoError(t, err)

		m := decode(t, body)
		system := m["system"].([]any)[0].(map[string]any)
		assert.Equal(t, "ephemeral", system["cache_control"].(map[string]any)["type"])

		tools := m["tools"].([]any)
		last := tools[len(tools)-1].(map[string]any)
		assert.Equal(t, "ephemeral", last["cache_control"].(map[string]any)["type"])
	})
	t.Run("cache_breakpoints_skip_the_newest_message", func(t *testing.T) {
		req := baseReq()
		req.Cache = CachePolicy{Enabled: true, KeepLast: 1}
		req.Messages = []Message{Text(RoleUser, "one"), Text(RoleAssistant, "two"), Text(RoleUser, "three")}

		body, err := buildAnthropicBody(req)
		require.NoError(t, err)

		msgs := decode(t, body)["messages"].([]any)
		newest := msgs[len(msgs)-1].(map[string]any)["content"].([]any)[0].(map[string]any)
		assert.NotContains(t, newest, "cache_control") // still changing
	})
	t.Run("long_retention_tier_when_supported", func(t *testing.T) {
		req := baseReq()
		req.Cache = CachePolicy{Enabled: true}
		req.Model.Caps.LongCache = true

		body, err := buildAnthropicBody(req)
		require.NoError(t, err)

		system := decode(t, body)["system"].([]any)[0].(map[string]any)
		assert.Equal(t, "1h", system["cache_control"].(map[string]any)["ttl"])
	})
	t.Run("default_tier_omits_the_ttl", func(t *testing.T) {
		req := baseReq()
		req.Cache = CachePolicy{Enabled: true}

		body, err := buildAnthropicBody(req)
		require.NoError(t, err)

		system := decode(t, body)["system"].([]any)[0].(map[string]any)
		assert.NotContains(t, system["cache_control"].(map[string]any), "ttl")
	})
	t.Run("no_cache_control_when_disabled", func(t *testing.T) {
		body, err := buildAnthropicBody(baseReq())
		require.NoError(t, err)
		assert.NotContains(t, string(body), "cache_control")
	})
	t.Run("tool_choice_modes", func(t *testing.T) {
		tests := []struct {
			name     string
			choice   ToolChoice
			expected string
		}{
			{"auto_is_omitted", ToolChoice{Mode: ToolChoiceAuto}, ""},
			{"required_is_any", ToolChoice{Mode: ToolChoiceRequired}, "any"},
			{"none", ToolChoice{Mode: ToolChoiceNone}, "none"},
			{"specific", ToolChoice{Mode: ToolChoiceSpecific, Name: "read"}, "tool"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				req := baseReq()
				req.Tools = []ToolSchema{{Name: "read"}}
				req.ToolChoice = tc.choice

				body, err := buildAnthropicBody(req)
				require.NoError(t, err)

				m := decode(t, body)
				if tc.expected == "" {
					assert.NotContains(t, m, "tool_choice")
					return
				}
				assert.Equal(t, tc.expected, m["tool_choice"].(map[string]any)["type"])
			})
		}
	})
	t.Run("image_becomes_a_base64_source", func(t *testing.T) {
		req := baseReq()
		req.Messages = []Message{{Role: RoleUser, Content: BlockList{
			ImageBlock{MediaType: "image/png", Data: []byte{1, 2, 3}},
		}}}
		body, err := buildAnthropicBody(req)
		require.NoError(t, err)
		assert.Contains(t, string(body), `"media_type":"image/png"`)
		assert.Contains(t, string(body), `"data":"AQID"`)
	})
}

func TestBuildAnthropicBodyInflation(t *testing.T) {
	t.Parallel()

	baseReq := func() Request {
		return Request{
			Model:    anthropicModel(nil),
			Messages: []Message{Text(RoleUser, "hi")},
		}
	}
	decode := func(t *testing.T, body []byte) map[string]any {
		t.Helper()
		var m map[string]any
		require.NoError(t, json.Unmarshal(body, &m))
		return m
	}

	t.Run("max_tokens_inflates_by_the_budget", func(t *testing.T) {
		// medium costs 8192, so a 16000 request asks for 24192 and the budget
		// fits within the inflated ceiling
		req := baseReq()
		req.MaxTokens = 16000
		req.Reasoning = ReasoningConfig{Level: LevelMedium}

		m := decode(t, buildBody(t, req))
		assert.InDelta(t, 16000+8192, m["max_tokens"], 0.001)
		assert.InDelta(t, 8192, m["thinking"].(map[string]any)["budget_tokens"], 0.001)
	})
	t.Run("inflation_clamps_at_the_model_cap", func(t *testing.T) {
		// near the full window the cap bites, and the budget still leaves the floor
		req := baseReq()
		req.MaxTokens = 60000
		req.Reasoning = ReasoningConfig{Level: LevelMedium}

		m := decode(t, buildBody(t, req))
		assert.InDelta(t, 64000, m["max_tokens"], 0.001)
		assert.InDelta(t, 8192, m["thinking"].(map[string]any)["budget_tokens"], 0.001)
	})
	t.Run("budget_keeps_the_answer_floor", func(t *testing.T) {
		// a budget bigger than the remaining window clamps so the answer floor stays
		req := baseReq()
		req.MaxTokens = 60000
		req.Reasoning = ReasoningConfig{Level: LevelMax, Budget: 65000}

		m := decode(t, buildBody(t, req))
		assert.InDelta(t, 64000-minAnswerTokens,
			m["thinking"].(map[string]any)["budget_tokens"], 0.001)
	})
	t.Run("floor_drops_thinking_and_restores_temperature", func(t *testing.T) {
		// a model cap too small to leave any room drops thinking entirely and
		// temperature, rejected while thinking is on, comes back
		req := baseReq()
		temp := 0.7
		req.Temperature = &temp
		req.Model.MaxOutput = minAnswerTokens + minThinkingBudget - 1
		req.Reasoning = ReasoningConfig{Level: LevelHigh}

		m := decode(t, buildBody(t, req))
		assert.NotContains(t, m, "thinking")
		assert.InDelta(t, temp, m["temperature"], 0.001)
	})
	t.Run("no_inflation_on_adaptive_models", func(t *testing.T) {
		req := baseReq()
		req.MaxTokens = 16000
		req.Reasoning = ReasoningConfig{Level: LevelMedium}
		req.Model.Caps.ForceAdaptiveThinking = true

		m := decode(t, buildBody(t, req))
		assert.InDelta(t, 16000, m["max_tokens"], 0.001)
	})
}

func TestBuildAnthropicBodyThinkingShape(t *testing.T) {
	t.Parallel()

	baseReq := func() Request {
		return Request{
			Model:     anthropicModel(nil),
			Messages:  []Message{Text(RoleUser, "hi")},
			MaxTokens: 16000,
		}
	}
	decode := func(t *testing.T, body []byte) map[string]any {
		t.Helper()
		var m map[string]any
		require.NoError(t, json.Unmarshal(body, &m))
		return m
	}

	t.Run("adaptive_shape_per_level", func(t *testing.T) {
		tests := []struct {
			level  Level
			effort string
		}{
			{LevelMinimal, "low"},
			{LevelLow, "low"},
			{LevelMedium, "medium"},
			{LevelHigh, "high"},
			{LevelXHigh, "high"},
			{LevelMax, "high"},
		}
		for _, tc := range tests {
			t.Run(tc.level.String(), func(t *testing.T) {
				req := baseReq()
				req.Reasoning = ReasoningConfig{Level: tc.level}
				req.Model.Caps.ForceAdaptiveThinking = true
				if tc.level >= LevelXHigh { // xhigh and max are opt-in levels
					high := "high"
					req.Model.Caps.LevelMap = map[Level]*string{tc.level: &high}
				}

				m := decode(t, buildBody(t, req))
				think := m["thinking"].(map[string]any)
				assert.Equal(t, "adaptive", think["type"])
				assert.NotContains(t, think, "budget_tokens")
				assert.Equal(t, "summarized", think["display"])
				assert.Equal(t, tc.effort, m["output_config"].(map[string]any)["effort"])
			})
		}
	})
	t.Run("level_map_overrides_the_default_effort", func(t *testing.T) {
		req := baseReq()
		req.Reasoning = ReasoningConfig{Level: LevelMedium}
		req.Model.Caps.ForceAdaptiveThinking = true
		max := "max"
		req.Model.Caps.LevelMap = map[Level]*string{LevelMedium: &max}

		m := decode(t, buildBody(t, req))
		assert.Equal(t, "max", m["output_config"].(map[string]any)["effort"])
	})
	t.Run("temperature_dropped_on_adaptive", func(t *testing.T) {
		req := baseReq()
		temp := 0.7
		req.Temperature = &temp
		req.Reasoning = ReasoningConfig{Level: LevelMedium}
		req.Model.Caps.ForceAdaptiveThinking = true

		m := decode(t, buildBody(t, req))
		assert.NotContains(t, m, "temperature")
		think := m["thinking"].(map[string]any)
		assert.Equal(t, "adaptive", think["type"])
		assert.Equal(t, "summarized", think["display"])
	})
	t.Run("off_null_suppresses_the_thinking_key", func(t *testing.T) {
		req := baseReq()
		req.Reasoning = ReasoningConfig{Level: LevelOff}
		req.Model.Caps.LevelMap = map[Level]*string{LevelOff: nil}

		m := decode(t, buildBody(t, req))
		assert.NotContains(t, m, "thinking")
	})
	t.Run("budget_shape_sends_summarized_display", func(t *testing.T) {
		req := baseReq()
		req.Reasoning = ReasoningConfig{Level: LevelMedium}

		m := decode(t, buildBody(t, req))
		think := m["thinking"].(map[string]any)
		assert.Equal(t, "enabled", think["type"])
		assert.Equal(t, "summarized", think["display"])
	})
	t.Run("non_reasoning_models_emit_no_thinking", func(t *testing.T) {
		req := baseReq()
		req.Reasoning = ReasoningConfig{Level: LevelOff}
		req.Model.Caps.Reasoning = false

		m := decode(t, buildBody(t, req))
		assert.NotContains(t, m, "thinking")
	})
}

func TestAnthropicHeaders(t *testing.T) {
	t.Parallel()

	baseReq := func() Request {
		return Request{
			Model:     anthropicModel(nil),
			Messages:  []Message{Text(RoleUser, "hi")},
			MaxTokens: 16000,
		}
	}

	t.Run("interleaved_by_default", func(t *testing.T) {
		headers := anthropicHeaders(baseReq())
		assert.Equal(t, betaInterleavedThinking, headers["anthropic-beta"])
	})
	t.Run("absent_on_adaptive_models", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.ForceAdaptiveThinking = true
		assert.NotContains(t, anthropicHeaders(req), "anthropic-beta")
	})
	t.Run("absent_on_non_reasoning_models", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.Reasoning = false
		assert.NotContains(t, anthropicHeaders(req), "anthropic-beta")
	})
	t.Run("fine_grained_only_without_eager_streaming", func(t *testing.T) {
		req := baseReq()
		req.Tools = []ToolSchema{{Name: "read"}}
		req.Model.Caps.EagerToolInputStreaming = false
		assert.Equal(t, betaFineGrainedTools+","+betaInterleavedThinking,
			anthropicHeaders(req)["anthropic-beta"])
	})
	t.Run("fine_grained_needs_tools", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.EagerToolInputStreaming = false
		assert.Equal(t, betaInterleavedThinking, anthropicHeaders(req)["anthropic-beta"])
	})
	t.Run("model_declared_beta_is_overridden", func(t *testing.T) {
		req := baseReq()
		req.Model.Headers = map[string]string{"anthropic-beta": "stale"}
		assert.Equal(t, betaInterleavedThinking, anthropicHeaders(req)["anthropic-beta"])
	})
	t.Run("model_headers_are_not_mutated", func(t *testing.T) {
		req := baseReq()
		req.Model.Headers = map[string]string{"X-Org": "acme"}
		headers := anthropicHeaders(req)
		assert.Equal(t, "acme", headers["X-Org"])
		assert.NotContains(t, req.Model.Headers, "anthropic-beta")
	})
	t.Run("beta_header_reaches_the_wire", func(t *testing.T) {
		srv, req := sseServer(t, "anthropic/text.sse")
		p := newAnthropicTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: anthropicModel(nil)})
		require.NoError(t, err)
		events := collect(t, s)
		require.NotEmpty(t, events)
		assert.Equal(t, betaInterleavedThinking, req.Header.Get("anthropic-beta"))
	})
}

func TestBuildAnthropicBodyToolsAndCache(t *testing.T) {
	t.Parallel()

	baseReq := func() Request {
		return Request{
			Model:     anthropicModel(nil),
			System:    BlockList{TextBlock{Text: "be terse"}},
			Messages:  []Message{Text(RoleUser, "hi")},
			MaxTokens: 16000,
			Tools:     []ToolSchema{{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}},
		}
	}
	decode := func(t *testing.T, body []byte) map[string]any {
		t.Helper()
		var m map[string]any
		require.NoError(t, json.Unmarshal(body, &m))
		return m
	}

	t.Run("eager_streaming_emitted_by_default", func(t *testing.T) {
		tools := decode(t, buildBody(t, baseReq()))["tools"].([]any)
		assert.Equal(t, true, tools[0].(map[string]any)["eager_input_streaming"])
	})
	t.Run("eager_streaming_absent_when_cleared", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.EagerToolInputStreaming = false

		tools := decode(t, buildBody(t, req))["tools"].([]any)
		assert.NotContains(t, tools[0].(map[string]any), "eager_input_streaming")
	})
	t.Run("tool_cache_marker_gated_on_capability", func(t *testing.T) {
		req := baseReq()
		req.Cache = CachePolicy{Enabled: true}
		req.Model.Caps.CacheControlOnTools = false

		m := decode(t, buildBody(t, req))
		tools := m["tools"].([]any)
		assert.NotContains(t, tools[0].(map[string]any), "cache_control")
		// the system breakpoint still lands
		system := m["system"].([]any)[0].(map[string]any)
		assert.Equal(t, "ephemeral", system["cache_control"].(map[string]any)["type"])
	})
}

func TestAnthropicEmptySignatureReplay(t *testing.T) {
	t.Parallel()

	build := func(t *testing.T, blocks BlockList, caps Capabilities) string {
		t.Helper()
		req := Request{
			Model:     anthropicModel(func(c *Capabilities) { *c = caps }),
			MaxTokens: 16000,
			Reasoning: ReasoningConfig{Retain: RetainAll},
		}
		req.Messages = sameOrigin(req.Model, []Message{
			Text(RoleUser, "q"), {Role: RoleAssistant, Content: blocks},
		})
		body, err := buildAnthropicBody(req)
		require.NoError(t, err)
		return string(body)
	}
	anthropic := func() Capabilities {
		caps := flavorDefaults[FlavorAnthropic].caps
		caps.Budgets = defaultBudgets(64000)
		return caps
	}

	t.Run("dropped_by_retention_by_default", func(t *testing.T) {
		body := build(t, BlockList{ThinkingBlock{Text: "orphaned"}}, anthropic())
		assert.NotContains(t, body, `"orphaned"`)
	})
	t.Run("demoted_to_text_when_it_reaches_blocks", func(t *testing.T) {
		// retention normally strips it first; if a block gets through it demotes
		// to visible text rather than vanishing
		blocks, err := anthropicBlocks(BlockList{ThinkingBlock{Text: "orphaned"}}, anthropic())
		require.NoError(t, err)
		require.Len(t, blocks, 1)
		assert.Equal(t, antTypeText, blocks[0].Type)
		assert.Equal(t, "orphaned", blocks[0].Text)
	})
	t.Run("kept_with_empty_signature_when_allowed", func(t *testing.T) {
		caps := anthropic()
		caps.AllowEmptySignature = true
		body := build(t, BlockList{ThinkingBlock{Text: "orphaned"}}, caps)
		assert.Contains(t, body, `"thinking":"orphaned"`)
		assert.Contains(t, body, `"signature":""`)
	})
	t.Run("blank_text_and_signature_dropped", func(t *testing.T) {
		caps := anthropic()
		caps.AllowEmptySignature = true
		body := build(t, BlockList{
			ThinkingBlock{Text: "", Signature: ""},
			TextBlock{Text: "answer"},
		}, caps)
		assert.NotContains(t, body, `"type":"thinking"`)
		assert.Contains(t, body, `"answer"`)
	})
	t.Run("signed_block_replays_even_without_text", func(t *testing.T) {
		body := build(t, BlockList{ThinkingBlock{Signature: "sig"}}, anthropic())
		assert.Contains(t, body, `"signature":"sig"`)
	})
}

func TestLevelBudget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		level     Level
		maxOutput int
		expected  int
	}{
		{"off_is_zero", LevelOff, 64000, 0},
		{"minimal", LevelMinimal, 64000, 1024},
		{"low", LevelLow, 64000, 2048},
		{"medium", LevelMedium, 64000, 8192},
		{"high", LevelHigh, 64000, 16384},
		{"xhigh", LevelXHigh, 64000, 32768},
		{"max", LevelMax, 128000, 64000},
		{"clamped_to_max_output", LevelMax, 8000, 7999},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, levelBudget(tc.level, tc.maxOutput))
		})
	}
}

func TestThinkingBudget(t *testing.T) {
	t.Parallel()

	req := func(level Level, budget, maxOut int) Request {
		caps := Capabilities{Dialect: DialectAnthropic,
			Thinking: ThinkingAnthropic, Reasoning: true, Budgets: defaultBudgets(maxOut)}
		return Request{
			Model:     Model{MaxOutput: maxOut, Caps: caps},
			Reasoning: ReasoningConfig{Level: level, Budget: budget},
		}
	}

	t.Run("raises_below_the_api_minimum", func(t *testing.T) {
		r := req(LevelLow, 0, 64)
		assert.Equal(t, minThinkingBudget,
			thinkingBudget(r.Model.Caps, LevelMinimal, r.Reasoning.Budget, 4000))

		r2 := req(LevelLow, 10, 100000)
		assert.Equal(t, minThinkingBudget,
			thinkingBudget(r2.Model.Caps, LevelLow, r2.Reasoning.Budget, 64000))
	})
	t.Run("leaves_room_for_the_reply", func(t *testing.T) {
		// an explicit budget bigger than the ceiling clamps so minAnswerTokens stay
		r := req(LevelHigh, 8000, 100000)
		const maxTok = 4000
		assert.Equal(t, maxTok-minAnswerTokens,
			thinkingBudget(r.Model.Caps, LevelHigh, r.Reasoning.Budget, maxTok))
	})
	t.Run("gives_up_when_max_tokens_is_too_small", func(t *testing.T) {
		// ceiling falls under the API minimum once the answer floor is kept
		r := req(LevelHigh, 8000, 100000)
		assert.Zero(t,
			thinkingBudget(r.Model.Caps, LevelHigh, r.Reasoning.Budget, minAnswerTokens+minThinkingBudget-1))
	})
	t.Run("off_is_zero", func(t *testing.T) {
		r := req(LevelOff, 0, 100000)
		assert.Zero(t,
			thinkingBudget(r.Model.Caps, LevelOff, r.Reasoning.Budget, 64000))
	})
	t.Run("non_budget_styles_are_zero", func(t *testing.T) {
		r := req(LevelHigh, 0, 64)
		r.Model.Caps.Dialect = DialectOpenAICompletions
		assert.Zero(t,
			thinkingBudget(r.Model.Caps, LevelLow, r.Reasoning.Budget, 64000))
	})
	t.Run("unsupported_level_clamps_down", func(t *testing.T) {
		// xhigh is opt-in; without a map entry it clamps to high before the budget lookup
		r := req(LevelXHigh, 0, 8000)
		assert.Equal(t, r.Model.Caps.Budgets[LevelHigh],
			thinkingBudget(r.Model.Caps, LevelHigh, r.Reasoning.Budget, 64000))
	})
}

func TestAnthropicClassifier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		body     string
		overflow bool
	}{
		{"prompt_too_long", 400,
			`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 250000 tokens"}}`, true},
		{"exceeds_context_limit", 400,
			`{"error":{"type":"invalid_request_error","message":"input exceed context limit"}}`, true},
		{"unrelated_400", 400,
			`{"error":{"type":"invalid_request_error","message":"model not found"}}`, false},
		{"overloaded_is_not_overflow", 529, `{"error":{"type":"overloaded_error","message":"Overloaded"}}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := anthropicClassifier("anthropic")(tc.status, []byte(tc.body))
			assert.Equal(t, tc.overflow, IsOverflow(err))
		})
	}

	t.Run("extracts_type_as_the_code", func(t *testing.T) {
		err := anthropicClassifier("anthropic")(400,
			[]byte(`{"error":{"type":"invalid_request_error","message":"bad"}}`))

		var ae *APIError
		require.ErrorAs(t, err, &ae)
		assert.Equal(t, "invalid_request_error", ae.Code)
		assert.Equal(t, "bad", ae.Message)
	})
}

func TestAnthropicProviderCountTokens(t *testing.T) {
	t.Parallel()

	srv, req := jsonServer(t, "anthropic/count_tokens.json")
	p := newAnthropicTestProvider(t, srv.URL)

	got, err := p.CountTokens(t.Context(), Request{
		Model:    anthropicModel(nil),
		Messages: []Message{Text(RoleUser, "hi")},
	})
	require.NoError(t, err)
	assert.Equal(t, 1234, got)
	assert.Equal(t, "/v1/messages/count_tokens", req.Path)

	// the endpoint rejects the streaming and sampling fields
	var sent map[string]any
	require.NoError(t, json.Unmarshal(req.Body, &sent))
	assert.NotContains(t, sent, "stream")
	assert.NotContains(t, sent, "max_tokens")
}

// TestCompactionSummaryReachesTheModel guards that a compaction summary
// injected as a user message must survive the adapter,
// whereas a system-role message is dropped (system is a top-level field).
func TestCompactionSummaryReachesTheModel(t *testing.T) {
	t.Parallel()

	summary := Text(RoleUser, "The conversation history before this point was compacted:\n<summary>\nthe goal\n</summary>")
	req := Request{Model: anthropicModel(nil), Messages: []Message{summary, Text(RoleUser, "next")}}

	msgs, err := anthropicMessages(req, req.Model.Caps)
	require.NoError(t, err)

	var sb strings.Builder
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
	}
	assert.Contains(t, sb.String(), "<summary>") // the summary survives as a user message

	// the same text as a system message would be dropped entirely.
	sysReq := Request{Model: anthropicModel(nil), Messages: []Message{Text(RoleSystem, "<summary>lost</summary>")}}
	sysMsgs, err := anthropicMessages(sysReq, sysReq.Model.Caps)
	require.NoError(t, err)
	assert.Empty(t, sysMsgs)
}
