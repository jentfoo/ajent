package llm

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// compatModel is a chat-completions model with the given capability overrides.
func compatModel(fn func(*Capabilities)) Model {
	caps := flavorDefaults[FlavorLMStudio].caps
	caps.Reasoning = ReasoningContentField // the common case, not inline tags
	if fn != nil {
		fn(&caps)
	}
	return Model{Provider: "lmstudio", ID: "m1", ContextWindow: 8192, MaxOutput: 512, Caps: caps}
}

// newCompatTestProvider builds a compat provider pointed at a test server.
func newCompatTestProvider(t *testing.T, url string) *compatProvider {
	t.Helper()

	c, err := newHTTPClient(clientOptions{provider: "lmstudio", baseURL: url})
	require.NoError(t, err)
	return &compatProvider{client: c, profile: compatProfile{
		name: "lmstudio", classify: compatClassifier("lmstudio", FlavorLMStudio),
	}}
}

func TestCompatProviderStream(t *testing.T) {
	t.Parallel()

	t.Run("text_only", func(t *testing.T) {
		srv, _ := sseServer(t, "compat/text.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: compatModel(nil)})
		require.NoError(t, err)
		events := collect(t, s)

		assert.Equal(t, []string{
			"message_start", "text_start", "text_delta", "text_delta", "usage", "text_end", "done",
		}, eventKinds(events))
		assert.Equal(t, "Hello world", textOf(events))
	})
	t.Run("accumulates_to_a_message", func(t *testing.T) {
		srv, _ := sseServer(t, "compat/text.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: compatModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		msg, usage, err := Accumulate(s)
		require.NoError(t, err)
		assert.Equal(t, BlockList{TextBlock{Text: "Hello world"}}, msg.Content)
		assert.Equal(t, Usage{Input: 12, Output: 3}, usage)
	})
	t.Run("reasoning_content_field", func(t *testing.T) {
		srv, _ := sseServer(t, "compat/reasoning_content.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: compatModel(nil)})
		require.NoError(t, err)
		events := collect(t, s)

		assert.Equal(t, "let me think", thinkingOf(events))
		assert.Equal(t, "answer", textOf(events))
		assert.Contains(t, eventKinds(events), "thinking_end")
	})
	t.Run("think_tags_split_across_deltas", func(t *testing.T) {
		srv, _ := sseServer(t, "compat/think_tags.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{
			Model: compatModel(func(c *Capabilities) { c.Reasoning = ReasoningInlineTags }),
		})
		require.NoError(t, err)
		events := collect(t, s)

		assert.Equal(t, "pondering", thinkingOf(events))
		assert.Equal(t, "the answer", textOf(events)) // no leaked tag fragment
	})
	t.Run("tool_arguments_split_mid_token", func(t *testing.T) {
		// 17 bytes is coprime with the frame lengths, so a write boundary lands
		// inside a JSON escape and proves reassembly happens before decoding
		srv, _ := sseServerChunked(t, "compat/tool_args_split.sse", 17)
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: compatModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		msg, usage, err := Accumulate(s)
		require.NoError(t, err)
		require.Len(t, msg.Content, 1)

		call, ok := msg.Content[0].(ToolCallBlock)
		require.True(t, ok)
		assert.Equal(t, "call_1", call.ID)
		assert.Equal(t, "read", call.Name)
		assert.JSONEq(t, `{"path": "main.go"}`, string(call.Input))
		assert.Equal(t, Usage{Input: 120, Output: 18}, usage)
	})
	t.Run("parallel_tool_calls", func(t *testing.T) {
		srv, _ := sseServer(t, "compat/parallel_tools.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: compatModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		msg, _, err := Accumulate(s)
		require.NoError(t, err)
		require.Len(t, msg.Content, 2)
		assert.Equal(t, "call_1", msg.Content[0].(ToolCallBlock).ID)
		assert.Equal(t, "call_2", msg.Content[1].(ToolCallBlock).ID)
	})
	t.Run("stop_reason_tool_use", func(t *testing.T) {
		srv, _ := sseServer(t, "compat/parallel_tools.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: compatModel(nil)})
		require.NoError(t, err)
		events := collect(t, s)

		last := events[len(events)-1]
		assert.Equal(t, EventDone, last.Type)
		assert.Equal(t, StopToolUse, last.StopReason)
	})
	t.Run("usage_only_final_frame", func(t *testing.T) {
		srv, _ := sseServer(t, "compat/usage_only.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: compatModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		_, usage, err := Accumulate(s)
		require.NoError(t, err)
		assert.Equal(t, Usage{Input: 10, CacheRead: 40}, usage)
	})
	t.Run("mid_stream_error_surfaces_the_partial", func(t *testing.T) {
		srv, _ := sseServer(t, "compat/error_midstream.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: compatModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		var events []Event
		for ev, ok := s.Next(); ok; ev, ok = s.Next() {
			events = append(events, ev)
		}
		require.Error(t, s.Err())

		assert.Equal(t, "partial", textOf(events)) // what arrived is still delivered
		last := events[len(events)-1]
		assert.Equal(t, EventDone, last.Type)
		assert.Equal(t, StopError, last.StopReason)
		assert.Error(t, last.Err)
	})
	t.Run("close_mid_stream_is_not_an_error", func(t *testing.T) {
		srv, _ := sseServer(t, "compat/text.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: compatModel(nil)})
		require.NoError(t, err)

		_, ok := s.Next()
		require.True(t, ok)
		require.NoError(t, s.Close())

		_, ok = s.Next()
		assert.False(t, ok)
		assert.NoError(t, s.Err()) // a deliberate close is not a failure
	})
}

func TestBuildCompatBody(t *testing.T) {
	t.Parallel()

	baseReq := func() Request {
		return Request{
			Model:     compatModel(nil),
			System:    BlockList{TextBlock{Text: "be terse"}},
			Messages:  []Message{Text(RoleUser, "hi")},
			MaxTokens: 100,
		}
	}
	decode := func(t *testing.T, body []byte) map[string]any {
		t.Helper()
		var m map[string]any
		require.NoError(t, json.Unmarshal(body, &m))
		return m
	}

	t.Run("basic_shape", func(t *testing.T) {
		body, err := buildCompatBody(baseReq(), compatProfile{})
		require.NoError(t, err)
		assert.JSONEq(t, `{
			"model": "m1",
			"stream": true,
			"stream_options": {"include_usage": true},
			"max_tokens": 100,
			"messages": [
				{"role": "system", "content": "be terse"},
				{"role": "user", "content": "hi"}
			]
		}`, string(body))
	})
	t.Run("max_completion_tokens_field", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.MaxTokensField = fieldMaxCompletion

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)

		m := decode(t, body)
		assert.InDelta(t, 100, m["max_completion_tokens"], 0.001)
		assert.NotContains(t, m, "max_tokens")
	})
	t.Run("developer_role_when_supported", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.DeveloperRole = true

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.Contains(t, string(body), `"role":"developer"`)
	})
	t.Run("temperature_omitted_when_unsupported", func(t *testing.T) {
		req := baseReq()
		temp := 0.7
		req.Temperature = &temp
		req.Model.Caps.Temperature = false

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.NotContains(t, decode(t, body), "temperature")
	})
	t.Run("temperature_sent_when_supported", func(t *testing.T) {
		req := baseReq()
		temp := 0.7
		req.Temperature = &temp

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.InDelta(t, 0.7, decode(t, body)["temperature"], 0.001)
	})
	t.Run("stream_usage_omitted_when_unsupported", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.StreamUsage = false

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.NotContains(t, decode(t, body), "stream_options")
	})
	t.Run("tools_and_choice", func(t *testing.T) {
		req := baseReq()
		req.Tools = []ToolSchema{{Name: "read", Description: "read a file",
			Parameters: json.RawMessage(`{"type":"object"}`)}}
		req.ToolChoice = ToolChoice{Mode: ToolChoiceRequired}

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)

		m := decode(t, body)
		assert.Equal(t, "required", m["tool_choice"])
		tools := m["tools"].([]any)
		require.Len(t, tools, 1)
		assert.Equal(t, "function", tools[0].(map[string]any)["type"])
	})
	t.Run("specific_tool_choice", func(t *testing.T) {
		req := baseReq()
		req.Tools = []ToolSchema{{Name: "read"}}
		req.ToolChoice = ToolChoice{Mode: ToolChoiceSpecific, Name: "read"}

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.Contains(t, string(body), `"tool_choice":{"function":{"name":"read"},"type":"function"}`)
	})
	t.Run("tool_results_become_tool_messages", func(t *testing.T) {
		req := baseReq()
		req.Messages = []Message{
			Text(RoleUser, "read it"),
			{Role: RoleAssistant, Content: BlockList{
				ToolCallBlock{ID: "c1", Name: "read", Input: json.RawMessage(`{}`)},
			}},
			{Role: RoleUser, Content: BlockList{
				ToolResultBlock{CallID: "c1", Content: BlockList{TextBlock{Text: "file body"}}},
			}},
		}
		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)

		msgs := decode(t, body)["messages"].([]any)
		last := msgs[len(msgs)-1].(map[string]any)
		assert.Equal(t, "tool", last["role"])
		assert.Equal(t, "c1", last["tool_call_id"])
		assert.Equal(t, "file body", last["content"])
	})
	t.Run("reasoning_effort_for_effort_models", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.Reasoning = ReasoningOpenAIEffort
		req.Reasoning = ReasoningConfig{Level: LevelHigh}

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.Equal(t, "high", decode(t, body)["reasoning_effort"])
	})
	t.Run("level_map_translates_the_effort", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.Reasoning = ReasoningOpenAIEffort
		req.Model.Caps.LevelMap = map[Level]*string{LevelXHigh: ptr("high")}
		req.Reasoning = ReasoningConfig{Level: LevelXHigh}

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.Equal(t, "high", decode(t, body)["reasoning_effort"])
	})
	t.Run("null_level_map_entry_omits_the_parameter", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.Reasoning = ReasoningOpenAIEffort
		req.Model.Caps.LevelMap = map[Level]*string{LevelOff: nil}
		req.Reasoning = ReasoningConfig{Level: LevelOff}

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.NotContains(t, decode(t, body), "reasoning_effort")
	})
	t.Run("extra_body_is_merged", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.ExtraBody = map[string]json.RawMessage{
			"top_k": json.RawMessage(`40`),
		}
		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.InDelta(t, 40, decode(t, body)["top_k"], 0.001)
	})
	t.Run("image_content_becomes_parts", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.Images = true
		req.Messages = []Message{{Role: RoleUser, Content: BlockList{
			TextBlock{Text: "what is this"},
			ImageBlock{MediaType: "image/png", Data: []byte{1, 2, 3}},
		}}}

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.Contains(t, string(body), `"type":"image_url"`)
		assert.Contains(t, string(body), "data:image/png;base64,AQID")
	})
	t.Run("image_rejected_when_unsupported", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.Images = false
		req.Messages = []Message{{Role: RoleUser, Content: BlockList{
			ImageBlock{MediaType: "image/png", Data: []byte{1}},
		}}}

		_, err := buildCompatBody(req, compatProfile{})
		assert.ErrorContains(t, err, "image")
	})
	t.Run("reasoning_content_replayed_when_required", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.ReplayReasoning = true
		req.Reasoning.Retain = RetainAll
		req.Messages = []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{
				ThinkingBlock{Text: "because"},
				TextBlock{Text: "answer"},
			}},
		}
		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.Contains(t, string(body), `"reasoning_content":"because"`)
	})
	t.Run("empty_reasoning_content_forced_on_assistant", func(t *testing.T) {
		// deepseek requires every replayed assistant message to carry the field,
		// even a plain reply with no thinking block.
		req := baseReq()
		req.Model.Caps.ReplayReasoning = true
		req.Reasoning.Retain = RetainAll
		req.Messages = []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{TextBlock{Text: "plain reply"}}},
		}
		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.Contains(t, string(body), `"reasoning_content":""`)
	})
	t.Run("empty_assistant_message_skipped", func(t *testing.T) {
		// neither content nor tool calls: dropped regardless of reasoning fields
		req := baseReq()
		req.Model.Caps.ReplayReasoning = true
		req.Reasoning.Retain = RetainAll
		req.Messages = []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{ThinkingBlock{Text: "because"}}},
		}
		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.NotContains(t, string(body), `reasoning_content`)
	})
	t.Run("profile_decorator_runs", func(t *testing.T) {
		body, err := buildCompatBody(baseReq(), compatProfile{
			decorate: func(b *compatRequest, _ Request) { b.CachePrompt = ptr(true) },
		})
		require.NoError(t, err)
		assert.Equal(t, true, decode(t, body)["cache_prompt"])
	})
}

func TestCompatClassifier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		flavor   Flavor
		status   int
		body     string
		overflow bool
	}{
		{"lmstudio_context_length", FlavorLMStudio, 400,
			`{"error":{"message":"the context length is exceeded"}}`, true},
		{"llamacpp_500_n_ctx", FlavorLlamaCpp, 500,
			`{"error":{"message":"the request exceeds n_ctx"}}`, true},
		{"openai_code", FlavorOpenAI, 400,
			`{"error":{"message":"too big","code":"context_length_exceeded"}}`, true},
		{"openrouter_metadata", FlavorOpenRouter, 400,
			`{"error":{"message":"upstream","metadata":{"raw":"maximum context length"}}}`, true},
		{"unrelated_400_is_not_overflow", FlavorLMStudio, 400,
			`{"error":{"message":"unknown model"}}`, false},
		{"llamacpp_500_without_ctx_is_not_overflow", FlavorLlamaCpp, 500,
			`{"error":{"message":"slot unavailable"}}`, false},
		{"429_is_not_overflow", FlavorOpenRouter, 429, `{"error":{"message":"slow down"}}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := compatClassifier("p", tc.flavor)(tc.status, []byte(tc.body))
			assert.Equal(t, tc.overflow, IsOverflow(err))
		})
	}

	t.Run("retryable_status_is_marked", func(t *testing.T) {
		err := compatClassifier("p", FlavorOpenRouter)(503, []byte(`{"error":{"message":"down"}}`))
		assert.True(t, IsRetryable(err))
	})
	t.Run("message_extracted_from_the_envelope", func(t *testing.T) {
		err := compatClassifier("p", FlavorOpenAI)(400, []byte(`{"error":{"message":"bad thing"}}`))
		assert.Contains(t, err.Error(), "bad thing")
	})
	t.Run("plain_body_is_kept", func(t *testing.T) {
		err := compatClassifier("p", FlavorOpenAI)(500, []byte(`internal failure`))
		assert.Contains(t, err.Error(), "internal failure")
	})
}
