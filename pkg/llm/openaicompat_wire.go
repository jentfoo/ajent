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
	ReasoningEffort   *string            `json:"reasoning_effort,omitempty"`
	Reasoning         *compatReasoning   `json:"reasoning,omitempty"`
	Thinking          any                `json:"thinking,omitempty"` // object or bare string
	EnableThinking    *bool              `json:"enable_thinking,omitempty"`
	ChatTemplateKwarg map[string]any     `json:"chat_template_kwargs,omitempty"`
	ChatTemplateArgs  map[string]any     `json:"chat_template_args,omitempty"`
	ToolStream        *bool              `json:"tool_stream,omitempty"`
	Provider          any                `json:"provider,omitempty"` // typed routing or verbatim compat JSON
	Usage             *compatUsageOption `json:"usage,omitempty"`
	CachePrompt       *bool              `json:"cache_prompt,omitempty"`

	// extra carries body keys whose name comes from configuration, folded in at
	// marshal time; unexported so encoding/json skips it
	extra map[string]json.RawMessage
}

// setExtra records one configuration named body key.
func (r *compatRequest) setExtra(key string, val json.RawMessage) {
	if r.extra == nil {
		r.extra = make(map[string]json.RawMessage, 1)
	}
	r.extra[key] = val
}

// compatReasoning is the openrouter reasoning parameter.
type compatReasoning struct {
	Effort    *string `json:"effort,omitempty"`
	MaxTokens int     `json:"max_tokens,omitempty"`
	Enabled   *bool   `json:"enabled,omitempty"` // together
}

// compatThinking is the object form of thinking for zai and deepseek.
type compatThinking struct {
	Type          string `json:"type"`
	ClearThinking *bool  `json:"clear_thinking,omitempty"`
}

// compatRouting is openrouter's upstream routing preference.
type compatRouting struct {
	Order               []string        `json:"order,omitempty"`
	AllowFallbacks      *bool           `json:"allow_fallbacks,omitempty"`
	DataCollection      string          `json:"data_collection,omitempty"`
	RequireParameters   *bool           `json:"require_parameters,omitempty"`
	Only                []string        `json:"only,omitempty"`
	Ignore              []string        `json:"ignore,omitempty"`
	Zdr                 *bool           `json:"zdr,omitempty"`
	EnforceDistillable  *bool           `json:"enforce_distillable_text,omitempty"`
	Quantizations       []string        `json:"quantizations,omitempty"`
	Sort                json.RawMessage `json:"sort,omitempty"`
	MaxPrice            json.RawMessage `json:"max_price,omitempty"`
	PreferredThroughput json.RawMessage `json:"preferred_min_throughput,omitempty"`
	PreferredLatency    json.RawMessage `json:"preferred_max_latency,omitempty"`
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
	Name             string           `json:"name,omitempty"`
	ReasoningDetails json.RawMessage  `json:"reasoning_details,omitempty"`

	// reasoning replay rides a provider-specific key (reasoning_content etc.),
	// so it is injected in MarshalJSON rather than carried by a fixed tag.
	reasoningField string
	reasoningText  string
}

// MarshalJSON injects the dynamic reasoning-replay key, whose name differs per
// provider and is resolved at build time onto reasoningField.
func (m compatMessage) MarshalJSON() ([]byte, error) {
	type wire compatMessage // alias so this method does not recurse
	data, err := json.Marshal((*wire)(&m))
	if err != nil {
		return nil, err
	}
	var out map[string]json.RawMessage
	if err = json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	if m.reasoningField != "" {
		v, _ := json.Marshal(m.reasoningText)
		out[m.reasoningField] = v
	}
	return json.Marshal(out)
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
	Strict      *bool           `json:"strict,omitempty"`
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
	ReasoningText    string           `json:"reasoning_text"` // chutes.ai spelling
	ReasoningDetails json.RawMessage  `json:"reasoning_details"`
	ToolCalls        []compatToolCall `json:"tool_calls"`
}

// compatUsage is the token report on the final chunk.
type compatUsage struct {
	PromptTokens         int  `json:"prompt_tokens"`
	CompletionTokens     int  `json:"completion_tokens"`
	PromptCacheHitTokens *int `json:"prompt_cache_hit_tokens,omitempty"` // deepseek spelling
	PromptTokensDetails  struct {
		CachedTokens     *int `json:"cached_tokens"` // pointer so a present zero wins over the fallback
		CacheWriteTokens *int `json:"cache_write_tokens,omitempty"`
	} `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
}

// toUsage converts a wire usage report.
func (u *compatUsage) toUsage() Usage {
	if u == nil {
		return Usage{}
	}
	cacheRead := 0
	if u.PromptTokensDetails.CachedTokens != nil {
		cacheRead = *u.PromptTokensDetails.CachedTokens
	} else if u.PromptCacheHitTokens != nil {
		cacheRead = *u.PromptCacheHitTokens
	}
	cacheWrite := 0
	if u.PromptTokensDetails.CacheWriteTokens != nil {
		cacheWrite = *u.PromptTokensDetails.CacheWriteTokens
	}
	return Usage{
		Input:      max(0, u.PromptTokens-cacheRead-cacheWrite),
		Output:     u.CompletionTokens,
		CacheRead:  cacheRead,
		CacheWrite: cacheWrite,
		Reasoning:  u.CompletionTokensDetails.ReasoningTokens,
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
