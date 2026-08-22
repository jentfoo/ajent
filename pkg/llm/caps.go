package llm

import "encoding/json"

// Capabilities is the fully resolved quirk set for one model: dialect defaults
// overlaid with detection, then provider and model compat blocks from
// configuration. It is what an adapter reads instead of special casing a vendor.
type Capabilities struct {
	Dialect    Dialect
	Reasoning  bool           // whether the model reasons at all (pi's reasoning)
	Thinking   ThinkingFormat // request encoding for chat-completions
	ThinkOpen  string         // inline reasoning tags, empty when unused
	ThinkClose string
	Budgets    map[Level]int // per-level token budgets from the compiled in ladder

	ReasoningReplay bool   // prior thinking blocks must be sent back
	ReasoningField  string // delta field carrying reasoning text, compat dialect
	ReplayReasoning bool   // echo reasoning_content on assistant messages

	// per-level override of the provider's effort value; nil omits it, absent uses the default
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

	// Gates resolved from compat, unset and explicitly false both landing here.
	SupportsReasoningEffort          bool
	SupportsFinishReason             bool
	SupportsStrict                   bool // openai-completions strict mode on tools
	SupportsStrictTools              bool // anthropic strict tool schemas
	SupportsGrammarTools             bool // openai custom Lark/regex grammar tools
	ForceAdaptiveThinking            bool // anthropic adaptive thinking regardless of id
	AllowEmptySignature              bool // replay empty signatures instead of text conversion
	RequiresThinkingAsText           bool
	RequiresToolResultName           bool
	RequiresAssistantAfterToolResult bool
	EagerToolInputStreaming          bool
	CacheControlOnTools              bool
	ToolReferences                   bool
	SupportsAdditionalTools          bool // responses message-anchored additional_tools
	SupportsToolSearch               bool // client-executed tool search for deferred tools
	SupportsExplicitPromptCache      bool // openai prompt_cache_options
	ZaiToolStream                    bool
	SessionAffinity                  bool // send session-affinity headers

	SessionAffinityFormat string                     // header format when SessionAffinity is set
	DeferredTools         string                     // deferred tool serialization mode
	ThinkingBudgetField   string                     // request key capping reasoning tokens; empty disables
	ChatTemplateKwargs    map[string]json.RawMessage // chat_template_kwargs additions
	ChatTemplateArgs      map[string]json.RawMessage // baseten chat_template_args additions
	OpenRouterRouting     json.RawMessage            // verbatim openrouter provider routing
}

// openRouterAffinityFormat is the compat session-affinity header format value.
const openRouterAffinityFormat = "openrouter"

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

// defaultBudgets fills a per-level ladder from the compiled in rungs for one
// model's output limit. Later parts consume it when no explicit budget is set.
func defaultBudgets(maxOutput int) map[Level]int {
	out := make(map[Level]int, 7)
	for _, l := range []Level{LevelMinimal, LevelLow, LevelMedium, LevelHigh, LevelXHigh, LevelMax} {
		if b := levelBudget(l, maxOutput); b > 0 {
			out[l] = b
		}
	}
	return out
}
