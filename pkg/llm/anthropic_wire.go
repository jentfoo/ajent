package llm

import "encoding/json"

const (
	antTypeText       = "text"
	antTypeImage      = "image"
	antTypeToolUse    = "tool_use"
	antTypeToolRes    = "tool_result"
	antTypeThinking   = "thinking"
	antTypeRedacted   = "redacted_thinking"
	antCacheEphemeral = "ephemeral"

	// extended-thinking shape names the Messages API accepts
	antThinkEnabled  = "enabled"
	antThinkAdaptive = "adaptive"
	antThinkDisabled = "disabled"
)

// antRequest is a Messages API request body.
type antRequest struct {
	Model        string           `json:"model"`
	MaxTokens    int              `json:"max_tokens"`
	System       []antBlock       `json:"system,omitempty"`
	Messages     []antMessage     `json:"messages"`
	Tools        []antTool        `json:"tools,omitempty"`
	ToolChoice   *antToolChoi     `json:"tool_choice,omitempty"`
	Thinking     *antThinking     `json:"thinking,omitempty"`
	OutputConfig *antOutputConfig `json:"output_config,omitempty"`
	Temperature  *float64         `json:"temperature,omitempty"`
	Stream       bool             `json:"stream"`
	Metadata     *antMetadata     `json:"metadata,omitempty"`
}

type antMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

// antThinking selects the extended reasoning shape: an explicit token budget, the
// adaptive mode the newest models require, or off. Display summarizes thinking so
// it streams as text instead of being omitted (the API default).
type antThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"` // only for the enabled shape
	Display      string `json:"display,omitempty"`
}

// antOutputConfig carries the effort level adaptive thinking is driven by.
type antOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

type antToolChoi struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// antTool is a tool definition.
type antTool struct {
	Name                string          `json:"name"`
	Description         string          `json:"description,omitempty"`
	InputSchema         json.RawMessage `json:"input_schema,omitempty"`
	EagerInputStreaming *bool           `json:"eager_input_streaming,omitempty"`
	CacheControl        *antCache       `json:"cache_control,omitempty"`
}

// antCache marks a prompt cache breakpoint.
type antCache struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"` // empty means the default five minute tier
}

type antMessage struct {
	Role    string     `json:"role"`
	Content []antBlock `json:"content"`
}

// antBlock is one content block. Only the fields for its type are set.
type antBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`
	Signature    *string         `json:"signature,omitempty"` // pointer so an explicit empty signature survives
	Data         string          `json:"data,omitempty"`      // redacted_thinking payload
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      []antBlock      `json:"content,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	Source       *antSource      `json:"source,omitempty"`
	CacheControl *antCache       `json:"cache_control,omitempty"`
}

// antSource carries inline image bytes.
type antSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// antEvent is one decoded SSE frame from the Messages API.
type antEvent struct {
	Type         string      `json:"type"`
	Index        int         `json:"index"`
	Message      *antRespMsg `json:"message"`
	ContentBlock *antBlock   `json:"content_block"`
	Delta        *antDelta   `json:"delta"`
	Usage        *antUsage   `json:"usage"`
	Error        *antError   `json:"error"`
}

type antRespMsg struct {
	ID         string    `json:"id"`
	Model      string    `json:"model"`
	Role       string    `json:"role"`
	StopReason string    `json:"stop_reason"`
	Usage      *antUsage `json:"usage"`
}

// antDelta carries either content or, on message_delta, the stop reason.
type antDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`
	Signature   string `json:"signature"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

type antUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheCreation            struct {
		Ephemeral5m *int `json:"ephemeral_5m_input_tokens,omitempty"`
		Ephemeral1h *int `json:"ephemeral_1h_input_tokens,omitempty"`
	} `json:"cache_creation,omitempty"`
	OutputTokensDetails struct {
		ThinkingTokens int `json:"thinking_tokens,omitempty"`
	} `json:"output_tokens_details,omitempty"`
}

// toUsage converts a wire usage report.
func (u *antUsage) toUsage() Usage {
	if u == nil {
		return Usage{}
	}
	cacheWrite := u.CacheCreationInputTokens
	if cacheWrite == 0 { // older shape nests the creation split instead
		if u.CacheCreation.Ephemeral5m != nil {
			cacheWrite = *u.CacheCreation.Ephemeral5m
		} else if u.CacheCreation.Ephemeral1h != nil {
			cacheWrite = *u.CacheCreation.Ephemeral1h
		}
	}
	return Usage{
		Input:      u.InputTokens,
		Output:     u.OutputTokens,
		CacheRead:  u.CacheReadInputTokens,
		CacheWrite: cacheWrite,
		Reasoning:  u.OutputTokensDetails.ThinkingTokens,
	}
}

type antError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// antErrBody is the top level of an error response.
type antErrBody struct {
	Error *antError `json:"error"`
}

// antCountResponse is the count_tokens result.
type antCountResponse struct {
	InputTokens int `json:"input_tokens"`
}

// antStopReason maps a Messages API stop reason.
func antStopReason(s string) StopReason {
	switch s {
	case "end_turn", "stop_sequence", "refusal":
		return StopEndTurn
	case "tool_use", "pause_turn":
		return StopToolUse
	case "max_tokens":
		return StopMaxTokens
	case "":
		return StopUnknown
	default:
		return StopEndTurn
	}
}
