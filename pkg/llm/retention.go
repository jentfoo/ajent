package llm

import "slices"

// ResolveRetain adjusts the requested retention policy to what caps accept. It
// returns RetainNone when reasoning is unsupported and upgrades a weaker policy
// to whole-turn when replay requires it.
func ResolveRetain(policy RetainPolicy, caps Capabilities) RetainPolicy {
	return resolveRetention(policy, caps)
}

// resolveRetention adjusts the policy to what caps can actually accept.
func resolveRetention(policy RetainPolicy, caps Capabilities) RetainPolicy {
	if !caps.Reasoning {
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
		if m.Role == RoleUser && !OnlyToolResults(m.Content) {
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
	switch caps.Dialect {
	case DialectAnthropic:
		return t.Signature != "" || t.Redacted != "" || caps.AllowEmptySignature
	case DialectOpenAIResponses:
		return t.ItemID != "" || len(t.Item) > 0
	}
	// a compat block with an originating field replays back to that field; this
	// replaces the deepseek-only replay gate once the source-field round trip lands.
	if t.Field != "" {
		return true
	}
	switch caps.Thinking {
	case ThinkingOpenRouter:
		return len(t.Details) > 0
	default:
		return false // inline tags are re-parsed from content, never replayed
	}
}

func hasThinking(blocks BlockList) bool {
	return slices.ContainsFunc(blocks, func(b Block) bool {
		return b.blockType() == BlockThinking
	})
}

func OnlyToolResults(blocks BlockList) bool {
	if len(blocks) == 0 {
		return false
	}
	return !slices.ContainsFunc(blocks, func(b Block) bool {
		return b.blockType() != BlockToolResult
	})
}
