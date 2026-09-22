package llm

import (
	"encoding/json"
	"strings"
)

// copyCall is the clipboard shape of one tool call: the name and its verbatim input.
type copyCall struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// copyResult is the clipboard shape of one tool result. Content blocks render
// in their natural JSON, not the transcript envelope.
type copyResult struct {
	CallID  string            `json:"callId"`
	Content []json.RawMessage `json:"content"`
	IsError bool              `json:"isError,omitempty"`
}

// CopyTurn renders the newest assistant turn as clipboard text: text blocks
// verbatim, then every tool call followed by its paired result as JSON.
// Empty when there is no assistant message.
func CopyTurn(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleAssistant {
			return CopyAssistant(msgs, i)
		}
	}
	return ""
}

// CopyAssistant renders assistant message i as clipboard text, pairing each
// call with the result that follows it. A call with no result copies alone.
func CopyAssistant(msgs []Message, i int) string {
	results := resultsAfter(msgs, i)
	parts := make([]string, 0, len(msgs[i].Content)+1)
	if text := blocksText(msgs[i].Content); text != "" {
		parts = append(parts, text)
	}
	for _, b := range msgs[i].Content {
		call, ok := b.(ToolCallBlock)
		if !ok {
			continue
		}
		res, paired := results[call.ID]
		parts = append(parts, copyPairLines(call, res, true, paired)...)
	}
	return strings.Join(parts, "\n")
}

// CopyResults renders the tool results of message i with their matching calls,
// found by scanning backward through the conversation. A result whose call is
// missing copies alone.
func CopyResults(msgs []Message, i int) string {
	var parts []string
	for _, b := range msgs[i].Content {
		res, ok := b.(ToolResultBlock)
		if !ok {
			continue
		}
		call, paired := callFor(msgs, i, res.CallID)
		parts = append(parts, copyPairLines(call, res, paired, true)...)
	}
	return strings.Join(parts, "\n")
}

// copyPairLines renders a call and/or a result as clipboard JSON lines.
func copyPairLines(call ToolCallBlock, res ToolResultBlock, withCall, withResult bool) []string {
	var out []string
	if withCall {
		if s := marshalCopy(copyCall{Name: call.Name, Input: call.Input}); s != "" {
			out = append(out, s)
		}
	}
	if withResult {
		content := make([]json.RawMessage, 0, len(res.Content))
		for _, b := range res.Content {
			if data, err := json.Marshal(b); err == nil {
				content = append(content, data)
			}
		}
		if s := marshalCopy(copyResult{CallID: res.CallID, Content: content, IsError: res.IsError}); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// marshalCopy renders v as one clipboard JSON line, "" when it cannot.
func marshalCopy(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// resultsAfter collects the tool results answering assistant message i, keyed
// by call id. Scanning stops at the next assistant message.
func resultsAfter(msgs []Message, i int) map[string]ToolResultBlock {
	out := make(map[string]ToolResultBlock)
	for j := i + 1; j < len(msgs); j++ {
		if msgs[j].Role == RoleAssistant {
			break
		}
		for _, b := range msgs[j].Content {
			if r, ok := b.(ToolResultBlock); ok {
				out[r.CallID] = r
			}
		}
	}
	return out
}

// callFor finds the call carrying callID, scanning backward from message i.
func callFor(msgs []Message, i int, callID string) (ToolCallBlock, bool) {
	for j := i - 1; j >= 0; j-- {
		for _, b := range msgs[j].Content {
			if c, ok := b.(ToolCallBlock); ok && c.ID == callID {
				return c, true
			}
		}
	}
	return ToolCallBlock{}, false
}
