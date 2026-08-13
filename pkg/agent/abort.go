package agent

import (
	"github.com/jentfoo/ajent/pkg/llm"
)

// interruptedText is the result content for a tool call abandoned by an
// interrupt, so the next request stays well formed.
const interruptedText = "interrupted by user"

// abortResults returns one tool_result per call in msg: its real result when
// results answers it, or a synthetic error marked interrupted otherwise. A
// partial assistant message with an unanswered tool_use makes the next Anthropic
// request 400, so every cancelled turn must fill them all.
func abortResults(msg llm.Message, results []llm.ToolResultBlock) []llm.ToolResultBlock {
	byCall := make(map[string]llm.ToolResultBlock, len(results))
	for _, r := range results {
		if r.CallID != "" {
			byCall[r.CallID] = r
		}
	}

	out := make([]llm.ToolResultBlock, 0, len(msg.Content))
	for _, b := range msg.Content {
		tc, ok := b.(llm.ToolCallBlock)
		if !ok || tc.ID == "" {
			continue
		}
		if real, found := byCall[tc.ID]; found {
			out = append(out, real) // call order preserved regardless of completion order
			continue
		}
		out = append(out, llm.ToolResultBlock{
			CallID: tc.ID, ToolName: tc.Name,
			IsError: true,
			Content: llm.BlockList{llm.TextBlock{Text: interruptedText}},
		})
	}
	return out
}
