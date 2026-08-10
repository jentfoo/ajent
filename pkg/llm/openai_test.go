package llm

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// responsesModel returns an OpenAI model with optional capability tweaks.
func responsesModel(fn func(*Capabilities)) Model {
	caps := flavorDefaults[FlavorOpenAI].caps
	if fn != nil {
		fn(&caps)
	}
	return Model{Provider: "openai", ID: "gpt-5", ContextWindow: 400000, MaxOutput: 100000, Caps: caps}
}

func newResponsesTestProvider(t *testing.T, url string) *responsesProvider {
	t.Helper()

	c, err := newHTTPClient(clientOptions{provider: "openai", baseURL: url})
	require.NoError(t, err)
	return newResponsesProvider("openai", c)
}

func TestResponsesProviderStream(t *testing.T) {
	t.Parallel()

	t.Run("text_only", func(t *testing.T) {
		srv, req := sseServer(t, "openai/text.sse")
		p := newResponsesTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: responsesModel(nil)})
		require.NoError(t, err)
		events := collect(t, s)

		assert.Equal(t, "/responses", req.Path)
		assert.Equal(t, "Hello there", textOf(events))
		assert.Equal(t, []string{
			"message_start", "text_start", "text_delta", "text_delta",
			"text_end", "usage", "done",
		}, eventKinds(events))
	})
	t.Run("usage_splits_cached_input", func(t *testing.T) {
		srv, _ := sseServer(t, "openai/text.sse")
		p := newResponsesTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: responsesModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		_, usage, err := Accumulate(s)
		require.NoError(t, err)
		assert.Equal(t, Usage{Input: 40, Output: 7, CacheRead: 10}, usage)
	})
	t.Run("reasoning_carries_its_replay_tokens", func(t *testing.T) {
		srv, _ := sseServer(t, "openai/encrypted_reasoning.sse")
		p := newResponsesTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: responsesModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		msg, usage, err := Accumulate(s)
		require.NoError(t, err)
		require.Len(t, msg.Content, 2)

		think, ok := msg.Content[0].(ThinkingBlock)
		require.True(t, ok)
		assert.Equal(t, "weighing options", think.Text)
		assert.Equal(t, "rs_abc", think.ItemID)
		assert.Equal(t, "ENCRYPTED-PAYLOAD", think.Encrypted)
		assert.Equal(t, TextBlock{Text: "the answer"}, msg.Content[1])
		assert.Equal(t, 25, usage.Reasoning)
	})
	t.Run("reasoning_round_trips_into_the_next_request", func(t *testing.T) {
		// without the item id and encrypted payload a stateless replay is
		// rejected, which is what breaks multi turn reasoning
		srv, _ := sseServer(t, "openai/encrypted_reasoning.sse")
		p := newResponsesTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: responsesModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		msg, _, err := Accumulate(s)
		require.NoError(t, err)

		body, err := buildResponsesBody(Request{
			Model:     responsesModel(nil),
			Messages:  []Message{Text(RoleUser, "q"), msg},
			Reasoning: ReasoningConfig{Retain: RetainAll},
		})
		require.NoError(t, err)
		assert.Contains(t, string(body), `"id":"rs_abc"`)
		assert.Contains(t, string(body), `"encrypted_content":"ENCRYPTED-PAYLOAD"`)
	})
	t.Run("tool_call_arguments_split", func(t *testing.T) {
		srv, _ := sseServerChunked(t, "openai/tool_call.sse", 19)
		p := newResponsesTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: responsesModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		msg, _, err := Accumulate(s)
		require.NoError(t, err)
		require.Len(t, msg.Content, 1)

		call, ok := msg.Content[0].(ToolCallBlock)
		require.True(t, ok)
		assert.Equal(t, "call_abc", call.ID) // the call id, not the item id
		assert.Equal(t, "read", call.Name)
		assert.JSONEq(t, `{"path":"main.go"}`, string(call.Input))
	})
	t.Run("tool_call_sets_the_stop_reason", func(t *testing.T) {
		srv, _ := sseServer(t, "openai/tool_call.sse")
		p := newResponsesTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: responsesModel(nil)})
		require.NoError(t, err)
		events := collect(t, s)

		assert.Equal(t, StopToolUse, events[len(events)-1].StopReason)
	})
	t.Run("failure_surfaces_the_partial", func(t *testing.T) {
		srv, _ := sseServer(t, "openai/failed.sse")
		p := newResponsesTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: responsesModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		var events []Event
		for ev, ok := s.Next(); ok; ev, ok = s.Next() {
			events = append(events, ev)
		}
		require.Error(t, s.Err())
		assert.Equal(t, "partial", textOf(events))
		assert.Equal(t, StopError, events[len(events)-1].StopReason)
		assert.ErrorContains(t, s.Err(), "upstream exploded")
	})
	t.Run("close_mid_stream_is_not_an_error", func(t *testing.T) {
		srv, _ := sseServer(t, "openai/text.sse")
		p := newResponsesTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: responsesModel(nil)})
		require.NoError(t, err)

		_, ok := s.Next()
		require.True(t, ok)
		require.NoError(t, s.Close())

		_, ok = s.Next()
		assert.False(t, ok)
		assert.NoError(t, s.Err())
	})
	t.Run("falls_back_to_chat_completions", func(t *testing.T) {
		// the dialect is chosen per model from the resolved capabilities
		srv, req := sseServer(t, "compat/text.sse")
		p := newResponsesTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{
			Model: responsesModel(func(c *Capabilities) { c.MaxTokensField = fieldMaxCompletion }),
		})
		require.NoError(t, err)
		events := collect(t, s)

		assert.Equal(t, "/chat/completions", req.Path)
		assert.Equal(t, "Hello world", textOf(events))
	})
}

func TestBuildResponsesBody(t *testing.T) {
	t.Parallel()

	baseReq := func() Request {
		return Request{
			Model:     responsesModel(nil),
			System:    BlockList{TextBlock{Text: "be terse"}},
			Messages:  []Message{Text(RoleUser, "hi")},
			MaxTokens: 1000,
		}
	}
	decode := func(t *testing.T, body []byte) map[string]any {
		t.Helper()
		var m map[string]any
		require.NoError(t, json.Unmarshal(body, &m))
		return m
	}

	t.Run("system_becomes_instructions", func(t *testing.T) {
		body, err := buildResponsesBody(baseReq())
		require.NoError(t, err)
		assert.JSONEq(t, `{
			"model": "gpt-5",
			"stream": true,
			"store": false,
			"max_output_tokens": 1000,
			"instructions": "be terse",
			"input": [
				{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi"}]}
			]
		}`, string(body))
	})
	t.Run("assistant_text_uses_the_output_type", func(t *testing.T) {
		req := baseReq()
		req.Messages = []Message{Text(RoleUser, "q"), Text(RoleAssistant, "a")}

		body, err := buildResponsesBody(req)
		require.NoError(t, err)
		assert.Contains(t, string(body), `"type":"output_text"`)
	})
	t.Run("tools_are_flat_not_nested", func(t *testing.T) {
		req := baseReq()
		req.Tools = []ToolSchema{{Name: "read", Description: "read a file",
			Parameters: json.RawMessage(`{"type":"object"}`)}}

		body, err := buildResponsesBody(req)
		require.NoError(t, err)

		tools := decode(t, body)["tools"].([]any)
		require.Len(t, tools, 1)
		tool := tools[0].(map[string]any)
		assert.Equal(t, "function", tool["type"])
		assert.Equal(t, "read", tool["name"]) // not under a "function" key
		assert.NotContains(t, tool, "function")
	})
	t.Run("reasoning_effort_and_encrypted_include", func(t *testing.T) {
		req := baseReq()
		req.Reasoning = ReasoningConfig{Level: LevelHigh}

		body, err := buildResponsesBody(req)
		require.NoError(t, err)

		m := decode(t, body)
		reasoning := m["reasoning"].(map[string]any)
		assert.Equal(t, "high", reasoning["effort"])
		assert.Equal(t, "auto", reasoning["summary"])
		assert.Equal(t, []any{respEncryptedInclude}, m["include"])
		assert.Equal(t, false, m["store"])
	})
	t.Run("no_include_without_reasoning", func(t *testing.T) {
		body, err := buildResponsesBody(baseReq())
		require.NoError(t, err)
		assert.NotContains(t, decode(t, body), "include")
	})
	t.Run("stored_responses_skip_the_include", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.Store = true
		req.Reasoning = ReasoningConfig{Level: LevelMedium}

		body, err := buildResponsesBody(req)
		require.NoError(t, err)

		m := decode(t, body)
		assert.NotContains(t, m, "include")
		assert.NotContains(t, m, "store")
	})
	t.Run("level_off_omits_reasoning", func(t *testing.T) {
		req := baseReq()
		req.Reasoning = ReasoningConfig{Level: LevelOff}

		body, err := buildResponsesBody(req)
		require.NoError(t, err)
		assert.NotContains(t, decode(t, body), "reasoning")
	})
	t.Run("tool_calls_and_results_become_items", func(t *testing.T) {
		req := baseReq()
		req.Messages = []Message{
			Text(RoleUser, "read it"),
			{Role: RoleAssistant, Content: BlockList{
				ToolCallBlock{ID: "call_1", Name: "read", Input: json.RawMessage(`{"p":1}`)},
			}},
			{Role: RoleTool, Content: BlockList{
				ToolResultBlock{CallID: "call_1", Content: BlockList{TextBlock{Text: "body"}}},
			}},
		}
		body, err := buildResponsesBody(req)
		require.NoError(t, err)

		input := decode(t, body)["input"].([]any)
		require.Len(t, input, 3)

		call := input[1].(map[string]any)
		assert.Equal(t, "function_call", call["type"])
		assert.Equal(t, "call_1", call["call_id"])

		result := input[2].(map[string]any)
		assert.Equal(t, "function_call_output", result["type"])
		assert.Equal(t, "body", result["output"])
	})
	t.Run("thinking_without_an_item_id_is_dropped", func(t *testing.T) {
		req := baseReq()
		req.Reasoning.Retain = RetainAll
		req.Messages = []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{
				ThinkingBlock{Text: "unreferencable"},
				TextBlock{Text: "answer"},
			}},
		}
		body, err := buildResponsesBody(req)
		require.NoError(t, err)
		assert.NotContains(t, string(body), "unreferencable")
	})
	t.Run("image_becomes_an_input_image", func(t *testing.T) {
		req := baseReq()
		req.Messages = []Message{{Role: RoleUser, Content: BlockList{
			ImageBlock{MediaType: "image/png", Data: []byte{1, 2, 3}},
		}}}
		body, err := buildResponsesBody(req)
		require.NoError(t, err)
		assert.Contains(t, string(body), `"type":"input_image"`)
		assert.Contains(t, string(body), "data:image/png;base64,AQID")
	})
	t.Run("image_rejected_when_unsupported", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.Images = false
		req.Messages = []Message{{Role: RoleUser, Content: BlockList{
			ImageBlock{MediaType: "image/png", Data: []byte{1}},
		}}}

		_, err := buildResponsesBody(req)
		assert.ErrorContains(t, err, "image")
	})
	t.Run("tool_choice_modes", func(t *testing.T) {
		tests := []struct {
			name     string
			choice   ToolChoice
			expected any
		}{
			{"auto_is_omitted", ToolChoice{Mode: ToolChoiceAuto}, nil},
			{"none", ToolChoice{Mode: ToolChoiceNone}, "none"},
			{"required", ToolChoice{Mode: ToolChoiceRequired}, "required"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				req := baseReq()
				req.Tools = []ToolSchema{{Name: "read"}}
				req.ToolChoice = tc.choice

				body, err := buildResponsesBody(req)
				require.NoError(t, err)

				m := decode(t, body)
				if tc.expected == nil {
					assert.NotContains(t, m, "tool_choice")
					return
				}
				assert.Equal(t, tc.expected, m["tool_choice"])
			})
		}
	})
}

func TestRespStopReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   string
		sawTool  bool
		expected StopReason
	}{
		{"completed", "completed", false, StopEndTurn},
		{"completed_with_tools", "completed", true, StopToolUse},
		{"incomplete_is_max_tokens", "incomplete", false, StopMaxTokens},
		{"failed", "failed", false, StopError},
		{"unknown_defaults_to_end_turn", "", false, StopEndTurn},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, respStopReason(tc.status, tc.sawTool))
		})
	}
}
