package llm

import "encoding/json"

const (
	roleSystem    = "system"
	roleDeveloper = "developer"
	roleUser      = "user"
	roleAssistant = "assistant"
	roleTool      = "tool"
	typeFunction  = "function"
)

// compatRequest is a chat-completions request body.
type compatRequest struct {
	Model             string             `json:"model"`
	Messages          []compatMessage    `json:"messages"`
	Stream            bool               `json:"stream"`
	StreamOptions     *compatStreamOpts  `json:"stream_options,omitempty"`
	MaxTokens         *int               `json:"max_tokens,omitempty"`
	MaxCompletion     *int               `json:"max_completion_tokens,omitempty"`
	Temperature       *float64           `json:"temperature,omitempty"`
	Tools             []compatTool       `json:"tools,omitempty"`
	ToolChoice        any                `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool              `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort   string             `json:"reasoning_effort,omitempty"`
	Reasoning         *compatReasoning   `json:"reasoning,omitempty"`
	ChatTemplateKwarg map[string]any     `json:"chat_template_kwargs,omitempty"`
	Provider          *compatRouting     `json:"provider,omitempty"`
	Usage             *compatUsageOption `json:"usage,omitempty"`
	CachePrompt       *bool              `json:"cache_prompt,omitempty"`
}

// compatReasoning is the openrouter reasoning parameter.
type compatReasoning struct {
	Effort    string `json:"effort,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	Exclude   bool   `json:"exclude,omitempty"`
}

// compatRouting is openrouter's upstream routing preference.
type compatRouting struct {
	Order             []string `json:"order,omitempty"`
	AllowFallbacks    *bool    `json:"allow_fallbacks,omitempty"`
	DataCollection    string   `json:"data_collection,omitempty"`
	RequireParameters *bool    `json:"require_parameters,omitempty"`
}

type compatUsageOption struct {
	Include bool `json:"include"`
}

type compatStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

// compatMessage is one message on the wire.
type compatMessage struct {
	Role             string           `json:"role"`
	Content          any              `json:"content,omitempty"` // string, or a part list
	ToolCalls        []compatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ReasoningDetails json.RawMessage  `json:"reasoning_details,omitempty"`
	Name             string           `json:"name,omitempty"`
}

// compatPart is one piece of multimodal content.
type compatPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *compatImageURL `json:"image_url,omitempty"`
}

type compatImageURL struct {
	URL string `json:"url"`
}

// compatToolCall is a tool invocation on an assistant message.
type compatToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Index    int                `json:"index,omitempty"`
	Function compatToolFunction `json:"function"`
}

type compatToolFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// compatTool is a tool definition.
type compatTool struct {
	Type     string           `json:"type"`
	Function compatToolSchema `json:"function"`
}

type compatToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// compatChunk is one streamed chat-completions chunk.
type compatChunk struct {
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Choices []compatChoi  `json:"choices"`
	Usage   *compatUsage  `json:"usage"`
	Error   *compatErrObj `json:"error"`
}

type compatChoi struct {
	Index        int         `json:"index"`
	Delta        compatDelta `json:"delta"`
	FinishReason string      `json:"finish_reason"`
}

// compatDelta is the incremental content of one choice.
type compatDelta struct {
	Content          string           `json:"content"`
	Reasoning        string           `json:"reasoning"`
	ReasoningContent string           `json:"reasoning_content"`
	ReasoningDetails json.RawMessage  `json:"reasoning_details"`
	ToolCalls        []compatToolCall `json:"tool_calls"`
}

// compatUsage is the token report on the final chunk.
type compatUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// toUsage converts a wire usage report.
func (u *compatUsage) toUsage() Usage {
	if u == nil {
		return Usage{}
	}
	return Usage{
		Input:      u.PromptTokens - u.PromptTokensDetails.CachedTokens,
		Output:     u.CompletionTokens,
		CacheRead:  u.PromptTokensDetails.CachedTokens,
		Reasoning:  u.CompletionTokensDetails.ReasoningTokens,
		CacheWrite: 0,
	}
}

// compatErrObj is a vendor error payload, shared by the error body and the
// mid-stream error frame.
type compatErrObj struct {
	Message  string          `json:"message"`
	Type     string          `json:"type"`
	Code     any             `json:"code"`
	Metadata json.RawMessage `json:"metadata"`
}

// compatErrBody is the top level of an error response.
type compatErrBody struct {
	Error *compatErrObj `json:"error"`
}

// codeString renders the code field, which is a string on some servers and a
// number on others.
func (e *compatErrObj) codeString() string {
	if e == nil {
		return ""
	}
	switch v := e.Code.(type) {
	case string:
		return v
	case float64:
		return ""
	default:
		return ""
	}
}

// stopReasonFrom maps a chat-completions finish_reason.
func stopReasonFrom(s string) StopReason {
	switch s {
	case "stop", "end_turn":
		return StopEndTurn
	case "tool_calls", "function_call":
		return StopToolUse
	case "length", "max_tokens":
		return StopMaxTokens
	case "":
		return StopUnknown
	default:
		return StopEndTurn
	}
}
