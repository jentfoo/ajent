package llm

import (
	"encoding/json"
	"strings"
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
		assert.JSONEq(t, `{"type":"reasoning","id":"rs_abc","encrypted_content":"ENCRYPTED-PAYLOAD","summary":[]}`,
			string(think.Item))
		assert.Equal(t, TextBlock{Text: "the answer", Signature: encodeTextSignature("msg_02", "")}, msg.Content[1])
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

		m := responsesModel(nil)
		body, err := buildResponsesBody(Request{
			Model:     m,
			Messages:  sameOrigin(m, []Message{Text(RoleUser, "q"), msg}),
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
		assert.Equal(t, "call_abc|fc_1", call.ID) // call id piped with the item id
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
	t.Run("reasoning_text_and_summary_separator", func(t *testing.T) {
		srv, _ := sseServer(t, "openai/reasoning_text.sse")
		p := newResponsesTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: responsesModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		msg, _, err := Accumulate(s)
		require.NoError(t, err)
		require.Len(t, msg.Content, 1)
		think, ok := msg.Content[0].(ThinkingBlock)
		require.True(t, ok)
		// inline reasoning_text deltas feed the thinking block; summary_part.done
		// inserts a separator between them
		assert.Equal(t, "step one \n\nstep two", think.Text)
	})
	t.Run("encrypted_payload_backfilled_from_completed", func(t *testing.T) {
		srv, _ := sseServer(t, "openai/encrypted_backfill.sse")
		p := newResponsesTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: responsesModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		msg, _, err := Accumulate(s)
		require.NoError(t, err)
		require.Len(t, msg.Content, 1)
		think, ok := msg.Content[0].(ThinkingBlock)
		require.True(t, ok)
		assert.Equal(t, "pondering ", think.Text)
		// the done event carried no payload; response.completed supplied it and
		// the accumulator replaced the earlier block in place
		assert.Equal(t, "ENC-LATE", think.Encrypted)
		assert.Contains(t, string(think.Item), `"encrypted_content":"ENC-LATE"`)
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
			"reasoning": {"effort": "none"},
			"instructions": "be terse",
			"input": [
				{"type": "message", "role": "user", "status": "completed", "id": "msg_pi_0",
					"content": [{"type": "input_text", "text": "hi"}]}
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
	t.Run("stored_responses_keep_the_include", func(t *testing.T) {
		// include rides on the reasoning param rather than the store flag; a
		// stored model still requests the encrypted payload so replay works
		req := baseReq()
		req.Model.Caps.Store = true
		req.Reasoning = ReasoningConfig{Level: LevelMedium}

		body, err := buildResponsesBody(req)
		require.NoError(t, err)

		m := decode(t, body)
		assert.Equal(t, []any{respEncryptedInclude}, m["include"])
		assert.NotContains(t, m, "store")
	})
	t.Run("level_off_names_the_none_effort", func(t *testing.T) {
		// pi sends reasoning:{effort:"none"} for off so the model stops
		// thinking; only {off:null} suppresses the key entirely
		req := baseReq()
		req.Reasoning = ReasoningConfig{Level: LevelOff}

		body, err := buildResponsesBody(req)
		require.NoError(t, err)

		reasoning := decode(t, body)["reasoning"].(map[string]any)
		assert.Equal(t, "none", reasoning["effort"])
	})
	t.Run("off_null_suppresses_reasoning", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.LevelMap = map[Level]*string{LevelOff: nil}

		body, err := buildResponsesBody(req)
		require.NoError(t, err)
		assert.NotContains(t, decode(t, body), "reasoning")
	})
	t.Run("unsupported_level_clamps_internally", func(t *testing.T) {
		// xhigh is opt-in; a bare build clamps to high so the encoder never sends
		// an unmapped level even when no agent pre-clamped it
		req := baseReq()
		req.Reasoning = ReasoningConfig{Level: LevelXHigh}

		body, err := buildResponsesBody(req)
		require.NoError(t, err)

		reasoning := decode(t, body)["reasoning"].(map[string]any)
		assert.Equal(t, "high", reasoning["effort"])
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
	t.Run("max_output_tokens_clamped_to_the_floor", func(t *testing.T) {
		req := baseReq()
		req.MaxTokens = 5 // below openai's floor of 16

		body, err := buildResponsesBody(req)
		require.NoError(t, err)

		m := decode(t, body)
		assert.Equal(t, respMinOutputTokens, int(m["max_output_tokens"].(float64)))
	})
	t.Run("tool_result_images_become_parts", func(t *testing.T) {
		req := baseReq()
		req.Messages = []Message{
			{Role: RoleAssistant, Content: BlockList{
				ToolCallBlock{ID: "call_1|fc_9", Name: "read", Input: json.RawMessage(`{}`)},
			}},
			{Role: RoleTool, Content: BlockList{
				ToolResultBlock{CallID: "call_1|fc_9", Content: BlockList{
					TextBlock{Text: "body"},
					ImageBlock{MediaType: "image/png", Data: []byte{1}},
				}},
			}},
		}
		body, err := buildResponsesBody(req)
		require.NoError(t, err)

		input := decode(t, body)["input"].([]any)
		result := input[len(input)-1].(map[string]any)
		assert.Equal(t, "function_call_output", result["type"])
		parts := result["output"].([]any)
		require.Len(t, parts, 2)
		assert.Equal(t, map[string]any{"type": "input_text", "text": "body"}, parts[0])
		img := parts[1].(map[string]any)
		assert.Equal(t, "input_image", img["type"])
		assert.Contains(t, img["image_url"], "data:image/png;base64,AQ==")
	})
	t.Run("tool_result_images_stay_text_when_unsupported", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.Images = false
		req.Messages = []Message{
			{Role: RoleAssistant, Content: BlockList{
				ToolCallBlock{ID: "call_1|fc_9", Name: "read", Input: json.RawMessage(`{}`)},
			}},
			{Role: RoleTool, Content: BlockList{
				ToolResultBlock{CallID: "call_1|fc_9", Content: BlockList{
					ImageBlock{MediaType: "image/png", Data: []byte{1}},
				}},
			}},
		}
		body, err := buildResponsesBody(req)
		require.NoError(t, err)

		input := decode(t, body)["input"].([]any)
		result := input[len(input)-1].(map[string]any)
		assert.IsType(t, "", result["output"])
	})
	t.Run("mid_conversation_system_becomes_developer", func(t *testing.T) {
		req := baseReq()
		req.Messages = []Message{
			Text(RoleUser, "hi"),
			{Role: RoleSystem, Content: BlockList{TextBlock{Text: "stay on topic"}}},
		}

		body, err := buildResponsesBody(req)
		require.NoError(t, err)

		input := decode(t, body)["input"].([]any)
		sys := input[len(input)-1].(map[string]any)
		assert.Equal(t, "message", sys["type"])
		assert.Equal(t, "developer", sys["role"])
	})
	t.Run("mid_conversation_system_stays_system_without_dev_role", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.DeveloperRole = false
		req.Messages = []Message{
			{Role: RoleSystem, Content: BlockList{TextBlock{Text: "behave"}}},
		}

		body, err := buildResponsesBody(req)
		require.NoError(t, err)

		input := decode(t, body)["input"].([]any)
		sys := input[0].(map[string]any)
		assert.Equal(t, "system", sys["role"])
	})
	t.Run("thinking_without_an_item_id_is_dropped", func(t *testing.T) {
		req := baseReq()
		req.Reasoning.Retain = RetainAll
		req.Messages = sameOrigin(req.Model, []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{
				ThinkingBlock{Text: "unreferencable"},
				TextBlock{Text: "answer"},
			}},
		})
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
	t.Run("image_downgraded_when_unsupported", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.Images = false
		req.Messages = []Message{{Role: RoleUser, Content: BlockList{
			ImageBlock{MediaType: "image/png", Data: []byte{1}},
		}}}

		body, err := buildResponsesBody(req)
		require.NoError(t, err)
		assert.Contains(t, string(body), imageOmitted)
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

// TestResponsesReplayRoundTrip streams a multi-turn fixture and replays it,
// asserting pi's stateless replay: the reasoning item round-trips verbatim, the
// message keeps its id and phase, and the tool call keeps its fc_ pairing.
func TestResponsesReplayRoundTrip(t *testing.T) {
	t.Parallel()

	srv, _ := sseServer(t, "openai/replay_multiturn.sse")
	p := newResponsesTestProvider(t, srv.URL)

	s, err := p.Stream(t.Context(), Request{Model: responsesModel(nil)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	msg, _, err := Accumulate(s)
	require.NoError(t, err)
	require.Len(t, msg.Content, 3)

	rm := responsesModel(nil)
	body, err := buildResponsesBody(Request{
		Model:     rm,
		Messages:  sameOrigin(rm, []Message{Text(RoleUser, "q"), msg}),
		Reasoning: ReasoningConfig{Retain: RetainAll},
	})
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(body, &m))
	input := m["input"].([]any)
	// the trailing tool call is answered by a synthetic result (orphan synthesis)
	require.Len(t, input, 5)

	// the reasoning item replays byte-identical, summary included
	reasoning, err := json.Marshal(input[1])
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"reasoning","id":"rs_replay","encrypted_content":"ENC-REPLAY","summary":[{"type":"summary_text","text":"checking files"}]}`,
		string(reasoning))

	// the message item keeps its original id and phase, in block order before the call
	message := input[2].(map[string]any)
	assert.Equal(t, "message", message["type"])
	assert.Equal(t, "msg_02", message["id"])
	assert.Equal(t, "final_answer", message["phase"])
	assert.Equal(t, "completed", message["status"])

	call := input[3].(map[string]any)
	assert.Equal(t, "function_call", call["type"])
	assert.Equal(t, "fc_replay", call["id"])
	assert.Equal(t, "call_replay", call["call_id"])
}

func TestResponsesItemsReplay(t *testing.T) {
	t.Parallel()

	decode := func(t *testing.T, body []byte) []any {
		t.Helper()
		var m map[string]any
		require.NoError(t, json.Unmarshal(body, &m))
		return m["input"].([]any)
	}
	build := func(t *testing.T, msgs ...Message) []any {
		t.Helper()
		m := responsesModel(nil)
		body, err := buildResponsesBody(Request{
			Model:     m,
			Messages:  sameOrigin(m, msgs),
			Reasoning: ReasoningConfig{Retain: RetainAll},
		})
		require.NoError(t, err)
		return decode(t, body)
	}

	t.Run("unsigned_text_gets_fallback_ids", func(t *testing.T) {
		input := build(t,
			Text(RoleUser, "one"),
			Text(RoleUser, "two"),
		)
		require.Len(t, input, 2)
		assert.Equal(t, "msg_pi_0", input[0].(map[string]any)["id"])
		assert.Equal(t, "msg_pi_1", input[1].(map[string]any)["id"])
	})
	t.Run("later_text_blocks_index_the_fallback", func(t *testing.T) {
		// a tool call interrupts the text run, so the second message needs its own
		// fallback id rather than reusing the first
		input := build(t, Message{Role: RoleAssistant, Content: BlockList{
			TextBlock{Text: "first"},
			ToolCallBlock{ID: "call_1", Name: "read", Input: json.RawMessage(`{}`)},
			TextBlock{Text: "second"},
		}})
		// the unanswered call is answered by a synthetic result (orphan synthesis)
		require.Len(t, input, 4)
		assert.Equal(t, "msg_pi_0", input[0].(map[string]any)["id"])
		assert.Equal(t, "msg_pi_0_1", input[2].(map[string]any)["id"])
	})
	t.Run("adjacent_text_blocks_merge", func(t *testing.T) {
		input := build(t, Message{Role: RoleAssistant, Content: BlockList{
			ThinkingBlock{ItemID: "rs_1"},
			TextBlock{Text: "first"},
			TextBlock{Text: "second"},
		}})
		require.Len(t, input, 2)
		message := input[1].(map[string]any)
		assert.Equal(t, "msg_pi_0", message["id"])
		assert.Len(t, message["content"].([]any), 2)
	})
	t.Run("distinct_signed_texts_stay_separate", func(t *testing.T) {
		// commentary and final answer are separate output items; each keeps its own
		// id and phase across the replay rather than collapsing into one message
		input := build(t, Message{Role: RoleAssistant, Content: BlockList{
			TextBlock{Text: "commentary", Signature: encodeTextSignature("msg_c", "commentary")},
			TextBlock{Text: "final answer", Signature: encodeTextSignature("msg_f", "final_answer")},
		}})
		require.Len(t, input, 2)

		first := input[0].(map[string]any)
		assert.Equal(t, "message", first["type"])
		assert.Equal(t, "msg_c", first["id"])
		assert.Equal(t, "commentary", first["phase"])

		second := input[1].(map[string]any)
		assert.Equal(t, "message", second["type"])
		assert.Equal(t, "msg_f", second["id"])
		assert.Equal(t, "final_answer", second["phase"])
	})
	t.Run("overlong_id_hashes_to_a_msg_id", func(t *testing.T) {
		longID := strings.Repeat("x", 100)
		input := build(t, Message{Role: RoleAssistant, Content: BlockList{
			TextBlock{Text: "a", Signature: encodeTextSignature(longID, "")},
		}})
		require.Len(t, input, 1)
		id := input[0].(map[string]any)["id"].(string)
		assert.True(t, strings.HasPrefix(id, "msg_"))
		assert.NotEqual(t, longID, id)
	})
	t.Run("legacy_bare_signature_is_the_id", func(t *testing.T) {
		input := build(t, Message{Role: RoleAssistant, Content: BlockList{
			TextBlock{Text: "a", Signature: "msg_legacy"},
		}})
		require.Len(t, input, 1)
		assert.Equal(t, "msg_legacy", input[0].(map[string]any)["id"])
	})
	t.Run("non_fc_item_id_is_dropped", func(t *testing.T) {
		input := build(t, Message{Role: RoleAssistant, Content: BlockList{
			ToolCallBlock{ID: "call_1|rs_9", Name: "read", Input: json.RawMessage(`{}`)},
		}})
		// the non-fc item is dropped and the orphaned call gets a synthetic result
		require.Len(t, input, 2)
		call := input[0].(map[string]any)
		assert.Equal(t, "call_1", call["call_id"])
		assert.NotContains(t, call, "id")
	})
	t.Run("thinking_without_a_replay_token_is_dropped", func(t *testing.T) {
		input := build(t, Message{Role: RoleAssistant, Content: BlockList{
			ThinkingBlock{Text: "unreferencable"},
			TextBlock{Text: "answer"},
		}})
		require.Len(t, input, 1)
		assert.Equal(t, "message", input[0].(map[string]any)["type"])
	})
	t.Run("item_only_reasoning_replays_verbatim", func(t *testing.T) {
		raw := json.RawMessage(`{"type":"reasoning","id":"rs_full","summary":[{"type":"summary_text","text":"s"}]}`)
		input := build(t, Message{Role: RoleAssistant, Content: BlockList{
			ThinkingBlock{Item: raw},
		}})
		require.Len(t, input, 1)
		got, err := json.Marshal(input[0])
		require.NoError(t, err)
		assert.JSONEq(t, string(raw), string(got))
	})
	t.Run("tool_result_uses_the_bare_call_id", func(t *testing.T) {
		input := build(t,
			Message{Role: RoleAssistant, Content: BlockList{
				ToolCallBlock{ID: "call_1|fc_9", Name: "read", Input: json.RawMessage(`{}`)},
			}},
			Message{Role: RoleTool, Content: BlockList{
				ToolResultBlock{CallID: "call_1|fc_9", Content: BlockList{TextBlock{Text: "out"}}},
			}},
		)
		require.Len(t, input, 2)
		assert.Equal(t, "fc_9", input[0].(map[string]any)["id"])
		assert.Equal(t, "call_1", input[1].(map[string]any)["call_id"])
	})
}

func TestTextSignature(t *testing.T) {
	t.Parallel()

	t.Run("round_trip", func(t *testing.T) {
		sig := encodeTextSignature("msg_02", "final_answer")
		id, phase := parseTextSignature(sig)
		assert.Equal(t, "msg_02", id)
		assert.Equal(t, "final_answer", phase)
	})
	t.Run("phase_omitted_when_empty", func(t *testing.T) {
		assert.JSONEq(t, `{"v":1,"id":"msg_02"}`, encodeTextSignature("msg_02", ""))
	})
	t.Run("commentary_is_accepted", func(t *testing.T) {
		_, phase := parseTextSignature(`{"v":1,"id":"m","phase":"commentary"}`)
		assert.Equal(t, "commentary", phase)
	})
	t.Run("unknown_phase_is_dropped", func(t *testing.T) {
		id, phase := parseTextSignature(`{"v":1,"id":"m","phase":"draft"}`)
		assert.Equal(t, "m", id)
		assert.Empty(t, phase)
	})
	t.Run("bare_id_passes_through", func(t *testing.T) {
		id, phase := parseTextSignature("msg_legacy")
		assert.Equal(t, "msg_legacy", id)
		assert.Empty(t, phase)
	})
	t.Run("bare_id_trims_surrounding_space", func(t *testing.T) {
		id, _ := parseTextSignature("  msg_padded\t")
		assert.Equal(t, "msg_padded", id)
	})
	t.Run("empty_is_empty", func(t *testing.T) {
		id, phase := parseTextSignature("")
		assert.Empty(t, id)
		assert.Empty(t, phase)
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
