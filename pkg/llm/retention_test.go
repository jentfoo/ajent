package llm

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retentionFixture is one completed turn followed by an in progress tool
// calling turn, which is what makes the wholeTurn boundary observable.
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
			ToolResultBlock{CallID: "c1", Content: BlockList{TextBlock{Text: "file"}}},
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

func TestApplyRetention(t *testing.T) {
	t.Parallel()

	anthropic := Capabilities{Reasoning: ReasoningAnthropicBudget, ReasoningReplay: true}
	openai := Capabilities{Reasoning: ReasoningOpenAIEffort, ReasoningReplay: true}
	openrouter := Capabilities{Reasoning: ReasoningOpenRouter}
	inline := Capabilities{Reasoning: ReasoningInlineTags}
	none := Capabilities{Reasoning: ReasoningNone}

	tests := []struct {
		name     string
		policy   RetainPolicy
		caps     Capabilities
		expected []string
	}{
		// replay providers upgrade none and lastTurn to wholeTurn
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
			got := applyRetention(retentionFixture(), tc.policy, tc.caps)
			assert.Equal(t, tc.expected, thinkingTexts(got))
		})
	}

	t.Run("input_is_not_mutated", func(t *testing.T) {
		msgs := retentionFixture()
		applyRetention(msgs, RetainNone, Capabilities{Reasoning: ReasoningOpenRouter})
		assert.Equal(t, retentionFixture(), msgs)
	})
	t.Run("unchanged_returns_same_slice", func(t *testing.T) {
		msgs := retentionFixture()
		got := applyRetention(msgs, RetainAll, Capabilities{Reasoning: ReasoningOpenRouter})
		require.Len(t, got, len(msgs))
		assert.Equal(t, msgs, got)
	})
	t.Run("unsigned_thinking_dropped", func(t *testing.T) {
		msgs := []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{
				ThinkingBlock{Text: "no signature"},
				TextBlock{Text: "a"},
			}},
		}
		got := applyRetention(msgs, RetainAll, Capabilities{Reasoning: ReasoningAnthropicBudget, ReasoningReplay: true})
		assert.Empty(t, thinkingTexts(got))
		assert.Equal(t, BlockList{TextBlock{Text: "a"}}, got[1].Content)
	})
	t.Run("redacted_only_is_replayable", func(t *testing.T) {
		msgs := []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{ThinkingBlock{Redacted: "cmVk"}}},
		}
		got := applyRetention(msgs, RetainAll, Capabilities{Reasoning: ReasoningAnthropicBudget, ReasoningReplay: true})
		require.Len(t, got, 2)
		assert.Len(t, got[1].Content, 1)
	})
	t.Run("emptied_message_dropped", func(t *testing.T) {
		msgs := []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{ThinkingBlock{Text: "only thinking"}}},
			Text(RoleUser, "q2"),
		}
		got := applyRetention(msgs, RetainNone, Capabilities{Reasoning: ReasoningOpenRouter})
		require.Len(t, got, 2)
		assert.Equal(t, RoleUser, got[0].Role)
		assert.Equal(t, RoleUser, got[1].Role)
	})
	t.Run("tool_call_survives_stripping", func(t *testing.T) {
		msgs := []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{
				ThinkingBlock{Text: "t"},
				ToolCallBlock{ID: "c1", Name: "read", Input: json.RawMessage(`{}`)},
			}},
		}
		got := applyRetention(msgs, RetainNone, Capabilities{Reasoning: ReasoningOpenRouter})
		require.Len(t, got, 2)
		assert.Equal(t, BlockList{ToolCallBlock{ID: "c1", Name: "read", Input: json.RawMessage(`{}`)}}, got[1].Content)
	})
	t.Run("reasoning_content_replay", func(t *testing.T) {
		msgs := []Message{
			Text(RoleUser, "q"),
			{Role: RoleAssistant, Content: BlockList{ThinkingBlock{Text: "deepseek style"}}},
		}
		caps := Capabilities{Reasoning: ReasoningContentField, ReplayReasoning: true}
		got := applyRetention(msgs, RetainAll, caps)
		assert.Equal(t, []string{"deepseek style"}, thinkingTexts(got))

		caps.ReplayReasoning = false
		got = applyRetention(msgs, RetainAll, caps)
		assert.Empty(t, thinkingTexts(got))
	})
}

func TestRetentionBounds(t *testing.T) {
	t.Parallel()

	t.Run("tool_results_are_not_boundaries", func(t *testing.T) {
		keepFrom, lastAssistant := retentionBounds(retentionFixture())
		assert.Equal(t, 2, keepFrom)
		assert.Equal(t, 5, lastAssistant)
	})
	t.Run("no_assistant_yet", func(t *testing.T) {
		keepFrom, lastAssistant := retentionBounds([]Message{Text(RoleUser, "q")})
		assert.Equal(t, 0, keepFrom)
		assert.Equal(t, -1, lastAssistant)
	})
	t.Run("empty", func(t *testing.T) {
		keepFrom, lastAssistant := retentionBounds(nil)
		assert.Equal(t, 0, keepFrom)
		assert.Equal(t, -1, lastAssistant)
	})
}
