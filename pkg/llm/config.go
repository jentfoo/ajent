package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"

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
	DialectAnthropic:         "anthropic-messages",
	DialectOpenAIResponses:   "openai-responses",
	DialectOpenAICompletions: "openai-completions",
}

// dialectAliases maps legacy spellings pi's catalogue once used onto their
// canonical name, consulted after the enum lookup fails.
var dialectAliases = map[string]Dialect{
	"anthropic": DialectAnthropic,
}

// String returns the configuration name of the dialect.
func (d Dialect) String() string { return dialectNames.name(d) }

// MarshalText encodes the dialect as its configuration name.
func (d Dialect) MarshalText() ([]byte, error) { return dialectNames.marshalText(d) }

// UnmarshalText decodes the configuration name.
func (d *Dialect) UnmarshalText(data []byte) error {
	if v, ok := dialectNames.lookup(string(data)); ok {
		*d = v
		return nil
	}
	if v, ok := dialectAliases[string(data)]; ok {
		*d = v
		return nil
	}
	return fmt.Errorf("llm: unknown api dialect %q, want one of %s", data, strings.Join(dialectNames.sorted(), ", "))
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

// Routing is openrouter's upstream provider preference. The four camelCase
// fields are ajent's historical spellings; the snake_case ones mirror pi's
// OpenRouterRouting so a drop-in file loads unchanged.
type Routing struct {
	Order          []string `json:"order,omitempty"`
	AllowFallbacks *bool    `json:"allowFallbacks,omitempty"`
	DataCollection string   `json:"dataCollection,omitempty"`
	RequireParams  *bool    `json:"requireParameters,omitempty"`

	Only                []string        `json:"only,omitempty"`
	Ignore              []string        `json:"ignore,omitempty"`
	Zdr                 *bool           `json:"zdr,omitempty"`
	EnforceDistillable  *bool           `json:"enforce_distillable_text,omitempty"`
	Quantizations       []string        `json:"quantizations,omitempty"`
	Sort                json.RawMessage `json:"sort,omitempty"`      // string or {by,partition}
	MaxPrice            json.RawMessage `json:"max_price,omitempty"` // per million token caps
	PreferredThroughput json.RawMessage `json:"preferred_min_throughput,omitempty"`
	PreferredLatency    json.RawMessage `json:"preferred_max_latency,omitempty"`
}

// ModelConfig is a model entry, or a partial override of a discovered one.
// Pointer fields distinguish absent from explicitly zero, which is what makes
// field by field layering work.
type ModelConfig struct {
	ID               string            `json:"id"`
	Name             string            `json:"name,omitempty"`
	Aliases          []string          `json:"aliases,omitempty"`
	Reasoning        *bool             `json:"reasoning,omitempty"`
	Input            []Modality        `json:"input,omitempty"`
	ContextWindow    *int              `json:"contextWindow,omitempty"`
	MaxTokens        *int              `json:"maxTokens,omitempty"`
	ContextReserve   *float64          `json:"contextReserve,omitempty"`   // fraction (<1) or absolute token count (>=1)
	CompactThreshold *float64          `json:"compactThreshold,omitempty"` // where auto-compaction fires; fraction (<1) of window or absolute count (>=1)
	Compat           *Compat           `json:"compat,omitempty"`
	LevelMap         map[Level]*string `json:"thinkingLevelMap,omitempty"`

	ThinkingBudgets map[Level]int `json:"thinkingBudgets,omitempty"` // per-level token budgets, override the ladder

	Headers        map[string]string `json:"headers,omitempty"`        // merged over the provider's per request
	SamplingParams map[string]any    `json:"samplingParams,omitempty"` // opaque additions to the request body
	Cost           json.RawMessage   `json:"cost,omitempty"`           // accepted for compatibility; pricing is out of scope
}

// Compat is the per model quirk set: a flat union of pi's four dialect schemas,
// plus ajent extensions. Every scalar field is a pointer so an override turns
// one quirk on without restating the rest.
//
// The dialect tag lists which dialects read a field; absent means every dialect
// honours it, which is where ajent's own extensions sit. compatWarnings reports
// any set field whose dialect does not include the model's resolved dialect.
type Compat struct {
	ThinkingFormat        *string  `json:"thinkingFormat,omitempty"`
	MaxTokensField        *string  `json:"maxTokensField,omitempty" dialect:"openai-completions"`
	ReasoningContentField *string  `json:"reasoningContentField,omitempty"`
	CacheControlFormat    *string  `json:"cacheControlFormat,omitempty"`
	Tokenizer             *string  `json:"tokenizer,omitempty"`
	ThinkTags             []string `json:"thinkTags,omitempty"` // open then close
	SessionAffinityFormat *string  `json:"sessionAffinityFormat,omitempty" dialect:"openai-completions,openai-responses"`
	DeferredTools         *string  `json:"deferredToolsMode,omitempty" dialect:"openai-completions"`

	SupportsDeveloperRole    *bool `json:"supportsDeveloperRole,omitempty"`
	SupportsSystemRole       *bool `json:"supportsSystemRole,omitempty"`
	SupportsReasoningEffort  *bool `json:"supportsReasoningEffort,omitempty" dialect:"openai-completions"`
	SupportsTemperature      *bool `json:"supportsTemperature,omitempty"`
	SupportsParallelTools    *bool `json:"supportsParallelToolCalls,omitempty"`
	SupportsStreamUsage      *bool `json:"supportsStreamUsage,omitempty" dialect:"openai-completions"`
	SupportsUsageInStreaming *bool `json:"supportsUsageInStreaming,omitempty" dialect:"openai-completions"` // pi name
	SupportsToolChoice       *bool `json:"supportsToolChoice,omitempty"`
	SupportsStore            *bool `json:"supportsStore,omitempty" dialect:"openai-completions"`
	SupportsPromptCache      *bool `json:"supportsPromptCache,omitempty"`
	SupportsImages           *bool `json:"supportsImages,omitempty"`
	SupportsLongCache        *bool `json:"supportsLongCacheRetention,omitempty"`

	RequiresReasoningReplay  *bool `json:"requiresReasoningReplay,omitempty"`
	RequiresReasoningContent *bool `json:"requiresReasoningContentOnAssistantMessages,omitempty" dialect:"openai-completions"`

	SupportsFinishReason             *bool `json:"supportsFinishReason,omitempty" dialect:"openai-completions"`
	RequiresToolResultName           *bool `json:"requiresToolResultName,omitempty" dialect:"openai-completions"`
	RequiresAssistantAfterToolResult *bool `json:"requiresAssistantAfterToolResult,omitempty" dialect:"openai-completions"`
	SupportsStrictMode               *bool `json:"supportsStrictMode,omitempty" dialect:"openai-completions,openai-responses"`
	RequiresThinkingAsText           *bool `json:"requiresThinkingAsText,omitempty" dialect:"openai-completions"`
	SupportsOpenAIGrammarTools       *bool `json:"supportsOpenAIGrammarTools,omitempty" dialect:"openai-completions,openai-responses"`
	SupportsThinkingTokenBudget      *bool `json:"supportsThinkingTokenBudget,omitempty" dialect:"openai-completions"`
	ZaiToolStream                    *bool `json:"zaiToolStream,omitempty" dialect:"openai-completions"`

	ForceAdaptiveThinking       *bool `json:"forceAdaptiveThinking,omitempty" dialect:"anthropic-messages"`
	AllowEmptySignature         *bool `json:"allowEmptySignature,omitempty" dialect:"anthropic-messages"`
	SupportsStrictTools         *bool `json:"supportsStrictTools,omitempty" dialect:"anthropic-messages"`
	SupportsToolReferences      *bool `json:"supportsToolReferences,omitempty" dialect:"anthropic-messages"`
	SupportsCacheControlOnTools *bool `json:"supportsCacheControlOnTools,omitempty" dialect:"anthropic-messages"`
	EagerToolInputStreaming     *bool `json:"supportsEagerToolInputStreaming,omitempty" dialect:"anthropic-messages"`

	SupportsAdditionalTools     *bool `json:"supportsAdditionalTools,omitempty" dialect:"openai-responses"`
	SupportsToolSearch          *bool `json:"supportsToolSearch,omitempty" dialect:"openai-responses"`
	SupportsExplicitPromptCache *bool `json:"supportsExplicitPromptCacheMode,omitempty" dialect:"openai-responses"`

	SendSessionAffinityHeaders *bool                      `json:"sendSessionAffinityHeaders,omitempty"`
	ChatTemplateKwargs         map[string]json.RawMessage `json:"chatTemplateKwargs,omitempty" dialect:"openai-completions"`
	ChatTemplateArgs           map[string]json.RawMessage `json:"chatTemplateArgs,omitempty" dialect:"openai-completions"`
	OpenRouterRouting          json.RawMessage            `json:"openRouterRouting,omitempty"` // pass through
	VercelGatewayRouting       json.RawMessage            `json:"vercelGatewayRouting,omitempty"`

	ExtraBody map[string]json.RawMessage `json:"extraBody,omitempty"`
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

// compatWarnings reports configuration that will not behave as authored for the
// dialect d: an unknown thinkingFormat value and any set field whose dialect tag
// excludes d. It returns nil when nothing is wrong with o.
func compatWarnings(o *Compat, d Dialect) []string {
	if o == nil {
		return nil
	}
	t := reflect.TypeOf(*o)
	v := reflect.ValueOf(*o)
	var out []string
	if o.ThinkingFormat != nil {
		if _, ok := parseThinkingFormat(*o.ThinkingFormat); !ok {
			out = append(out, fmt.Sprintf("compat thinkingFormat %q is not supported, the provider default is used", *o.ThinkingFormat))
		}
	}
	for i := range t.NumField() {
		f := t.Field(i)
		dialects, hasTag := f.Tag.Lookup("dialect")
		if !hasTag || !compatFieldSet(v, i) {
			continue
		}
		allowed := strings.Split(dialects, ",")
		name := compatJSONName(f)
		if d == DialectUnset || !slices.Contains(allowed, d.String()) {
			out = append(out, fmt.Sprintf("compat field %q is ignored by dialect %s", name, d))
		}
	}
	return out
}

// compatFieldSet reports whether the struct field at i was set in configuration.
func compatFieldSet(v reflect.Value, i int) bool {
	switch fv := v.Field(i); fv.Kind() {
	case reflect.Ptr:
		return !fv.IsNil()
	default:
		return !fv.IsZero()
	}
}

// compatJSONName returns a field's json name for warning messages.
func compatJSONName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if name == "" || name == "-" {
		return f.Name
	}
	return name
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
