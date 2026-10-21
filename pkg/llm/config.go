package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/go-analyze/bulk"
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
	// DialectUnknown is a configured api this build cannot speak. It disables its
	// provider rather than falling back, since a wrong dialect is a wrong protocol.
	DialectUnknown
)

var dialectNames = enumNames[Dialect]{
	DialectUnset:             "",
	DialectAnthropic:         "anthropic-messages",
	DialectOpenAIResponses:   "openai-responses",
	DialectOpenAICompletions: "openai-completions",
	DialectUnknown:           nameUnknown,
}

// dialectChoices are the api values a provider entry may name.
func dialectChoices() []string {
	return []string{
		DialectAnthropic.String(), DialectOpenAIResponses.String(), DialectOpenAICompletions.String(),
	}
}

// dialectAliases maps legacy spellings once used by a model catalogue onto their
// canonical name, consulted after the enum lookup fails.
var dialectAliases = map[string]Dialect{
	providerAnthropic: DialectAnthropic,
}

// String returns the configuration name of the dialect.
func (d Dialect) String() string { return dialectNames.name(d) }

// MarshalText encodes the dialect as its configuration name.
func (d Dialect) MarshalText() ([]byte, error) { return dialectNames.marshalText(d) }

// UnmarshalText decodes the configuration name. An unrecognized value decodes to
// DialectUnknown rather than failing, so one unsupported provider does not cost
// the user every other provider in the file.
func (d *Dialect) UnmarshalText(data []byte) error {
	if v, ok := dialectNames.lookup(string(data)); ok && v != DialectUnknown {
		*d = v
	} else if v, ok := dialectAliases[string(data)]; ok {
		*d = v
	} else {
		*d = DialectUnknown
	}
	return nil
}

// Flavor selects discovery and quirk defaults. It is separate from Dialect so an
// OpenAI compatible proxy in front of a known server still gets the right defaults.
type Flavor uint8

const (
	FlavorUnset Flavor = iota
	FlavorAnthropic
	FlavorOpenAI
	FlavorZAI
	FlavorZaiCodingCN
	FlavorOpenRouter
	FlavorDeepSeek
	FlavorTogether
	FlavorXAI
	FlavorGroq
	FlavorMistral
	FlavorMoonshotAI
	FlavorMoonshotCN
	FlavorKimi
	FlavorGoogle
	FlavorCerebras
	FlavorNVIDIA
	FlavorHuggingFace
	FlavorMiniMax
	FlavorMiniMaxCN
	FlavorBaseten
	FlavorAntLing
	FlavorFireworks
	FlavorQwenPlan
	FlavorQwenPlanCN
	FlavorXiaomi
	FlavorXiaomiCN
	FlavorXiaomiSGP
	FlavorXiaomiAMS
	FlavorLMStudio
	FlavorLlamaCpp
	FlavorGeneric
	// FlavorUnknown is a configured flavor this build does not know. It degrades to
	// FlavorGeneric, since a flavor only picks defaults and never the protocol.
	FlavorUnknown
)

var flavorNames = enumNames[Flavor]{
	FlavorUnset:       "",
	FlavorAnthropic:   providerAnthropic,
	FlavorOpenAI:      providerOpenAI,
	FlavorZAI:         "zai",
	FlavorZaiCodingCN: "zai-coding-cn",
	FlavorOpenRouter:  openRouterAffinityFormat,
	FlavorDeepSeek:    "deepseek",
	FlavorTogether:    "together",
	FlavorXAI:         "xai",
	FlavorGroq:        "groq",
	FlavorMistral:     "mistral",
	FlavorMoonshotAI:  "moonshotai",
	FlavorMoonshotCN:  "moonshotai-cn",
	FlavorKimi:        "kimi",
	FlavorGoogle:      "google",
	FlavorCerebras:    "cerebras",
	FlavorNVIDIA:      "nvidia",
	FlavorHuggingFace: "huggingface",
	FlavorMiniMax:     "minimax",
	FlavorMiniMaxCN:   "minimax-cn",
	FlavorBaseten:     "baseten",
	FlavorAntLing:     "ant-ling",
	FlavorFireworks:   "fireworks",
	FlavorQwenPlan:    "qwen-token-plan",
	FlavorQwenPlanCN:  "qwen-token-plan-cn",
	FlavorXiaomi:      "xiaomi",
	FlavorXiaomiCN:    "xiaomi-token-plan-cn",
	FlavorXiaomiSGP:   "xiaomi-token-plan-sgp",
	FlavorXiaomiAMS:   "xiaomi-token-plan-ams",
	FlavorLMStudio:    "lmstudio",
	FlavorLlamaCpp:    "llamacpp",
	FlavorGeneric:     "generic",
	FlavorUnknown:     nameUnknown,
}

// String returns the configuration name of the flavor.
func (f Flavor) String() string { return flavorNames.name(f) }

// MarshalText encodes the flavor as its configuration name.
func (f Flavor) MarshalText() ([]byte, error) { return flavorNames.marshalText(f) }

// UnmarshalText decodes the configuration name. An unrecognized value decodes to
// FlavorUnknown rather than failing the whole file.
func (f *Flavor) UnmarshalText(data []byte) error {
	if v, ok := flavorNames.lookup(string(data)); ok && v != FlavorUnknown {
		*f = v
	} else {
		*f = FlavorUnknown
	}
	return nil
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
	Name       string            `json:"name,omitempty"` // accepted for compatibility, not used
	BaseURL    string            `json:"baseUrl,omitempty"`
	APIKey     string            `json:"apiKey,omitempty"`
	APIKeyEnv  string            `json:"apiKeyEnv,omitempty"`
	OAuth      string            `json:"oauth,omitempty"`      // accepted for compatibility, not used
	AuthHeader *bool             `json:"authHeader,omitempty"` // accepted for compatibility, not used
	Headers    map[string]string `json:"headers,omitempty"`
	Timeouts   Timeouts          `json:"timeouts,omitzero"`
	Retry      RetryPolicy       `json:"retry,omitzero"`
	Discover   *bool             `json:"discover,omitempty"`
	Routing    *Routing          `json:"routing,omitempty"` // openrouter only
	Compat     *Compat           `json:"compat,omitempty"`  // defaults for every model here
	Models     []ModelConfig     `json:"models,omitempty"`
	// ModelOverrides adjusts discovered models by id without restating them. An id
	// declared in Models is not a valid target: edit the declaration instead.
	ModelOverrides map[string]ModelOverride `json:"modelOverrides,omitempty"`
	Disabled       bool                     `json:"disabled,omitempty"`
}

// Routing is openrouter's upstream provider preference. The four camelCase
// fields are ajent's historical spellings, and the snake_case ones mirror
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
	API              Dialect           `json:"api,omitempty"`     // overrides the provider's
	BaseURL          string            `json:"baseUrl,omitempty"` // overrides the provider's
	Aliases          []string          `json:"aliases,omitempty"`
	Reasoning        *bool             `json:"reasoning,omitempty"`
	Input            []Modality        `json:"input,omitempty"`
	ContextWindow    *int              `json:"contextWindow,omitempty"`
	MaxTokens        *int              `json:"maxTokens,omitempty"`
	ContextReserve   *float64          `json:"contextReserve,omitempty"`   // fraction (<1) or absolute token count (>=1)
	CompactThreshold *float64          `json:"compactThreshold,omitempty"` // where auto-compaction fires: fraction (<1) of window or absolute count (>=1)
	Compat           *Compat           `json:"compat,omitempty"`
	LevelMap         map[Level]*string `json:"thinkingLevelMap,omitempty"`

	ThinkingBudgets map[Level]int `json:"thinkingBudgets,omitempty"` // per-level token budgets, override the ladder

	Headers        map[string]string `json:"headers,omitempty"`        // merged over the provider's per request
	SamplingParams map[string]any    `json:"samplingParams,omitempty"` // opaque additions to the request body
}

// ModelOverride is a partial model applied over a discovered entry. It carries
// the documented override field set, which is narrower than ModelConfig: an
// override cannot introduce a model, rename its id or change its endpoint.
type ModelOverride struct {
	Name          string            `json:"name,omitempty"`
	Reasoning     *bool             `json:"reasoning,omitempty"`
	Input         []Modality        `json:"input,omitempty"`
	ContextWindow *int              `json:"contextWindow,omitempty"`
	MaxTokens     *int              `json:"maxTokens,omitempty"`
	Compat        *Compat           `json:"compat,omitempty"`
	LevelMap      map[Level]*string `json:"thinkingLevelMap,omitempty"`

	Headers        map[string]string `json:"headers,omitempty"`        // merged per key
	SamplingParams map[string]any    `json:"samplingParams,omitempty"` // merged per key
}

// Compat is the per model quirk set: a flat union of the four dialect schemas,
// plus ajent extensions. Every scalar field is a pointer so an override turns
// one quirk on without restating the rest.
//
// The dialect tag lists which dialects read a field. Absent means every dialect
// honours it, which is where ajent's own extensions sit. compatWarnings reports
// any set field whose dialect does not include the model's resolved dialect.
type Compat struct {
	ThinkingFormat        *string `json:"thinkingFormat,omitempty"`
	MaxTokensField        *string `json:"maxTokensField,omitempty" dialect:"openai-completions"`
	ReasoningContentField *string `json:"reasoningContentField,omitempty"`
	// RetainDefault is the policy a model falls back to when no layer sets one.
	// "none" parses to the enum zero and reads as unset, a silent no-op here
	RetainDefault         *RetainPolicy `json:"retainDefault,omitempty"`
	CacheControlFormat    *string       `json:"cacheControlFormat,omitempty"`
	Tokenizer             *string       `json:"tokenizer,omitempty"`
	ThinkTags             []string      `json:"thinkTags,omitempty"` // open then close
	SessionAffinityFormat *string       `json:"sessionAffinityFormat,omitempty" dialect:"openai-completions,openai-responses"`
	DeferredTools         *string       `json:"deferredToolsMode,omitempty" dialect:"openai-completions"`

	SupportsDeveloperRole    *bool `json:"supportsDeveloperRole,omitempty"`
	SupportsSystemRole       *bool `json:"supportsSystemRole,omitempty"`
	SupportsReasoningEffort  *bool `json:"supportsReasoningEffort,omitempty" dialect:"openai-completions"`
	SupportsTemperature      *bool `json:"supportsTemperature,omitempty"`
	SupportsParallelTools    *bool `json:"supportsParallelToolCalls,omitempty"`
	SupportsStreamUsage      *bool `json:"supportsStreamUsage,omitempty" dialect:"openai-completions"`
	SupportsUsageInStreaming *bool `json:"supportsUsageInStreaming,omitempty" dialect:"openai-completions"` // canonical field name
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
	// ThinkingTokenBudgetField is the canonical spelling. SupportsThinkingTokenBudget
	// is the boolean alias for "thinking_token_budget".
	ThinkingTokenBudgetField *string `json:"thinkingTokenBudgetField,omitempty" dialect:"openai-completions"`
	ZaiToolStream            *bool   `json:"zaiToolStream,omitempty" dialect:"openai-completions"`

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
// anything questionable in it. A missing, empty or comment-only file returns
// the zero value without error.
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
	data, err := config.RelaxedObject(path, raw)
	if err != nil {
		return File{}, nil, err
	} else if data == nil {
		return File{}, nil, nil // blank or comment-only content behaves as an absent file
	}

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
	// encoding/json keeps the last of a repeated key, so the earlier one looks applied and is not
	for _, k := range config.DuplicateKeys(data) {
		warnings = append(warnings, fmt.Sprintf("%s: duplicate key %q, the last one wins", path, k))
	}
	// a permissive mode on a file holding a literal key is worth a warning
	if slices.ContainsFunc(bulk.MapValuesSlice(f.Providers), func(p ProviderConfig) bool { return p.APIKey != "" }) {
		if w := config.CheckSecretPerms(path); w != "" {
			warnings = append(warnings, w)
		}
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

// SaveUserFile writes the models file to the ajent configuration directory,
// atomically and with secret permissions since it may hold a literal key. A
// bounded cross-process lock covers the read-modify-write, like config.json's
// user layer.
//
// f overlays the existing file: fields the File schema declares follow f, and
// unrecognized keys anywhere in the current contents survive the rewrite. The
// returned warnings report content the save drops, such as // comments.
func SaveUserFile(f File) ([]string, error) {
	path, err := config.UserPath(ModelsFileName)
	if err != nil {
		return nil, err
	}
	over, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}

	release := config.LockSave()
	defer release()

	var warns []string
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		raw = nil // a brand new file starts empty
	case err != nil:
		return nil, err
	default:
		// a trailing comma is fixed silently; only comments are content lost
		relaxed, hasComments := config.RelaxJSONReport(raw)
		if hasComments {
			warns = append(warns, path+" has comments; saving will remove them")
		}
		raw = relaxed
	}

	out, err := config.OverlayJSON(raw, over, File{})
	if err != nil {
		return nil, err
	}
	return warns, config.WriteFileAtomic(path, out, config.SecretPerm)
}

// compatWarnings reports configuration that will not behave as authored for the
// dialect d: an unknown thinkingFormat value, alias spellings set twice in one
// block, and any set field whose dialect tag excludes d. It returns nil when
// nothing is wrong with o.
func compatWarnings(o *Compat, d Dialect) []string {
	if o == nil {
		return nil
	}
	t := reflect.TypeOf(*o)
	v := reflect.ValueOf(*o)
	out := compatAliasWarnings(o)
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
		jsonName, _, _ := strings.Cut(f.Tag.Get("json"), ",") // a field's json name for the warning
		if jsonName == "" || jsonName == "-" {
			jsonName = f.Name
		}
		if d == DialectUnset || !slices.Contains(allowed, d.String()) {
			out = append(out, fmt.Sprintf("compat field %q is ignored by dialect %s", jsonName, d))
		}
	}
	return out
}

// mergeCompat layers every set field of over onto base into a new block. A nil
// argument on either side returns the other unchanged. Alias pairs are promoted
// then collapsed, so the merged block carries one spelling per quirk and
// applyCompat's within-block precedence never re-decides which layer won.
func mergeCompat(base, over *Compat) *Compat {
	if over == nil {
		return base
	} else if base == nil {
		return over
	}
	local := *over
	promoteCompatAliases(&local) // a copy, the caller's block stays untouched
	out := *base
	dst := reflect.ValueOf(&out).Elem()
	src := reflect.ValueOf(local)
	for i := range src.NumField() {
		if compatFieldSet(src, i) {
			dst.Field(i).Set(src.Field(i))
		}
	}
	collapseCompatAliases(&out)
	return &out
}

// promoteCompatAliases moves an alias without its canonical partner onto the
// canonical field, so a later layer's write lands whichever spelling it used.
func promoteCompatAliases(c *Compat) {
	if c.SupportsStreamUsage != nil && c.SupportsUsageInStreaming == nil {
		c.SupportsUsageInStreaming = c.SupportsStreamUsage
	}
	if c.SupportsThinkingTokenBudget != nil && c.ThinkingTokenBudgetField == nil {
		// the boolean alias maps to the vLLM field name, false clears it
		field := ""
		if *c.SupportsThinkingTokenBudget {
			field = fieldThinkingTokenBudget
		}
		c.ThinkingTokenBudgetField = &field
	}
}

// collapseCompatAliases promotes any remaining alias value, then drops the alias
// fields, leaving one canonical spelling per pair.
func collapseCompatAliases(c *Compat) {
	promoteCompatAliases(c)
	c.SupportsStreamUsage = nil
	c.SupportsThinkingTokenBudget = nil
}

// compatAliasWarnings names a block setting both spellings of one aliased
// quirk, which the canonical spelling wins. mergeCompat collapses the pair, so
// blocks it merges are reported by collapsedAliasWarnings instead.
func compatAliasWarnings(o *Compat) []string {
	if o == nil {
		return nil
	}
	var out []string
	if o.SupportsStreamUsage != nil && o.SupportsUsageInStreaming != nil {
		out = append(out, "compat sets both supportsUsageInStreaming and supportsStreamUsage; supportsUsageInStreaming wins")
	}
	if o.SupportsThinkingTokenBudget != nil && o.ThinkingTokenBudgetField != nil {
		out = append(out, "compat sets both thinkingTokenBudgetField and supportsThinkingTokenBudget; thinkingTokenBudgetField wins")
	}
	return out
}

// compatFieldSet reports whether the struct field at i was set in configuration.
func compatFieldSet(v reflect.Value, i int) bool {
	switch fv := v.Field(i); fv.Kind() {
	case reflect.Pointer:
		return !fv.IsNil()
	default:
		return !fv.IsZero()
	}
}
