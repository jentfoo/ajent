package llm

import (
	"encoding/json"
	"testing"

	"github.com/go-analyze/bulk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// compatModel is a chat-completions model with the given capability overrides.
func compatModel(fn func(*Capabilities)) Model {
	caps := flavorDefaults[FlavorLMStudio].caps
	// deepseek is the common case: detection would set both of these together,
	// and ReplayReasoning drives the empty reasoning_content echo on replies
	caps.Thinking = ThinkingDeepSeek // the common case, not inline tags
	caps.ReplayReasoning = true
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
		name: "lmstudio", flavor: FlavorLMStudio,
		classify: compatClassifier("lmstudio", FlavorLMStudio),
	}}
}

// thinkModel is a compat model parsing inline think tags.
func thinkModel() Model {
	return compatModel(func(c *Capabilities) {
		c.ThinkOpen, c.ThinkClose = thinkOpenTag, thinkCloseTag
	})
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

	t.Run("reasoning_content_field_records_source", func(t *testing.T) {
		srv, _ := sseServer(t, "compat/reasoning_content.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: compatModel(nil)})
		require.NoError(t, err)
		events := collect(t, s)

		assert.Equal(t, "let me think", thinkingOf(events))
		assert.Equal(t, "answer", textOf(events))
		// the originating delta field rides onto the block for replay (B1/B2)
		var tb ThinkingBlock
		for _, ev := range events {
			if b, ok := ev.Block.(ThinkingBlock); ok {
				tb = b
			}
		}
		assert.Equal(t, "reasoning_content", tb.Field)
	})

	t.Run("reasoning_text_field_read_last", func(t *testing.T) {
		srv, _ := sseServer(t, "compat/reasoning_text.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: compatModel(nil)})
		require.NoError(t, err)
		events := collect(t, s)

		assert.Equal(t, "chutes thinks", thinkingOf(events))
		var tb ThinkingBlock
		for _, ev := range events {
			if b, ok := ev.Block.(ThinkingBlock); ok {
				tb = b
			}
		}
		assert.Equal(t, "reasoning_text", tb.Field)
	})

	t.Run("reasoning_content_precedes_reasoning", func(t *testing.T) {
		srv, _ := sseServer(t, "compat/reasoning_precedence.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: compatModel(nil)})
		require.NoError(t, err)
		events := collect(t, s)

		// reasoning_content is read before reasoning, since chutes sends both fields
		assert.Equal(t, "preferred", thinkingOf(events))
	})

	t.Run("separated_reasoning_regions_stay_clean", func(t *testing.T) {
		srv, _ := sseServer(t, "compat/second_thinking.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: compatModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		msg, _, err := Accumulate(s)
		require.NoError(t, err)
		// interleaved regions become distinct blocks in stream order, each with its
		// own text and field. Nothing duplicates or inherits the other's metadata.
		assert.Equal(t, BlockList{
			ThinkingBlock{Text: "first region", Field: "reasoning_content"},
			TextBlock{Text: "visible answer"},
			ThinkingBlock{Text: "second region", Field: "reasoning_content"},
			TextBlock{Text: "more text"},
		}, msg.Content)
	})

	t.Run("think_tags_split_across_deltas", func(t *testing.T) {
		srv, _ := sseServer(t, "compat/think_tags.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: thinkModel()})
		require.NoError(t, err)
		events := collect(t, s)

		assert.Equal(t, "pondering", thinkingOf(events))
		assert.Equal(t, "the answer", textOf(events)) // no leaked tag fragment
	})

	t.Run("text_tail_flush_commits", func(t *testing.T) {
		srv, _ := sseServer(t, "compat/think_tail_text.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: thinkModel()})
		require.NoError(t, err)
		events := collect(t, s)

		assert.Equal(t, "Hello<", textOf(events)) // the withheld "<" still streams

		var acc Accumulator
		for _, ev := range events {
			acc.Add(ev)
		}
		// the end block must carry the tail too, or the transcript loses it
		assert.Equal(t, BlockList{TextBlock{Text: "Hello<"}}, acc.Message().Content)
	})

	t.Run("thinking_tail_flush_commits", func(t *testing.T) {
		srv, _ := sseServer(t, "compat/think_tail_thinking.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: thinkModel()})
		require.NoError(t, err)

		msg, _, err := Accumulate(s)
		require.NoError(t, err)
		// an unclosed region keeps its partial close tag verbatim
		assert.Equal(t, BlockList{ThinkingBlock{Text: "pondering</th",
			Field: "reasoning_content"}}, msg.Content)
	})

	t.Run("flush_tail_opens_block", func(t *testing.T) {
		srv, _ := sseServer(t, "compat/think_tail_lone_angle.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: thinkModel()})
		require.NoError(t, err)
		events := collect(t, s)

		// no block ever opened: the tail opens one instead of vanishing
		assert.Equal(t, []string{
			"message_start", "text_start", "text_delta", "text_end", "done",
		}, eventKinds(events))

		var acc Accumulator
		for _, ev := range events {
			acc.Add(ev)
		}
		assert.Equal(t, BlockList{TextBlock{Text: "<"}}, acc.Message().Content)
	})

	t.Run("tail_keeps_block_order", func(t *testing.T) {
		srv, _ := sseServer(t, "compat/think_tail_after_text.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: thinkModel()})
		require.NoError(t, err)

		msg, _, err := Accumulate(s)
		require.NoError(t, err)
		// a thinking tail closes the open text block first, keeping stream order
		assert.Equal(t, BlockList{
			TextBlock{Text: "hi"},
			ThinkingBlock{Text: "<", Field: "reasoning_content"},
		}, msg.Content)
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

	t.Run("regions_opening_after_tool_calls", func(t *testing.T) {
		// GLM/Kimi/DeepSeek-style vendors keep interleaving reasoning after
		// tool_calls deltas began, so both sides must claim distinct indexes
		srv, _ := sseServer(t, "compat/reasoning_after_tools.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: compatModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		msg, _, err := Accumulate(s)
		require.NoError(t, err)
		// a reused index would make one end event replace the other's block,
		// silently losing tool arguments or text
		require.Len(t, msg.Content, 6)
		assert.Equal(t, ThinkingBlock{Text: "first", Field: "reasoning_content"}, msg.Content[0])
		assert.Equal(t, TextBlock{Text: "answer"}, msg.Content[1])
		assert.Equal(t, ThinkingBlock{Text: "reconsidering", Field: "reasoning_content"}, msg.Content[2])
		assert.Equal(t, TextBlock{Text: "final"}, msg.Content[3])
		assert.Equal(t, "call_1", msg.Content[4].(ToolCallBlock).ID)
		assert.Equal(t, "call_2", msg.Content[5].(ToolCallBlock).ID)
	})

	t.Run("think_tags_after_tool_calls", func(t *testing.T) {
		srv, _ := sseServer(t, "compat/think_after_tools.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: thinkModel()})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		msg, _, err := Accumulate(s)
		require.NoError(t, err)
		assert.Equal(t, BlockList{
			TextBlock{Text: "visible"},
			ThinkingBlock{Text: "reconsidering", Field: "reasoning_content"},
			ToolCallBlock{ID: "call_1", Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)},
		}, msg.Content)
	})

	t.Run("malformed_args_fail_the_call_not_the_turn", func(t *testing.T) {
		srv, _ := sseServer(t, "compat/malformed_tool_args.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: compatModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		var acc Accumulator
		var events []Event
		for ev, ok := s.Next(); ok; ev, ok = s.Next() {
			acc.Add(ev)
			events = append(events, ev)
		}
		require.NoError(t, s.Err()) // a malformed call must not fail the stream
		assert.NoError(t, acc.Err())

		ends := bulk.SliceFilter(func(ev Event) bool { return ev.Type == EventToolCallEnd }, events)
		require.Len(t, ends, 2)
		require.ErrorIs(t, ends[0].Err, ErrMalformedToolArgs) // the one call failed
		require.NoError(t, ends[1].Err)                       // its sibling stayed valid

		done := events[len(events)-1]
		assert.Equal(t, EventDone, done.Type)
		assert.Equal(t, StopToolUse, done.StopReason)
		require.NoError(t, done.Err)

		require.Len(t, acc.Message().Content, 2) // both calls reach the agent for dispatch
	})

	t.Run("tool_index_restart_keeps_both_calls", func(t *testing.T) {
		// a proxy emitting one batch of calls, then a fresh sequential batch
		// restarting at index 0 within the same response
		srv, _ := sseServer(t, "compat/tool_index_restart.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: compatModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		msg, _, err := Accumulate(s)
		require.NoError(t, err)
		require.Len(t, msg.Content, 2)
		first := msg.Content[0].(ToolCallBlock)
		second := msg.Content[1].(ToolCallBlock)
		assert.Equal(t, "call_1", first.ID)
		assert.JSONEq(t, `{"path":"a.go"}`, string(first.Input))
		assert.Equal(t, "call_2", second.ID)
		assert.JSONEq(t, `{"path":"b.go"}`, string(second.Input))
	})

	t.Run("unnamed_reuse_fails_that_call_not_the_turn", func(t *testing.T) {
		// fragments arriving on the reused index with no id or name cannot be
		// attributed: the closed call stays intact and the stray call fails by name
		srv, _ := sseServer(t, "compat/tool_index_restart_unnamed.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: compatModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		var acc Accumulator
		var events []Event
		for ev, ok := s.Next(); ok; ev, ok = s.Next() {
			acc.Add(ev)
			events = append(events, ev)
		}
		require.NoError(t, s.Err()) // the failure is recorded, not a stream error
		assert.NoError(t, acc.Err())

		ends := bulk.SliceFilter(func(ev Event) bool { return ev.Type == EventToolCallEnd }, events)
		require.Len(t, ends, 2)
		require.NoError(t, ends[0].Err) // the first call was never merged into
		assert.JSONEq(t, `{"path":"a.go"}`, string(ends[0].Block.(ToolCallBlock).Input))
		require.ErrorIs(t, ends[1].Err, ErrReusedToolIndex)
		assert.Contains(t, ends[1].Err.Error(), `{"pa`) // the evidence is named

		done := events[len(events)-1]
		assert.Equal(t, EventDone, done.Type)
		assert.Equal(t, StopToolUse, done.StopReason)

		msg := acc.Message()
		require.Len(t, msg.Content, 2) // both reach the agent for dispatch
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

		// the fixture never sends a finish_reason. That is fine for providers which
		// declare they do not support one (the cache fields are what this tests)
		s, err := p.Stream(t.Context(), Request{Model: compatModel(func(c *Capabilities) {
			c.SupportsFinishReason = false
		})})
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
		require.Error(t, last.Err)
		ok, _ := Recoverable(s.Err())
		assert.False(t, ok) // an opaque code keeps the fail-safe default
	})

	t.Run("mid_stream_numeric_code_is_recoverable", func(t *testing.T) {
		srv, _ := sseServer(t, "compat/error_midstream_numeric.sse")
		p := newCompatTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: compatModel(nil)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		for {
			if _, ok := s.Next(); !ok {
				break
			}
		}
		require.Error(t, s.Err())
		ok, _ := Recoverable(s.Err())
		assert.True(t, ok) // numeric codes ride the same status ladder
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

func TestCompatStreamFinishReason(t *testing.T) {
	t.Parallel()

	collectStop := func(t *testing.T, fixture string, caps Capabilities) (StopReason, error) {
		t.Helper()

		srv, _ := sseServer(t, fixture)
		p := newCompatTestProvider(t, srv.URL)
		s, err := p.Stream(t.Context(), Request{Model: compatModel(func(c *Capabilities) { *c = caps })})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })
		var events []Event
		for ev, ok := s.Next(); ok; ev, ok = s.Next() {
			events = append(events, ev)
		}
		last := events[len(events)-1]
		return last.StopReason, last.Err
	}

	t.Run("no_finish_tools_infer_stop_tool_use", func(t *testing.T) {
		caps := flavorDefaults[FlavorLMStudio].caps
		caps.SupportsFinishReason = false
		stop, err := collectStop(t, "compat/no_finish_tool.sse", caps)
		require.NoError(t, err)
		assert.Equal(t, StopToolUse, stop)
	})

	t.Run("no_finish_text_infer_end_turn", func(t *testing.T) {
		caps := flavorDefaults[FlavorLMStudio].caps
		caps.SupportsFinishReason = false
		stop, err := collectStop(t, "compat/no_finish_text.sse", caps)
		require.NoError(t, err)
		assert.Equal(t, StopEndTurn, stop)
	})

	t.Run("no_finish_with_support_is_truncation", func(t *testing.T) {
		caps := flavorDefaults[FlavorLMStudio].caps
		caps.SupportsFinishReason = true
		_, err := collectStop(t, "compat/no_finish_text.sse", caps)
		assert.ErrorContains(t, err, "stream ended without finish_reason")
	})

	t.Run("content_filter_stops_incomplete", func(t *testing.T) {
		stop, err := collectStop(t, "compat/content_filter.sse", flavorDefaults[FlavorLMStudio].caps)
		require.NoError(t, err)
		assert.Equal(t, StopIncomplete, stop)
	})
}

func TestStopReasonFrom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		reason   string
		expected StopReason
	}{
		{"stop", "stop", StopEndTurn},
		{"end_turn_spelling", "end_turn", StopEndTurn},
		{"tool_calls", "tool_calls", StopToolUse},
		{"function_call", "function_call", StopToolUse},
		{"length", "length", StopMaxTokens},
		{"max_tokens_field", fieldMaxTokens, StopMaxTokens},
		{"content_filter_is_incomplete", "content_filter", StopIncomplete},
		{"mixed_case_stop", "STOP", StopEndTurn},
		{"mixed_case_tool_calls", "Tool_Calls", StopToolUse},
		{"mixed_case_length", "Length", StopMaxTokens},
		{"mixed_case_content_filter", "Content_Filter", StopIncomplete},
		{"empty_is_unknown", "", StopUnknown},
		{"unknown_defaults_end_turn", "some_future_reason", StopEndTurn},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, stopReasonFrom(tc.reason))
		})
	}
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
		// the default level is off, and a deepseek model toggles it explicitly
		assert.JSONEq(t, `{
			"model": "m1",
			"stream": true,
			"stream_options": {"include_usage": true},
			"max_tokens": 100,
			"thinking": {"type": "disabled"},
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

	t.Run("prompt_cache_key_when_enabled", func(t *testing.T) {
		req := baseReq()
		req.SessionID = t.Name()
		req.Cache = CachePolicy{Enabled: true}
		req.Model.Caps.SupportsExplicitPromptCache = true

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.Contains(t, string(body), `"prompt_cache_key":"`+t.Name()+`"`)
	})

	t.Run("no_prompt_cache_key_when_disabled", func(t *testing.T) {
		req := baseReq()
		req.SessionID = t.Name()
		req.Model.Caps.SupportsExplicitPromptCache = true // capable but not requested

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.NotContains(t, string(body), "prompt_cache_key")
	})

	t.Run("no_prompt_cache_key_when_uncapable", func(t *testing.T) {
		req := baseReq() // lmstudio flavor: no SupportsExplicitPromptCache
		req.SessionID = t.Name()
		req.Cache = CachePolicy{Enabled: true}

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.NotContains(t, string(body), "prompt_cache_key")
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
		req.Model.Caps.Reasoning, req.Model.Caps.Thinking = true, ThinkingOpenAI
		req.Reasoning = ReasoningConfig{Level: LevelHigh}

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.Equal(t, "high", decode(t, body)["reasoning_effort"])
	})

	t.Run("level_map_translates_the_effort", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.Reasoning, req.Model.Caps.Thinking = true, ThinkingOpenAI
		req.Model.Caps.LevelMap = map[Level]*string{LevelXHigh: ptr("high")}
		req.Reasoning = ReasoningConfig{Level: LevelXHigh}

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.Equal(t, "high", decode(t, body)["reasoning_effort"])
	})

	t.Run("null_level_map_entry_omits_the_parameter", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.Reasoning, req.Model.Caps.Thinking = true, ThinkingOpenAI
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

	t.Run("store_false_sent_when_capability_set", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.Store = true // endpoint accepts the store param

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.Equal(t, false, decode(t, body)["store"])
	})

	t.Run("no_store_when_capability_unset", func(t *testing.T) {
		body, err := buildCompatBody(baseReq(), compatProfile{})
		require.NoError(t, err)
		assert.NotContains(t, decode(t, body), "store")
	})

	t.Run("vercel_gateway_routing_emits_provider_options", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.VercelGatewayRouting =
			json.RawMessage(`{"only":["fireworks"],"order":["fireworks","novita"]}`)

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		po, ok := decode(t, body)["provider_options"].(map[string]any)
		require.True(t, ok)
		gateway := po["gateway"].(map[string]any)
		assert.Equal(t, []any{"fireworks"}, gateway["only"])
		assert.Equal(t, []any{"fireworks", "novita"}, gateway["order"])
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

	t.Run("image_downgraded_when_unsupported", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.Images = false
		req.Messages = []Message{{Role: RoleUser, Content: BlockList{
			ImageBlock{MediaType: "image/png", Data: []byte{1}},
		}}}

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.Contains(t, string(body), imageOmitted)
	})

	t.Run("reasoning_replays_to_its_source_field", func(t *testing.T) {
		req := baseReq()
		req.Reasoning.Retain = RetainAll
		req.Messages = sameOrigin(req.Model, []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{
				ThinkingBlock{Field: "reasoning_content", Text: "because"},
				TextBlock{Text: "answer"},
			}},
		})
		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.Contains(t, string(body), `"reasoning_content":"because"`)
	})

	t.Run("replays_to_block_field_without_config", func(t *testing.T) {
		// no configured ReasoningField: the block's own source field wins
		req := baseReq()
		req.Model.Caps.ReasoningField = ""
		req.Reasoning.Retain = RetainAll
		req.Messages = sameOrigin(req.Model, []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{
				ThinkingBlock{Field: "reasoning", Text: "pondered"},
				TextBlock{Text: "answer"},
			}},
		})
		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.Contains(t, string(body), `"reasoning":"pondered"`)
	})

	t.Run("configured_field_overrides_block_source", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.ReasoningField = "custom_reason"
		req.Reasoning.Retain = RetainAll
		req.Messages = sameOrigin(req.Model, []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{
				ThinkingBlock{Field: "reasoning", Text: "pondered"},
				TextBlock{Text: "answer"},
			}},
		})
		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.Contains(t, string(body), `"custom_reason":"pondered"`)
	})

	t.Run("multi_block_thinking_joined_with_newline", func(t *testing.T) {
		req := baseReq()
		req.Reasoning.Retain = RetainAll
		req.Messages = sameOrigin(req.Model, []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{
				ThinkingBlock{Field: "reasoning_content", Text: "first"},
				ThinkingBlock{Field: "reasoning_content", Text: "second"},
				TextBlock{Text: "answer"},
			}},
		})
		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.Contains(t, string(body), `"reasoning_content":"first\nsecond"`)
	})

	t.Run("empty_reasoning_forced_when_level_on", func(t *testing.T) {
		req := baseReq()
		req.Reasoning = ReasoningConfig{Level: LevelHigh, Retain: RetainAll}
		req.Messages = []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{TextBlock{Text: "plain reply"}}},
		}
		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.Contains(t, string(body), `"reasoning_content":""`)
	})

	t.Run("no_empty_reasoning_when_level_off", func(t *testing.T) {
		req := baseReq()
		req.Reasoning = ReasoningConfig{Retain: RetainAll}
		req.Messages = []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{TextBlock{Text: "plain reply"}}},
		}
		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.NotContains(t, string(body), `reasoning_content`)
	})

	t.Run("non_deepseek_replay_override_forces_empty", func(t *testing.T) {
		// requiresReplayReasoningOnAssistantMessages drives the empty echo for any
		// model, not just deepseek. Detection supplies it for deepseek by default.
		req := baseReq()
		req.Model.Caps.Thinking = ThinkingOpenAI // a non-deepseek format
		req.Reasoning = ReasoningConfig{Level: LevelHigh, Retain: RetainAll}
		req.Messages = []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{TextBlock{Text: "plain reply"}}},
		}
		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.Contains(t, string(body), `"reasoning_content":""`)
	})

	t.Run("non_deepseek_without_replay_sends_nothing", func(t *testing.T) {
		// the field stays silent when not requested: ReplayReasoning is now a real gate
		req := baseReq()
		req.Model.Caps.Thinking = ThinkingOpenAI // a non-deepseek format
		req.Model.Caps.ReplayReasoning = false
		req.Reasoning = ReasoningConfig{Level: LevelHigh, Retain: RetainAll}
		req.Messages = []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{TextBlock{Text: "plain reply"}}},
		}
		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.NotContains(t, string(body), `reasoning_content`)
	})

	t.Run("empty_assistant_message_skipped", func(t *testing.T) {
		// neither content nor tool calls: dropped regardless of reasoning fields
		req := baseReq()
		req.Model.Caps.ReplayReasoning = true
		req.Reasoning.Retain = RetainAll
		req.Messages = sameOrigin(req.Model, []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{ThinkingBlock{Text: "because"}}},
		})
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

	// kimiDiscovery frames a turn whose search result loads the named tools.
	kimiDiscovery := func(added ...string) []Message {
		return []Message{
			Text(RoleUser, "find tools"),
			{Role: RoleAssistant, Content: BlockList{
				ToolCallBlock{ID: "c1", Name: "search", Input: json.RawMessage(`{}`)},
			}},
			{Role: RoleUser, Content: BlockList{
				ToolResultBlock{CallID: "c1", Content: BlockList{TextBlock{Text: "found"}},
					AddedToolNames: added},
			}},
			Text(RoleUser, "use it"),
		}
	}
	kimiTools := func(names ...string) []ToolSchema {
		out := make([]ToolSchema, len(names))
		for i, n := range names {
			out[i] = ToolSchema{Name: n, Description: "the " + n + " tool",
				Parameters: json.RawMessage(`{"type":"object"}`)}
		}
		return out
	}
	funcName := func(t *testing.T, tool any) string {
		t.Helper()
		fn, ok := tool.(map[string]any)["function"].(map[string]any)
		require.True(t, ok)
		n, ok := fn["name"].(string)
		require.True(t, ok)
		return n
	}

	t.Run("kimi_load_point_reoffers_loaded_tool", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.DeferredTools = "kimi"
		req.Tools = kimiTools("read", "deep")
		req.Messages = kimiDiscovery("deep")

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)

		m := decode(t, body)
		topLevel := m["tools"].([]any)
		require.Len(t, topLevel, 1) // the loaded tool leaves the top level
		assert.Equal(t, "read", funcName(t, topLevel[0]))

		var sys map[string]any
		for _, raw := range m["messages"].([]any) {
			mm := raw.(map[string]any)
			if mm["role"] == "system" && mm["tools"] != nil {
				sys = mm
			}
		}
		require.NotNil(t, sys, "no system tool message after the load point")
		assert.NotContains(t, sys, "content") // a bare system message with tools
		loaded := sys["tools"].([]any)
		require.Len(t, loaded, 1)
		assert.Equal(t, "deep", funcName(t, loaded[0]))
	})

	t.Run("kimi_mode_off_keeps_all_tools", func(t *testing.T) {
		req := baseReq() // DeferredTools unset
		req.Tools = kimiTools("read", "deep")
		req.Messages = kimiDiscovery("deep")

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)

		m := decode(t, body)
		assert.Len(t, m["tools"].([]any), 2)
		for _, raw := range m["messages"].([]any) {
			assert.NotContains(t, raw.(map[string]any), "tools")
		}
	})

	t.Run("kimi_batch_shares_one_load_point", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.DeferredTools = "kimi"
		req.Tools = kimiTools("read", "deep", "deeper")
		req.Messages = []Message{
			Text(RoleUser, "find tools"),
			{Role: RoleAssistant, Content: BlockList{
				ToolCallBlock{ID: "c1", Name: "search", Input: json.RawMessage(`{}`)},
				ToolCallBlock{ID: "c2", Name: "search", Input: json.RawMessage(`{}`)},
			}},
			{Role: RoleUser, Content: BlockList{
				ToolResultBlock{CallID: "c1", Content: BlockList{TextBlock{Text: "a"}},
					AddedToolNames: []string{"deeper"}},
				ToolResultBlock{CallID: "c2", Content: BlockList{TextBlock{Text: "b"}},
					AddedToolNames: []string{"deep", "deep"}},
			}},
		}

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)

		msgs := decode(t, body)["messages"].([]any)
		var names []string
		for _, raw := range msgs {
			mm := raw.(map[string]any)
			if tools, ok := mm["tools"].([]any); ok {
				for _, tool := range tools {
					names = append(names, funcName(t, tool))
				}
			}
		}
		assert.Equal(t, []string{"deeper", "deep"}, names) // one message, first-seen order
	})

	t.Run("kimi_unknown_loaded_name_skipped", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.DeferredTools = "kimi"
		req.Tools = kimiTools("read") // "ghost" was never offered
		req.Messages = kimiDiscovery("ghost")

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)

		m := decode(t, body)
		require.Len(t, m["tools"].([]any), 1)
		for _, raw := range m["messages"].([]any) {
			assert.NotContains(t, raw.(map[string]any), "tools")
		}
	})

	t.Run("kimi_all_loaded_keeps_empty_tools_param", func(t *testing.T) {
		req := baseReq()
		req.Model.Caps.DeferredTools = "kimi"
		req.Tools = kimiTools("deep")
		req.Messages = kimiDiscovery("deep")

		body, err := buildCompatBody(req, compatProfile{})
		require.NoError(t, err)
		assert.Contains(t, string(body), `"tools":[]`)
	})
}

func TestCompatUsageToUsage(t *testing.T) {
	t.Parallel()

	decode := func(raw string) Usage {
		var u compatUsage
		require.NoError(t, json.Unmarshal([]byte(raw), &u))
		return u.toUsage()
	}

	t.Run("openai_cached_tokens", func(t *testing.T) {
		got := decode(`{"prompt_tokens":100,"completion_tokens":20,
			"prompt_tokens_details":{"cached_tokens":30}}`)
		assert.Equal(t, Usage{Input: 70, Output: 20, CacheRead: 30}, got)
	})

	t.Run("deepseek_prompt_cache_hit", func(t *testing.T) {
		got := decode(`{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":30}`)
		assert.Equal(t, Usage{Input: 70, Output: 20, CacheRead: 30}, got)
	})

	t.Run("present_zero_wins_over_fallback", func(t *testing.T) {
		got := decode(`{"prompt_tokens":100,"completion_tokens":20,
			"prompt_cache_hit_tokens":30,"prompt_tokens_details":{"cached_tokens":0}}`)
		assert.Equal(t, Usage{Input: 100, Output: 20}, got)
	})

	t.Run("cache_write_subtracted", func(t *testing.T) {
		got := decode(`{"prompt_tokens":100,"completion_tokens":20,
			"prompt_tokens_details":{"cached_tokens":30,"cache_write_tokens":10}}`)
		assert.Equal(t, Usage{Input: 60, Output: 20, CacheRead: 30, CacheWrite: 10}, got)
	})

	t.Run("reasoning_tokens", func(t *testing.T) {
		got := decode(`{"prompt_tokens":100,"completion_tokens":20,
			"completion_tokens_details":{"reasoning_tokens":12}}`)
		assert.Equal(t, Usage{Input: 100, Output: 20, Reasoning: 12}, got)
	})
}

func TestCompatErrObjCodeString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"string_code", `{"code":"context_length_exceeded"}`, "context_length_exceeded"},
		{"numeric_code", `{"code":502}`, "502"},
		{"float_code", `{"code":529.0}`, "529"},
		{"missing_code", `{}`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var e compatErrObj
			require.NoError(t, json.Unmarshal([]byte(tc.raw), &e))
			assert.Equal(t, tc.want, e.codeString())
		})
	}
}

func TestImageDataURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mediaType string
		data      []byte
		want      string
	}{
		{"png_bytes", "image/png", []byte{1, 2, 3}, "data:image/png;base64,AQID"},
		{"empty_media_and_data", "", nil, "data:;base64,"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, imageDataURL(tc.mediaType, tc.data))
		})
	}
}

// TestThinkTagReplayField pins the ingest rule end to end: reasoning that
// arrived inside content tags carries the configured replay field, so the
// request after a tool result sends it back instead of dropping it.
func TestThinkTagReplayField(t *testing.T) {
	t.Parallel()

	srv, _ := sseServer(t, "compat/think_tags.sse")
	p := newCompatTestProvider(t, srv.URL)
	m := thinkModel()

	s, err := p.Stream(t.Context(), Request{Model: m})
	require.NoError(t, err)

	msg, _, err := Accumulate(s)
	require.NoError(t, err)
	require.Len(t, msg.Content, 2)
	think, ok := msg.Content[0].(ThinkingBlock)
	require.True(t, ok)
	assert.Equal(t, "reasoning_content", think.Field) // stamped at ingest

	req := Request{
		Model:     m,
		System:    BlockList{TextBlock{Text: "be terse"}},
		Reasoning: ReasoningConfig{Level: LevelHigh, Retain: RetainAll},
		Messages: sameOrigin(m, []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: msg.Content},
		}),
	}
	body, err := buildCompatBody(req, compatProfile{})
	require.NoError(t, err)
	assert.Contains(t, string(body), `"reasoning_content":"pondering"`)
}
