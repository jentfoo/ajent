package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/jentfoo/ajent/pkg/config"
)

// ModelsFileName is the configuration file listing providers and their models.
const ModelsFileName = "models.json"

// Dialect is the wire protocol a provider speaks.
type Dialect uint8

const (
	DialectUnset Dialect = iota
	DialectAnthropic
	DialectOpenAIResponses
	DialectOpenAICompletions
)

var dialectNames = enumNames[Dialect]{
	DialectUnset:             "",
	DialectAnthropic:         "anthropic",
	DialectOpenAIResponses:   "openai-responses",
	DialectOpenAICompletions: "openai-completions",
}

// String returns the configuration name of the dialect.
func (d Dialect) String() string { return dialectNames.name(d) }

// MarshalText encodes the dialect as its configuration name.
func (d Dialect) MarshalText() ([]byte, error) { return dialectNames.marshalText(d) }

// UnmarshalText decodes the configuration name.
func (d *Dialect) UnmarshalText(data []byte) error {
	return dialectNames.unmarshalText(data, d, "api dialect")
}

// Flavor selects discovery and quirk defaults. It is separate from Dialect so
// an OpenAI compatible proxy in front of a known server still gets the right
// defaults.
type Flavor uint8

const (
	FlavorUnset Flavor = iota
	FlavorAnthropic
	FlavorOpenAI
	FlavorOpenRouter
	FlavorLMStudio
	FlavorLlamaCpp
	FlavorGeneric
)

var flavorNames = enumNames[Flavor]{
	FlavorUnset:      "",
	FlavorAnthropic:  "anthropic",
	FlavorOpenAI:     "openai",
	FlavorOpenRouter: "openrouter",
	FlavorLMStudio:   "lmstudio",
	FlavorLlamaCpp:   "llamacpp",
	FlavorGeneric:    "generic",
}

// String returns the configuration name of the flavor.
func (f Flavor) String() string { return flavorNames.name(f) }

// MarshalText encodes the flavor as its configuration name.
func (f Flavor) MarshalText() ([]byte, error) { return flavorNames.marshalText(f) }

// UnmarshalText decodes the configuration name.
func (f *Flavor) UnmarshalText(data []byte) error {
	return flavorNames.unmarshalText(data, f, "provider flavor")
}

// File is the decoded models.json.
type File struct {
	Version      int                       `json:"version,omitempty"`
	DefaultModel string                    `json:"defaultModel,omitempty"`
	Providers    map[string]ProviderConfig `json:"providers"`
}

// ProviderConfig is one endpoint. Unset fields fall back to the flavor default.
type ProviderConfig struct {
	API        Dialect           `json:"api,omitempty"`
	Flavor     Flavor            `json:"flavor,omitempty"`
	Name       string            `json:"name,omitempty"` // accepted for compatibility; not used
	BaseURL    string            `json:"baseUrl,omitempty"`
	APIKey     string            `json:"apiKey,omitempty"`
	APIKeyEnv  string            `json:"apiKeyEnv,omitempty"`
	OAuth      string            `json:"oauth,omitempty"`      // accepted for compatibility; not used
	AuthHeader *bool             `json:"authHeader,omitempty"` // accepted for compatibility; not used
	Headers    map[string]string `json:"headers,omitempty"`
	Timeouts   Timeouts          `json:"timeouts,omitzero"`
	Retry      RetryPolicy       `json:"retry,omitzero"`
	Discover   *bool             `json:"discover,omitempty"`
	Routing    *Routing          `json:"routing,omitempty"` // openrouter only
	Compat     *Compat           `json:"compat,omitempty"`  // defaults for every model here
	Models     []ModelConfig     `json:"models,omitempty"`
	Disabled   bool              `json:"disabled,omitempty"`
}

// Routing is openrouter's upstream provider preference.
type Routing struct {
	Order          []string `json:"order,omitempty"`
	AllowFallbacks *bool    `json:"allowFallbacks,omitempty"`
	DataCollection string   `json:"dataCollection,omitempty"`
	RequireParams  *bool    `json:"requireParameters,omitempty"`
}

// ModelConfig is a model entry, or a partial override of a discovered one.
// Pointer fields distinguish absent from explicitly zero, which is what makes
// field by field layering work.
type ModelConfig struct {
	ID             string            `json:"id"`
	Name           string            `json:"name,omitempty"`
	Aliases        []string          `json:"aliases,omitempty"`
	Reasoning      *ReasoningStyle   `json:"reasoning,omitempty"`
	Input          []Modality        `json:"input,omitempty"`
	ContextWindow  *int              `json:"contextWindow,omitempty"`
	MaxTokens      *int              `json:"maxTokens,omitempty"`
	ContextReserve *float64          `json:"contextReserve,omitempty"` // fraction (<1) or absolute token count (>=1)
	Compat         *Compat           `json:"compat,omitempty"`
	LevelMap       map[Level]*string `json:"thinkingLevelMap,omitempty"`

	Headers        map[string]string `json:"headers,omitempty"`        // merged over the provider's per request
	SamplingParams map[string]any    `json:"samplingParams,omitempty"` // opaque additions to the request body
	Cost           json.RawMessage   `json:"cost,omitempty"`           // accepted for compatibility; pricing is out of scope
}

// Compat is the per model quirk set. Every field is a pointer so an override
// turns one quirk on without restating the rest.
type Compat struct {
	ThinkingFormat        *string  `json:"thinkingFormat,omitempty"`
	MaxTokensField        *string  `json:"maxTokensField,omitempty"`
	ReasoningContentField *string  `json:"reasoningContentField,omitempty"`
	CacheControlFormat    *string  `json:"cacheControlFormat,omitempty"`
	Tokenizer             *string  `json:"tokenizer,omitempty"`
	ThinkTags             []string `json:"thinkTags,omitempty"` // open then close

	SupportsDeveloperRole   *bool `json:"supportsDeveloperRole,omitempty"`
	SupportsSystemRole      *bool `json:"supportsSystemRole,omitempty"`
	SupportsReasoningEffort *bool `json:"supportsReasoningEffort,omitempty"`
	SupportsTemperature     *bool `json:"supportsTemperature,omitempty"`
	SupportsParallelTools   *bool `json:"supportsParallelToolCalls,omitempty"`
	SupportsStreamUsage     *bool `json:"supportsStreamUsage,omitempty"`
	SupportsToolChoice      *bool `json:"supportsToolChoice,omitempty"`
	SupportsStore           *bool `json:"supportsStore,omitempty"`
	SupportsPromptCache     *bool `json:"supportsPromptCache,omitempty"`
	SupportsImages          *bool `json:"supportsImages,omitempty"`
	SupportsLongCache       *bool `json:"supportsLongCacheRetention,omitempty"`

	RequiresReasoningReplay  *bool `json:"requiresReasoningReplay,omitempty"`
	RequiresReasoningContent *bool `json:"requiresReasoningContentOnAssistantMessages,omitempty"`

	ExtraBody map[string]json.RawMessage `json:"extraBody,omitempty"`
}

// thinkingFormats maps the standard thinkingFormat names onto a reasoning style.
var thinkingFormats = map[string]ReasoningStyle{
	"none":               ReasoningNone,
	"anthropic":          ReasoningAnthropicBudget,
	"reasoning_effort":   ReasoningOpenAIEffort,
	"openai-responses":   ReasoningOpenAIEffort,
	"openrouter":         ReasoningOpenRouter,
	"qwen-chat-template": ReasoningInlineTags,
	"together":           ReasoningInlineTags,
	"think-tags":         ReasoningInlineTags,
	"deepseek":           ReasoningContentField,
	"reasoning_content":  ReasoningContentField,
}

// LoadFile reads a models.json, returning the decoded file and warnings for
// anything questionable in it. A missing file is not an error.
//
// Line comments and trailing commas are accepted, so an existing configuration
// loads unchanged. A syntax error names the line and column it is on.
func LoadFile(path string) (File, []string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return File{}, nil, nil
	} else if err != nil {
		return File{}, nil, err
	}
	data := config.RelaxJSON(raw)

	var f File
	if err = json.Unmarshal(data, &f); err != nil {
		return File{}, nil, config.JSONError(path, raw, err)
	}

	unknown, err := config.UnknownKeys(data, f)
	if err != nil {
		return File{}, nil, config.JSONError(path, raw, err)
	}
	warnings := make([]string, 0, len(unknown))
	for _, k := range unknown {
		warnings = append(warnings, fmt.Sprintf("%s: unrecognized key %q", path, k))
	}
	// encoding/json keeps the last of a repeated key, so the earlier one looks
	// applied and is not
	for _, k := range config.DuplicateKeys(data) {
		warnings = append(warnings, fmt.Sprintf("%s: duplicate key %q, the last one wins", path, k))
	}
	if w := secretWarning(path, f); w != "" {
		warnings = append(warnings, w)
	}
	return f, warnings, nil
}

// LoadUserFile reads the models.json in the ajent configuration directory.
func LoadUserFile() (File, []string, error) {
	path, err := config.UserPath(ModelsFileName)
	if err != nil {
		return File{}, nil, err
	}
	return LoadFile(path)
}

// secretWarning reports a permissive mode on a file holding a literal key.
func secretWarning(path string, f File) string {
	for _, p := range f.Providers {
		if p.APIKey != "" {
			return config.CheckSecretPerms(path)
		}
	}
	return ""
}
