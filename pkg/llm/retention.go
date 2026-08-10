package llm

import (
	"slices"

	"github.com/go-analyze/bulk"
)

// applyRetention returns messages with thinking blocks stripped per policy, at
// request build time only. It never mutates msgs, and returns it unchanged when
// nothing is stripped.
//
// A provider that cannot read thinking blocks downgrades to RetainNone, and one
// that must replay them upgrades to RetainWholeTurn, since either alternative
// makes the request invalid. A block carrying no replay token this provider
// accepts is dropped whatever the policy, since it cannot be sent at all.
func applyRetention(msgs []Message, policy RetainPolicy, caps Capabilities) []Message {
	policy = resolveRetention(policy, caps)
	keepFrom, lastAssistant := retentionBounds(msgs)

	var changed bool
	out := make([]Message, 0, len(msgs))
	for i, m := range msgs {
		if m.Role != RoleAssistant || !hasThinking(m.Content) {
			out = append(out, m)
			continue
		}
		keep := keepThinking(policy, i, keepFrom, lastAssistant)
		content := bulk.SliceFilter(func(b Block) bool {
			t, ok := b.(ThinkingBlock)
			return !ok || (keep && replayable(t, caps))
		}, m.Content)
		if len(content) == len(m.Content) {
			out = append(out, m)
			continue
		}
		changed = true
		if len(content) == 0 {
			continue // an empty assistant message is rejected by some providers
		}
		out = append(out, Message{Role: m.Role, Content: content})
	}
	if !changed {
		return msgs
	}
	return out
}

// resolveRetention adjusts the policy to what caps can actually accept.
func resolveRetention(policy RetainPolicy, caps Capabilities) RetainPolicy {
	if caps.Reasoning == ReasoningNone {
		return RetainNone
	} else if caps.ReasoningReplay && policy < RetainWholeTurn {
		return RetainWholeTurn
	}
	return policy
}

// retentionBounds returns the index the current turn starts at and the index of
// the last assistant message.
//
// A turn starts at a user message carrying something other than tool results,
// because anthropic delivers tool results as user role messages and treating
// those as turn boundaries collapses wholeTurn into lastTurn.
func retentionBounds(msgs []Message) (keepFrom, lastAssistant int) {
	lastAssistant = -1
	for i, m := range msgs {
		if m.Role == RoleUser && !onlyToolResults(m.Content) {
			keepFrom = i
		} else if m.Role == RoleAssistant {
			lastAssistant = i
		}
	}
	return keepFrom, lastAssistant
}

// keepThinking reports whether the assistant message at i keeps its thinking.
func keepThinking(policy RetainPolicy, i, keepFrom, lastAssistant int) bool {
	switch policy {
	case RetainLastTurn:
		return i == lastAssistant
	case RetainWholeTurn:
		return i >= keepFrom
	case RetainAll:
		return true
	default:
		return false
	}
}

// replayable reports whether the block carries the token this provider needs to
// accept it back. A provider rejects a thinking block it cannot verify.
func replayable(t ThinkingBlock, caps Capabilities) bool {
	switch caps.Reasoning {
	case ReasoningAnthropicBudget:
		return t.Signature != "" || t.Redacted != ""
	case ReasoningOpenAIEffort:
		return t.ItemID != ""
	case ReasoningOpenRouter:
		return len(t.Details) > 0
	case ReasoningContentField:
		return caps.ReplayReasoning && t.Text != ""
	default:
		return false // inline tags are re-parsed from content, never replayed
	}
}

func hasThinking(blocks BlockList) bool {
	return slices.ContainsFunc(blocks, func(b Block) bool {
		return b.blockType() == BlockThinking
	})
}

func onlyToolResults(blocks BlockList) bool {
	if len(blocks) == 0 {
		return false
	}
	return !slices.ContainsFunc(blocks, func(b Block) bool {
		return b.blockType() != BlockToolResult
	})
}
