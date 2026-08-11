package llm

import "encoding/json"

// Capabilities is the fully resolved quirk set for one model: dialect defaults
// overlaid with the provider then model compat blocks from configuration. It is
// what an adapter reads instead of special casing a vendor.
type Capabilities struct {
	Reasoning       ReasoningStyle
	ReasoningReplay bool   // prior thinking blocks must be sent back
	ReasoningField  string // delta field carrying reasoning text, compat dialect
	ReplayReasoning bool   // echo reasoning_content on assistant messages
	ThinkOpen       string // inline reasoning tags, empty when unused
	ThinkClose      string
	// per-level override of the provider's reasoning value; nil omits it, absent uses the default
	LevelMap map[Level]*string

	PromptCache bool
	CacheFormat string // breakpoint encoding, empty when automatic
	LongCache   bool   // the extended cache retention tier is available

	Tokenizer TokenizerKind

	MaxTokensField string // max_tokens | max_completion_tokens | max_output_tokens
	SystemAsRole   bool   // system as a message rather than a top level field
	DeveloperRole  bool   // system messages use the developer role
	Temperature    bool
	ToolChoice     bool
	ParallelTools  bool
	StreamUsage    bool
	Store          bool
	Images         bool

	ExtraBody map[string]json.RawMessage // verbatim additions to the request body
}

// ReasoningStyle is how a provider encodes a reasoning request and response.
type ReasoningStyle uint8

const (
	ReasoningNone ReasoningStyle = iota
	ReasoningAnthropicBudget
	ReasoningOpenAIEffort
	ReasoningOpenRouter
	ReasoningInlineTags
	ReasoningContentField
)

var reasoningNames = enumNames[ReasoningStyle]{
	ReasoningNone:            "none",
	ReasoningAnthropicBudget: "anthropic_budget",
	ReasoningOpenAIEffort:    "openai_effort",
	ReasoningOpenRouter:      "openrouter",
	ReasoningInlineTags:      "inline_tags",
	ReasoningContentField:    "reasoning_content",
	ReasoningUnset:           "default",
}

// String returns the configuration name of the style.
func (s ReasoningStyle) String() string { return reasoningNames.name(s) }

// MarshalText encodes the style as its configuration name.
func (s ReasoningStyle) MarshalText() ([]byte, error) { return reasoningNames.marshalText(s) }

// UnmarshalText decodes the configuration name.
func (s *ReasoningStyle) UnmarshalText(data []byte) error {
	return reasoningNames.unmarshalText(data, s, "reasoning style")
}

// UnmarshalJSON also accepts a bool, so a model entry declaring
// "reasoning": true carries over unchanged.
func (s *ReasoningStyle) UnmarshalJSON(data []byte) error {
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		if b {
			*s = ReasoningUnset
		} else {
			*s = ReasoningNone
		}
		return nil
	}
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return err
	}
	return s.UnmarshalText([]byte(name))
}

// ReasoningUnset marks a model that reasons in whatever way its dialect
// defaults to, which is what a bare "reasoning": true means.
const ReasoningUnset ReasoningStyle = 255

// TokenizerKind is how exact input token counts are obtained.
type TokenizerKind uint8

const (
	TokenizerNone TokenizerKind = iota
	TokenizerRemoteCount
	TokenizerRemoteTokenize
	TokenizerLocalEstimate
)

var tokenizerNames = enumNames[TokenizerKind]{
	TokenizerNone:           "none",
	TokenizerRemoteCount:    "remote_count",
	TokenizerRemoteTokenize: "remote_tokenize",
	TokenizerLocalEstimate:  "local_estimate",
}

// String returns the configuration name of the kind.
func (k TokenizerKind) String() string { return tokenizerNames.name(k) }

// MarshalText encodes the kind as its configuration name.
func (k TokenizerKind) MarshalText() ([]byte, error) { return tokenizerNames.marshalText(k) }

// UnmarshalText decodes the configuration name.
func (k *TokenizerKind) UnmarshalText(data []byte) error {
	return tokenizerNames.unmarshalText(data, k, "tokenizer kind")
}
