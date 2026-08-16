// Package srv serves a scripted OpenAI-compatible chat service: a standalone
// HTTP server that speaks the chat-completions SSE dialect and plays a fixed
// sequence of real tool calls, so an agent harness runs its whole loop against it.
package srv

import "encoding/json"

// chatRequest is the subset of a chat-completions request the script reads.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream,omitempty"`
	Tools    []toolSpec    `json:"tools,omitempty"` // advertised tools; absent on classifier calls
}

// toolSpec is one advertised function definition.
type toolSpec struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// chatMessage is one message on the wire.
type chatMessage struct {
	Role      string          `json:"role"` // system | user | assistant | tool
	Content   any             `json:"content,omitempty"`
	ToolCalls []assistantCall `json:"tool_calls,omitempty"`
}

// assistantCall is a prior tool invocation carried back in context.
type assistantCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function callInvocation `json:"function"`
}

type callInvocation struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"` // object or a JSON-encoded string
}

// chunk is one streamed chat-completions delta frame.
type chunk struct {
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Choices []choiceDelta `json:"choices"`
	Usage   *usageReport  `json:"usage,omitempty"`
}

type choiceDelta struct {
	Index        int    `json:"index"`
	Delta        delta  `json:"delta"`
	FinishReason string `json:"finish_reason"`
}

// delta is the incremental content of one choice.
type delta struct {
	Content   string      `json:"content,omitempty"`
	Reasoning string      `json:"reasoning_content,omitempty"`
	ToolCalls []callDelta `json:"tool_calls,omitempty"`
}

// callDelta carries a tool-call fragment keyed by an array index.
type callDelta struct {
	Index    int         `json:"index"`
	ID       string      `json:"id,omitempty"`
	Type     string      `json:"type,omitempty"`
	Function functionArg `json:"function"`
}

type functionArg struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// usageReport reports token counts on the final chunk.
type usageReport struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}
