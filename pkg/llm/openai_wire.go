package llm

import "encoding/json"

const (
	respTypeMessage    = "message"
	respTypeFunction   = "function_call"
	respTypeFuncOutput = "function_call_output"
	respTypeReasoning  = "reasoning"
	respInputText      = "input_text"
	respOutputText     = "output_text"
	respInputImage     = "input_image"
	// respEncryptedInclude asks for the reasoning payload needed to replay a
	// turn without the server storing it.
	respEncryptedInclude = "reasoning.encrypted_content"
)

// respRequest is a Responses API request body.
type respRequest struct {
	Model           string         `json:"model"`
	Input           []respItem     `json:"input"`
	Instructions    string         `json:"instructions,omitempty"`
	Tools           []respTool     `json:"tools,omitempty"`
	ToolChoice      any            `json:"tool_choice,omitempty"`
	MaxOutputTokens *int           `json:"max_output_tokens,omitempty"`
	Temperature     *float64       `json:"temperature,omitempty"`
	Reasoning       *respReasoning `json:"reasoning,omitempty"`
	Include         []string       `json:"include,omitempty"`
	Store           *bool          `json:"store,omitempty"`
	Stream          bool           `json:"stream"`
	ParallelTools   *bool          `json:"parallel_tool_calls,omitempty"`
}

// respReasoning asks for reasoning at a given effort.
type respReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// respItem is one element of the input list. Only the fields for its type are
// set, which is why they are all optional.
type respItem struct {
	Type    string        `json:"type"`
	Role    string        `json:"role,omitempty"`
	Content []respContent `json:"content,omitempty"`

	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`

	EncryptedContent string `json:"encrypted_content,omitempty"`
	Summary          []any  `json:"summary,omitempty"`
}

// respContent is one piece of message content.
type respContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// respTool is a tool definition. Unlike chat-completions it is flat, not nested
// under a function key.
type respTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// respEvent is one decoded Responses API stream frame.
type respEvent struct {
	Type           string       `json:"type"`
	OutputIndex    int          `json:"output_index"`
	ItemID         string       `json:"item_id"`
	Delta          string       `json:"delta"`
	Item           *respItem    `json:"item"`
	Response       *respPayload `json:"response"`
	SequenceNumber int          `json:"sequence_number"`
	Message        string       `json:"message"`
	Code           string       `json:"code"`
}

// respPayload is the response object carried by lifecycle frames.
type respPayload struct {
	ID     string     `json:"id"`
	Model  string     `json:"model"`
	Status string     `json:"status"`
	Usage  *respUsage `json:"usage"`
	Error  *respError `json:"error"`
	Output []respItem `json:"output"`
}

type respError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type respUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// toUsage converts a wire usage report.
func (u *respUsage) toUsage() Usage {
	if u == nil {
		return Usage{}
	}
	return Usage{
		Input:     u.InputTokens - u.InputTokensDetails.CachedTokens,
		Output:    u.OutputTokens,
		CacheRead: u.InputTokensDetails.CachedTokens,
		Reasoning: u.OutputTokensDetails.ReasoningTokens,
	}
}

// respStopReason maps a terminal response status.
func respStopReason(status string, sawToolCall bool) StopReason {
	switch status {
	case "incomplete":
		return StopMaxTokens
	case "failed":
		return StopError
	default:
		if sawToolCall {
			return StopToolUse
		}
		return StopEndTurn
	}
}
