package llm

import (
	"encoding/json"
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
	return Model{Provider: "anthropic", ID: "claude-opus-4-5",
		ContextWindow: 200000, MaxOutput: 64000, Caps: caps}
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
	t.Run("thinking_dropped_when_max_tokens_cannot_fit_it", func(t *testing.T) {
		// the budget must leave room for a reply, and below the API minimum
		// there is no point asking for reasoning at all
		req := baseReq()
		req.MaxTokens = 1000
		req.Reasoning = ReasoningConfig{Level: LevelMedium}

		body, err := buildAnthropicBody(req)
		require.NoError(t, err)
		assert.NotContains(t, decode(t, body), "thinking")
	})
	t.Run("no_thinking_at_level_off", func(t *testing.T) {
		req := baseReq()
		req.Reasoning = ReasoningConfig{Level: LevelOff}

		body, err := buildAnthropicBody(req)
		require.NoError(t, err)
		assert.NotContains(t, decode(t, body), "thinking")
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
		req.Messages = []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{
				ThinkingBlock{Text: "because", Signature: "sig"},
				TextBlock{Text: "answer"},
			}},
		}
		body, err := buildAnthropicBody(req)
		require.NoError(t, err)
		assert.Contains(t, string(body), `"signature":"sig"`)
		assert.Contains(t, string(body), `"thinking":"because"`)
	})
	t.Run("unsigned_thinking_is_not_replayed", func(t *testing.T) {
		req := baseReq()
		req.Reasoning.Retain = RetainAll
		req.Messages = []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{
				ThinkingBlock{Text: "no signature"},
				TextBlock{Text: "answer"},
			}},
		}
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
		return Request{
			Model:     Model{MaxOutput: maxOut, Caps: Capabilities{Reasoning: ReasoningAnthropicBudget}},
			Reasoning: ReasoningConfig{Level: level, Budget: budget},
		}
	}

	t.Run("raises_below_the_api_minimum", func(t *testing.T) {
		assert.Equal(t, minThinkingBudget, thinkingBudget(req(LevelLow, 10, 64000), 64000))
	})
	t.Run("leaves_room_for_the_reply", func(t *testing.T) {
		assert.Equal(t, 3999, thinkingBudget(req(LevelHigh, 8000, 64000), 4000))
	})
	t.Run("gives_up_when_max_tokens_is_too_small", func(t *testing.T) {
		assert.Zero(t, thinkingBudget(req(LevelHigh, 0, 64000), 500))
	})
	t.Run("off_is_zero", func(t *testing.T) {
		assert.Zero(t, thinkingBudget(req(LevelOff, 0, 64000), 64000))
	})
	t.Run("non_budget_styles_are_zero", func(t *testing.T) {
		r := req(LevelHigh, 0, 64000)
		r.Model.Caps.Reasoning = ReasoningOpenAIEffort
		assert.Zero(t, thinkingBudget(r, 64000))
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
