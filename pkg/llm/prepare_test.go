package llm

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sameOrigin stamps every message as produced by m so Prepare treats them as
// same-model rather than degrading or flattening their blocks.
func sameOrigin(m Model, msgs []Message) []Message {
	o := &Origin{Provider: m.Provider, Dialect: m.Caps.Dialect, Model: m.ID}
	out := slices.Clone(msgs)
	for i := range out {
		out[i].Origin = o
	}
	return out
}

// foreignOrigin stamps every message as produced by another model (same dialect,
// different id), which is the /model cross-model switch.
func foreignOrigin(m Model, msgs []Message) []Message {
	o := &Origin{Provider: m.Provider, Dialect: m.Caps.Dialect, Model: "other-model"}
	out := slices.Clone(msgs)
	for i := range out {
		out[i].Origin = o
	}
	return out
}

// retentionFixture is one completed turn followed by an in progress tool calling
// turn, which is what makes the wholeTurn boundary observable.
func retentionFixture() []Message {
	return []Message{
		Text(RoleUser, "first question"),
		{Role: RoleAssistant, Content: BlockList{
			ThinkingBlock{Text: "old", Signature: "sig-old", ItemID: "rs-old", Details: []byte(`["old"]`)},
			TextBlock{Text: "first answer"},
		}},
		Text(RoleUser, "second question"), // turn boundary
		{Role: RoleAssistant, Content: BlockList{
			ThinkingBlock{Text: "planning", Signature: "sig-a", ItemID: "rs-a", Details: []byte(`["a"]`)},
			ToolCallBlock{ID: "c1", Name: "read", Input: json.RawMessage(`{}`)},
		}},
		{Role: RoleUser, Content: BlockList{ // tool result, not a turn boundary
			ToolResultBlock{CallID: "c1", ToolName: "read", Content: BlockList{TextBlock{Text: "file"}}},
		}},
		{Role: RoleAssistant, Content: BlockList{
			ThinkingBlock{Text: "concluding", Signature: "sig-b", ItemID: "rs-b", Details: []byte(`["b"]`)},
			TextBlock{Text: "second answer"},
		}},
	}
}

// thinkingTexts returns the thinking text kept on each assistant message.
func thinkingTexts(msgs []Message) []string {
	var out []string
	for _, m := range msgs {
		for _, b := range m.Content {
			if t, ok := b.(ThinkingBlock); ok {
				out = append(out, t.Text)
			}
		}
	}
	return out
}

// preparedRetention runs Prepare on the fixture under policy and caps with all
// messages treated as same-model, returning the thinking kept.
func preparedRetention(policy RetainPolicy, caps Capabilities) []string {
	m := Model{Provider: "p", ID: "m", Caps: caps}
	req := Request{
		Model:     m,
		Messages:  sameOrigin(m, retentionFixture()),
		Reasoning: ReasoningConfig{Retain: policy},
	}
	return thinkingTexts(Prepare(req).Messages)
}

func TestPrepareRetention(t *testing.T) {
	t.Parallel()

	anthropic := Capabilities{Dialect: DialectAnthropic, Reasoning: true,
		Thinking: ThinkingAnthropic, ReasoningReplay: true}
	openai := Capabilities{Dialect: DialectOpenAIResponses, Reasoning: true,
		Thinking: ThinkingOpenAI, ReasoningReplay: true}
	openrouter := Capabilities{Reasoning: true, Thinking: ThinkingOpenRouter}
	inline := Capabilities{Reasoning: true, ThinkOpen: thinkOpenTag, ThinkClose: thinkCloseTag}
	none := Capabilities{}

	tests := []struct {
		name     string
		policy   RetainPolicy
		caps     Capabilities
		expected []string
	}{
		{"anthropic_none_upgraded", RetainNone, anthropic, []string{"planning", "concluding"}},
		{"anthropic_last_upgraded", RetainLastTurn, anthropic, []string{"planning", "concluding"}},
		{"anthropic_whole_turn", RetainWholeTurn, anthropic, []string{"planning", "concluding"}},
		{"anthropic_all", RetainAll, anthropic, []string{"old", "planning", "concluding"}},

		{"openai_none_upgraded", RetainNone, openai, []string{"planning", "concluding"}},
		{"openai_all", RetainAll, openai, []string{"old", "planning", "concluding"}},

		// openrouter does not require replay, so the policy is honoured as written
		{"openrouter_none", RetainNone, openrouter, nil},
		{"openrouter_last_turn", RetainLastTurn, openrouter, []string{"concluding"}},
		{"openrouter_whole_turn", RetainWholeTurn, openrouter, []string{"planning", "concluding"}},
		{"openrouter_all", RetainAll, openrouter, []string{"old", "planning", "concluding"}},

		// inline tag reasoning is re-parsed from content, never sent back, so it
		// is dropped whatever the policy asks for
		{"inline_none", RetainNone, inline, nil},
		{"inline_whole_turn", RetainWholeTurn, inline, nil},
		{"inline_all", RetainAll, inline, nil},

		// a provider that cannot read thinking blocks is downgraded to none
		{"no_reasoning_all_downgraded", RetainAll, none, nil},
		{"no_reasoning_whole_turn", RetainWholeTurn, none, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, preparedRetention(tc.policy, tc.caps))
		})
	}

	t.Run("input_is_not_mutated", func(t *testing.T) {
		m := Model{Provider: "p", ID: "m", Caps: openrouter}
		in := sameOrigin(m, retentionFixture())
		want := slices.Clone(in)
		for i := range want {
			want[i].Content = slices.Clone(want[i].Content)
		}
		Prepare(Request{Model: m, Messages: in, Reasoning: ReasoningConfig{Retain: RetainNone}})
		assert.Equal(t, retentionFixture(), restoreOriginNil(in))
	})
}

// restoreOriginNil clears the stamp so equality against the fixture holds.
func restoreOriginNil(msgs []Message) []Message {
	out := slices.Clone(msgs)
	for i := range out {
		out[i].Origin = nil
	}
	return out
}

func TestPrepareCrossModelDegradation(t *testing.T) {
	t.Parallel()

	m := Model{Provider: "openai", ID: "gpt-5", Caps: Capabilities{
		Dialect: DialectOpenAIResponses, Reasoning: true,
	}}

	t.Run("foreign_thinking_flattens_to_text", func(t *testing.T) {
		in := []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{
				ThinkingBlock{Text: "reasoned", Signature: "sig"},
				TextBlock{Text: "answer"},
			}},
		}
		out := Prepare(Request{Model: m, Messages: foreignOrigin(m, in),
			Reasoning: ReasoningConfig{Retain: RetainAll}}).Messages
		require.Len(t, out, 2)
		assert.Equal(t, BlockList{
			TextBlock{Text: "reasoned"},
			TextBlock{Text: "answer"},
		}, out[1].Content)
	})

	t.Run("foreign_redacted_thinking_dropped", func(t *testing.T) {
		in := []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{
				ThinkingBlock{Redacted: "cmVk"},
				TextBlock{Text: "answer"},
			}},
		}
		out := Prepare(Request{Model: m, Messages: foreignOrigin(m, in),
			Reasoning: ReasoningConfig{Retain: RetainAll}}).Messages
		assert.Equal(t, BlockList{TextBlock{Text: "answer"}}, out[1].Content)
	})

	t.Run("foreign_text_signature_dropped", func(t *testing.T) {
		in := []Message{
			{Role: RoleAssistant, Content: BlockList{
				TextBlock{Text: "a", Signature: encodeTextSignature("msg_9", "")},
			}},
		}
		out := Prepare(Request{Model: m, Messages: foreignOrigin(m, in),
			Reasoning: ReasoningConfig{Retain: RetainAll}}).Messages
		assert.Equal(t, BlockList{TextBlock{Text: "a"}}, out[0].Content)
	})

	t.Run("same_model_replay_preserved", func(t *testing.T) {
		in := []Message{
			{Role: RoleAssistant, Content: BlockList{
				ThinkingBlock{ItemID: "rs_1"},
				TextBlock{Text: "a", Signature: encodeTextSignature("msg_9", "")},
			}},
		}
		out := Prepare(Request{Model: m, Messages: sameOrigin(m, in),
			Reasoning: ReasoningConfig{Retain: RetainAll}}).Messages
		assert.Equal(t, BlockList{
			ThinkingBlock{ItemID: "rs_1"},
			TextBlock{Text: "a", Signature: encodeTextSignature("msg_9", "")},
		}, out[0].Content)
	})

	t.Run("nil_origin_treated_as_foreign", func(t *testing.T) {
		in := []Message{
			{Role: RoleAssistant, Content: BlockList{ThinkingBlock{Text: "t"}}},
		}
		out := Prepare(Request{Model: m, Messages: in,
			Reasoning: ReasoningConfig{Retain: RetainAll}}).Messages
		assert.Equal(t, BlockList{TextBlock{Text: "t"}}, out[0].Content)
	})

	t.Run("requires_thinking_as_text_same_model", func(t *testing.T) {
		mc := m
		mc.Caps.RequiresThinkingAsText = true
		in := []Message{
			{Role: RoleAssistant, Content: BlockList{
				ThinkingBlock{ItemID: "rs_1", Text: "reasoned"},
			}},
		}
		out := Prepare(Request{Model: mc, Messages: sameOrigin(mc, in),
			Reasoning: ReasoningConfig{Retain: RetainAll}}).Messages
		assert.Equal(t, BlockList{TextBlock{Text: "reasoned"}}, out[0].Content)
	})
}

func TestNormalizeCallID(t *testing.T) {
	t.Parallel()

	caps := func(d Dialect) Capabilities { return Capabilities{Dialect: d} }

	t.Run("anthropic_sanitizes_and_caps", func(t *testing.T) {
		// every non [a-zA-Z0-9_-] rune maps to an underscore; a long id is capped
		assert.Equal(t, "a_b_1_", normalizeCallID("a+b/1=", caps(DialectAnthropic), "p", callForeignEndpoint))
		long := strings.Repeat("x", 100)
		out := normalizeCallID(long, caps(DialectAnthropic), "p", callForeignEndpoint)
		assert.Len(t, out, 64)
	})

	t.Run("chat_completions_pipe_joins_sanitized", func(t *testing.T) {
		assert.Equal(t, "call_a_fc_1", normalizeCallID("call.a|fc/1", caps(DialectOpenAICompletions), "p", callForeignEndpoint))
	})

	t.Run("chat_completions_overlong_pipe_hashes_suffix", func(t *testing.T) {
		long := strings.Repeat("a", 40)
		out := normalizeCallID(long+"|"+strings.Repeat("b", 30), caps(DialectOpenAICompletions), "p", callForeignEndpoint)
		assert.Len(t, out, 31+1+8)
	})

	t.Run("chat_completions_openai_bare_capped_at_40", func(t *testing.T) {
		long := strings.Repeat("y", 80)
		out := normalizeCallID(long, caps(DialectOpenAICompletions), "openai", callForeignEndpoint)
		assert.Len(t, out, maxCompatCallID)
	})
	t.Run("chat_completions_other_provider_bare_kept", func(t *testing.T) {
		long := strings.Repeat("y", 80)
		out := normalizeCallID(long, caps(DialectOpenAICompletions), "lmstudio", callForeignEndpoint)
		assert.Equal(t, long, out)
	})

	t.Run("responses_pipe_hashes_item_when_endpoint_foreign", func(t *testing.T) {
		id := "call.1|fc/9"
		out := normalizeCallID(id, caps(DialectOpenAIResponses), "p", callForeignEndpoint)
		assert.Equal(t, "call_1|fc_"+shortHash("fc/9"), out)
	})

	t.Run("responses_pipe_sanitizes_item_when_local", func(t *testing.T) {
		out := normalizeCallID("call.1|fc_9", caps(DialectOpenAIResponses), "p", callSameEndpoint)
		assert.Equal(t, "call_1|fc_9", out)
	})

	t.Run("responses_non_fc_item_dropped_when_local", func(t *testing.T) {
		out := normalizeCallID("call.1|rs_9", caps(DialectOpenAIResponses), "p", callSameEndpoint)
		assert.Equal(t, "call_1", out)
	})

	t.Run("responses_model_switch_drops_item_id", func(t *testing.T) {
		// same provider and dialect but a different model: pi drops the fc item so
		// openai pairing validation cannot reject it, instead of hashing like foreign
		out := normalizeCallID("call.1|fc_9", caps(DialectOpenAIResponses), "p", callForeignModel)
		assert.Equal(t, "call_1", out)
	})
}

// TestPrepareToolCallIDRewrite checks the rename map rewrites a foreign piped id
// on both the call and its matching result in one pass.
func TestPrepareToolCallIDRewrite(t *testing.T) {
	t.Parallel()

	m := Model{Provider: "openai", ID: "gpt-5", Caps: Capabilities{
		Dialect: DialectAnthropic, // target anthropic so ids are sanitized/capped
	}}
	in := []Message{
		{Role: RoleAssistant, Content: BlockList{
			ToolCallBlock{ID: "call.1|fc/9", Name: "read", Input: json.RawMessage(`{}`)},
		}},
		{Role: RoleUser, Content: BlockList{
			ToolResultBlock{CallID: "call.1|fc/9", Content: BlockList{TextBlock{Text: "out"}}},
		}},
	}
	out := Prepare(Request{Model: m, Messages: foreignOrigin(m, in),
		Reasoning: ReasoningConfig{Retain: RetainAll}}).Messages
	nid := normalizeCallID("call.1|fc/9", m.Caps, "", callForeignEndpoint)
	assert.Equal(t, nid, out[0].Content[0].(ToolCallBlock).ID)
	assert.Equal(t, nid, out[1].Content[0].(ToolResultBlock).CallID)
}

// TestPrepareSameOriginResultFollowsRename pins A3: a tool result whose own
// origin matches the target still follows the call id its foreign caller was
// renamed to earlier in the same pass.
func TestPrepareSameOriginResultFollowsRename(t *testing.T) {
	t.Parallel()

	m := Model{Provider: "openai", ID: "gpt-5", Caps: Capabilities{
		Dialect: DialectAnthropic, Reasoning: true,
	}}
	assistant := foreignOrigin(m, []Message{{Role: RoleAssistant, Content: BlockList{
		ThinkingBlock{Text: "t"},
		ToolCallBlock{ID: "call.1|fc/9", Name: "read", Input: json.RawMessage(`{}`)},
	}}})[0]
	result := sameOrigin(m, []Message{{Role: RoleUser, Content: BlockList{
		ToolResultBlock{CallID: "call.1|fc/9", Content: BlockList{TextBlock{Text: "out"}}},
	}}})[0] // the result is stamped as produced by the target

	out := Prepare(Request{Model: m, Messages: []Message{assistant, result},
		Reasoning: ReasoningConfig{Retain: RetainAll}}).Messages
	nid := normalizeCallID("call.1|fc/9", m.Caps, "openai", callForeignEndpoint)
	assert.Equal(t, nid, out[0].Content[1].(ToolCallBlock).ID)
	assert.Equal(t, nid, out[1].Content[0].(ToolResultBlock).CallID)
}

// TestPrepareDoesNotMutateInputBlocks pins A4: the placeholder ladder must not
// write into the caller's backing array.
func TestPrepareDoesNotMutateInputBlocks(t *testing.T) {
	t.Parallel()

	m := Model{Provider: "p", ID: "m", Caps: Capabilities{}}
	in := []Message{{Role: RoleUser, Content: BlockList{
		ToolResultBlock{CallID: "c1"}, // empty content triggers the placeholder ladder
	}}}
	wantContent := slices.Clone(in[0].Content)

	Prepare(Request{Model: m, Messages: in})
	assert.Equal(t, wantContent, in[0].Content) // caller's block slice untouched
}

func TestDowngradeImages(t *testing.T) {
	t.Parallel()

	m := Model{Provider: "p", ID: "m", Caps: Capabilities{}}
	img := func() ImageBlock { return ImageBlock{MediaType: "image/png", Data: []byte{1}} }

	t.Run("user_image_becomes_placeholder", func(t *testing.T) {
		in := []Message{{Role: RoleUser, Content: BlockList{
			TextBlock{Text: "look"},
			img(),
		}}}
		out := Prepare(Request{Model: m, Messages: in}).Messages
		assert.Equal(t, BlockList{
			TextBlock{Text: "look"},
			TextBlock{Text: imageOmitted},
		}, out[0].Content)
	})

	t.Run("run_of_images_collapses", func(t *testing.T) {
		in := []Message{{Role: RoleUser, Content: BlockList{img(), img(), TextBlock{Text: "x"}, img()}}}
		out := Prepare(Request{Model: m, Messages: in}).Messages
		assert.Equal(t, BlockList{
			TextBlock{Text: imageOmitted},
			TextBlock{Text: "x"},
			TextBlock{Text: imageOmitted},
		}, out[0].Content)
	})

	t.Run("existing_placeholder_suppresses_next", func(t *testing.T) {
		in := []Message{{Role: RoleUser, Content: BlockList{
			TextBlock{Text: imageOmitted}, img(),
		}}}
		out := Prepare(Request{Model: m, Messages: in}).Messages
		assert.Equal(t, BlockList{TextBlock{Text: imageOmitted}}, out[0].Content)
	})

	t.Run("tool_result_image_downgraded", func(t *testing.T) {
		in := []Message{{Role: RoleUser, Content: BlockList{
			ToolResultBlock{CallID: "c1", Content: BlockList{img()}},
		}}}
		out := Prepare(Request{Model: m, Messages: in}).Messages
		assert.Equal(t, toolImageOmitted, out[0].Content[0].(ToolResultBlock).Content[0].(TextBlock).Text)
	})

	t.Run("image_capable_model_untouched", func(t *testing.T) {
		mc := m
		mc.Caps.Images = true
		in := []Message{{Role: RoleUser, Content: BlockList{img()}}}
		out := Prepare(Request{Model: mc, Messages: in}).Messages
		assert.Equal(t, img(), out[0].Content[0])
	})
}

func TestRepairTurns(t *testing.T) {
	t.Parallel()

	m := Model{Provider: "p", ID: "m", Caps: Capabilities{}}

	t.Run("orphan_at_end_of_conversation", func(t *testing.T) {
		in := []Message{{Role: RoleAssistant, Content: BlockList{
			ToolCallBlock{ID: "c1", Name: "read", Input: json.RawMessage(`{}`)},
		}}}
		out := Prepare(Request{Model: m, Messages: in}).Messages
		require.Len(t, out, 2)
		assert.Equal(t, RoleUser, out[1].Role)
		tr := out[1].Content[0].(ToolResultBlock)
		assert.True(t, tr.IsError)
		assert.Equal(t, noResult, tr.Content[0].(TextBlock).Text)
	})

	t.Run("orphan_before_next_assistant", func(t *testing.T) {
		in := []Message{
			{Role: RoleAssistant, Content: BlockList{ToolCallBlock{ID: "c1", Name: "read"}}},
			{Role: RoleAssistant, Content: BlockList{TextBlock{Text: "done"}}},
		}
		out := Prepare(Request{Model: m, Messages: in}).Messages
		assert.Equal(t, RoleUser, out[1].Role)
		assert.Equal(t, RoleAssistant, out[2].Role)
	})

	t.Run("partially_answered_call_synthesized", func(t *testing.T) {
		in := []Message{
			{Role: RoleAssistant, Content: BlockList{ToolCallBlock{ID: "c1"}, ToolCallBlock{ID: "c2"}}},
			{Role: RoleUser, Content: BlockList{ToolResultBlock{CallID: "c1", Content: BlockList{TextBlock{Text: "r"}}}}},
		}
		out := Prepare(Request{Model: m, Messages: in}).Messages
		var syn bool
		for _, msg := range out {
			for _, b := range msg.Content {
				if tr, ok := b.(ToolResultBlock); ok && tr.CallID == "c2" {
					syn = true
				}
			}
		}
		assert.True(t, syn)
	})

	t.Run("errored_turn_skipped_with_results", func(t *testing.T) {
		in := []Message{
			{Role: RoleAssistant, Stop: StopError, Content: BlockList{ToolCallBlock{ID: "c1"}}},
			{Role: RoleUser, Content: BlockList{ToolResultBlock{CallID: "c1", Content: BlockList{TextBlock{Text: "x"}}}}},
			{Role: RoleAssistant, Content: BlockList{TextBlock{Text: "fine"}}},
		}
		out := Prepare(Request{Model: m, Messages: in}).Messages
		require.Len(t, out, 1)
		assert.Equal(t, RoleAssistant, out[0].Role)
		assert.Equal(t, BlockList{TextBlock{Text: "fine"}}, out[0].Content)
	})

	t.Run("aborted_turn_kept", func(t *testing.T) {
		in := []Message{{Role: RoleAssistant, Stop: StopAborted, Content: BlockList{
			ToolCallBlock{ID: "c1"}, TextBlock{Text: "partial"},
		}}}
		out := Prepare(Request{Model: m, Messages: in}).Messages
		assert.Len(t, out, 2) // the call is orphaned and synthesized a result
	})

	t.Run("bridging_assistant_inserted", func(t *testing.T) {
		mc := Model{Provider: "p", ID: "m", Caps: Capabilities{
			RequiresAssistantAfterToolResult: true,
		}}
		in := []Message{
			{Role: RoleUser, Content: BlockList{ToolResultBlock{CallID: "c1", Content: BlockList{TextBlock{Text: "r"}}}}},
			{Role: RoleUser, Content: BlockList{TextBlock{Text: "next question"}}},
		}
		out := Prepare(Request{Model: mc, Messages: in}).Messages
		require.Len(t, out, 3) // result, bridging assistant, then the user question
		tb, ok := out[1].Content[0].(TextBlock)
		assert.True(t, ok)
		assert.Equal(t, processedTools, tb.Text)
	})
	t.Run("user_after_user_no_bridge", func(t *testing.T) {
		mc := Model{Provider: "p", ID: "m", Caps: Capabilities{
			RequiresAssistantAfterToolResult: true,
		}}
		in := []Message{
			Text(RoleUser, "hello"),
			Text(RoleUser, "how are you"), // a plain user turn is not a tool-result follow on
		}
		out := Prepare(Request{Model: mc, Messages: in}).Messages
		// a plain user turn is not a tool-result follow on, so no bridge is inserted
		require.Len(t, out, 2)
		assert.Equal(t, RoleUser, out[1].Role)
	})
	t.Run("assistant_between_results_and_user_no_bridge", func(t *testing.T) {
		mc := Model{Provider: "p", ID: "m", Caps: Capabilities{
			RequiresAssistantAfterToolResult: true,
		}}
		in := []Message{
			{Role: RoleUser, Content: BlockList{ToolResultBlock{CallID: "c1", Content: BlockList{TextBlock{Text: "r"}}}}},
			Text(RoleAssistant, "processed"), // an assistant reply already separates the turns
			Text(RoleUser, "next question"),
		}
		out := Prepare(Request{Model: mc, Messages: in}).Messages
		require.Len(t, out, 3)
		assert.Equal(t, RoleAssistant, out[1].Role)
		// the assistant reply already separates the turns, so no bridge is added
		assert.NotEqual(t, processedTools, out[2].Content[0].(TextBlock).Text)
	})
}

func TestToolResultPlaceholderLadder(t *testing.T) {
	t.Parallel()

	m := Model{Provider: "p", ID: "m", Caps: Capabilities{}}
	result := func(content BlockList) Message {
		return Message{Role: RoleUser, Content: BlockList{ToolResultBlock{CallID: "c1", Content: content}}}
	}

	t.Run("empty_content_gets_no_output", func(t *testing.T) {
		out := Prepare(Request{Model: m, Messages: []Message{result(nil)}}).Messages
		tr := out[0].Content[0].(ToolResultBlock)
		assert.Equal(t, "(no tool output)", tr.Content[0].(TextBlock).Text)
	})

	t.Run("image_only_gets_no_output", func(t *testing.T) {
		// image-only content downgrades to the placeholder text first, which is not
		// "attached" since images are unsupported here
		out := Prepare(Request{Model: m, Messages: []Message{
			result(BlockList{ImageBlock{MediaType: "image/png", Data: []byte{1}}}),
		}}).Messages
		tr := out[0].Content[0].(ToolResultBlock)
		assert.Equal(t, toolImageOmitted, tr.Content[0].(TextBlock).Text)
	})

	t.Run("text_content_unchanged", func(t *testing.T) {
		out := Prepare(Request{Model: m, Messages: []Message{
			result(BlockList{TextBlock{Text: "has text"}}),
		}}).Messages
		assert.Equal(t, BlockList{TextBlock{Text: "has text"}}, out[0].Content[0].(ToolResultBlock).Content)
	})
}

func TestPrepareIdempotent(t *testing.T) {
	t.Parallel()

	m := Model{Provider: "openai", ID: "gpt-5", Caps: Capabilities{
		Dialect: DialectOpenAIResponses, Reasoning: true,
	}}
	in := []Message{
		{Role: RoleAssistant, Content: BlockList{
			ThinkingBlock{Text: "t"},
			ToolCallBlock{ID: "call.1|fc/9", Name: "read"},
		}},
	}
	req := Request{Model: m, Messages: foreignOrigin(m, in), Reasoning: ReasoningConfig{Retain: RetainAll}}
	once := Prepare(req)
	twice := Prepare(Prepare(req))
	assert.Equal(t, once.Messages, twice.Messages)
}

func TestPrepareToolResultImageSplit(t *testing.T) {
	t.Parallel()

	imgMsg := func(dialect Dialect, images bool) Model {
		return Model{Provider: "p", ID: "m", Caps: Capabilities{Dialect: dialect, Images: images}}
	}
	toolImage := Message{Role: RoleUser, Content: BlockList{
		ToolResultBlock{CallID: "c1", Content: BlockList{
			ImageBlock{MediaType: "image/png", Data: []byte{1}},
		}},
	}}

	t.Run("compat_splits_images_into_user_message", func(t *testing.T) {
		out := Prepare(Request{Model: imgMsg(DialectOpenAICompletions, true),
			Messages: []Message{toolImage}}).Messages
		require.Len(t, out, 2)
		// the result keeps its placeholder and no longer carries the image
		tr := out[0].Content[0].(ToolResultBlock)
		assert.Equal(t, "(see attached image)", tr.Content[0].(TextBlock).Text)
		require.Len(t, tr.Content, 1)
		// a following user message carries the images with pi's lead-in
		assert.Equal(t, RoleUser, out[1].Role)
		tb := out[1].Content[0].(TextBlock)
		assert.Equal(t, "Attached image(s) from tool result:", tb.Text)
		img, ok := out[1].Content[1].(ImageBlock)
		require.True(t, ok)
		assert.Equal(t, []byte{1}, img.Data)
	})
	t.Run("anthropic_keeps_images_in_result", func(t *testing.T) {
		out := Prepare(Request{Model: imgMsg(DialectAnthropic, true),
			Messages: []Message{toolImage}}).Messages
		require.Len(t, out, 1)
		tr := out[0].Content[0].(ToolResultBlock)
		assert.True(t, hasImage(tr.Content)) // image stayed put for anthropic
	})
	t.Run("responses_keeps_images_in_result", func(t *testing.T) {
		out := Prepare(Request{Model: imgMsg(DialectOpenAIResponses, true),
			Messages: []Message{toolImage}}).Messages
		require.Len(t, out, 1)
		tr := out[0].Content[0].(ToolResultBlock)
		assert.True(t, hasImage(tr.Content)) // image stayed put for responses
	})
	t.Run("no_split_without_image_capability", func(t *testing.T) {
		m := Model{Provider: "p", ID: "m", Caps: Capabilities{Dialect: DialectOpenAICompletions, Images: false}}
		out := Prepare(Request{Model: m, Messages: []Message{toolImage}}).Messages
		require.Len(t, out, 1)
		tr := out[0].Content[0].(ToolResultBlock)
		assert.Equal(t, toolImageOmitted, tr.Content[0].(TextBlock).Text)
	})
	t.Run("split_is_idempotent", func(t *testing.T) {
		req := Request{Model: imgMsg(DialectOpenAICompletions, true),
			Messages: []Message{toolImage}}
		once := Prepare(req)
		twice := Prepare(Prepare(req))
		assert.Equal(t, once.Messages, twice.Messages)
	})
	t.Run("does_not_mutate_input_blocks", func(t *testing.T) {
		m := imgMsg(DialectOpenAICompletions, true)
		in := []Message{toolImage}
		Prepare(Request{Model: m, Messages: in})
		tr := in[0].Content[0].(ToolResultBlock)
		_, ok := tr.Content[0].(ImageBlock)
		assert.True(t, ok) // the caller's result still holds its image
	})
}

func TestPrepareToolImageBridgePlacement(t *testing.T) {
	t.Parallel()

	m := Model{Provider: "p", ID: "m", Caps: Capabilities{
		Dialect: DialectOpenAICompletions, Images: true,
		RequiresAssistantAfterToolResult: true,
	}}
	in := []Message{
		{Role: RoleUser, Content: BlockList{
			ToolResultBlock{CallID: "c1", ToolName: "read", Content: BlockList{
				ImageBlock{MediaType: "image/png", Data: []byte{1}},
			}},
		}},
		Text(RoleUser, "what do you see"),
	}
	out := Prepare(Request{Model: m, Messages: in}).Messages
	require.Len(t, out, 4)
	assert.Equal(t, RoleAssistant, out[1].Role) // the bridge sits after tool results...
	tb, ok := out[1].Content[0].(TextBlock)
	require.True(t, ok)
	assert.Equal(t, processedTools, tb.Text)
	// ...ahead of the image-attachment user message that follows
	assert.Equal(t, RoleUser, out[2].Role)
	assert.Contains(t, out[3].Content[0].(TextBlock).Text, "what do you see")
}
