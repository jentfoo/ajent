package llm

import (
	"encoding/json"
	"maps"
	"slices"
	"time"
)

const (
	fieldMaxTokens       = "max_tokens"
	fieldMaxCompletion   = "max_completion_tokens"
	fieldMaxOutputTokens = "max_output_tokens"
	fieldReasoning       = "reasoning"
	fieldReasoningConten = "reasoning_content"
	thinkOpenTag         = "<think>"
	thinkCloseTag        = "</think>"
)

// flavorDefault is everything built in about a known provider. It carries no
// models: those come from configuration and discovery only.
type flavorDefault struct {
	dialect   Dialect
	baseURL   string
	apiKeyEnv string
	timeouts  Timeouts
	discover  bool
	caps      Capabilities
}

// flavorDefaults is the entire compiled in catalogue. Adding a model here would
// be a staleness bug waiting to happen, so it holds endpoints and quirks only.
var flavorDefaults = map[Flavor]flavorDefault{
	FlavorAnthropic: {
		dialect:   DialectAnthropic,
		baseURL:   "https://api.anthropic.com",
		apiKeyEnv: "ANTHROPIC_API_KEY",
		caps: Capabilities{
			Reasoning:       ReasoningAnthropicBudget,
			ReasoningReplay: true,
			PromptCache:     true,
			CacheFormat:     "anthropic",
			Tokenizer:       TokenizerRemoteCount,
			MaxTokensField:  fieldMaxTokens,
			ParallelTools:   true,
			Images:          true,
			Temperature:     true,
			ToolChoice:      true,
		},
	},
	FlavorOpenAI: {
		dialect:   DialectOpenAIResponses,
		baseURL:   "https://api.openai.com/v1",
		apiKeyEnv: "OPENAI_API_KEY",
		caps: Capabilities{
			Reasoning:       ReasoningOpenAIEffort,
			ReasoningReplay: true,
			PromptCache:     true,
			Tokenizer:       TokenizerLocalEstimate,
			MaxTokensField:  fieldMaxOutputTokens,
			SystemAsRole:    true,
			DeveloperRole:   true,
			ParallelTools:   true,
			StreamUsage:     true,
			Images:          true,
			Temperature:     true,
			ToolChoice:      true,
		},
	},
	FlavorOpenRouter: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://openrouter.ai/api/v1",
		apiKeyEnv: "OPENROUTER_API_KEY",
		discover:  true,
		caps: Capabilities{
			Reasoning:      ReasoningOpenRouter,
			ReasoningField: fieldReasoning,
			PromptCache:    true,
			Tokenizer:      TokenizerLocalEstimate,
			MaxTokensField: fieldMaxTokens,
			SystemAsRole:   true,
			ParallelTools:  true,
			StreamUsage:    true,
			Images:         true,
			Temperature:    true,
			ToolChoice:     true,
		},
	},
	FlavorLMStudio: {
		dialect:  DialectOpenAICompletions,
		baseURL:  "http://localhost:1234/v1",
		discover: true,
		// a just in time model load holds the headers and then the stream for
		// minutes, so neither bound applies
		timeouts: Timeouts{Header: dur(0), Idle: dur(0), Connect: dur(5 * time.Second)},
		caps: Capabilities{
			Reasoning:      ReasoningInlineTags,
			ReasoningField: fieldReasoningConten,
			ThinkOpen:      thinkOpenTag,
			ThinkClose:     thinkCloseTag,
			Tokenizer:      TokenizerLocalEstimate,
			MaxTokensField: fieldMaxTokens,
			SystemAsRole:   true,
			StreamUsage:    true,
			Temperature:    true,
			ToolChoice:     true,
		},
	},
	FlavorLlamaCpp: {
		dialect:  DialectOpenAICompletions,
		baseURL:  "http://localhost:8080",
		discover: true,
		timeouts: Timeouts{Idle: dur(0), Connect: dur(5 * time.Second)},
		caps: Capabilities{
			Reasoning:      ReasoningInlineTags,
			ReasoningField: fieldReasoningConten,
			ThinkOpen:      thinkOpenTag,
			ThinkClose:     thinkCloseTag,
			Tokenizer:      TokenizerRemoteTokenize,
			MaxTokensField: fieldMaxTokens,
			SystemAsRole:   true,
			// older builds reject the unknown stream_options key, discovery
			// turns it back on when the build reports support
			StreamUsage: false,
			Temperature: true,
			ToolChoice:  true,
		},
	},
	FlavorGeneric: {
		dialect: DialectOpenAICompletions,
		caps: Capabilities{
			Tokenizer:      TokenizerLocalEstimate,
			MaxTokensField: fieldMaxTokens,
			SystemAsRole:   true,
			Temperature:    true,
			ToolChoice:     true,
		},
	},
}

// flavorFor returns the flavor for a provider entry, defaulting to the one its
// configuration key names so a provider called "lmstudio" needs no flavor field.
func flavorFor(name string, cfg ProviderConfig) Flavor {
	if cfg.Flavor != FlavorUnset {
		return cfg.Flavor
	} else if f, ok := flavorNames.lookup(name); ok && f != FlavorUnset {
		return f
	}
	return FlavorGeneric
}

// resolveCaps layers the provider then model compat blocks over the flavor
// defaults, field by field.
func resolveCaps(base Capabilities, layers ...*Compat) Capabilities {
	for _, l := range layers {
		base = applyCompat(base, l)
	}
	return base
}

// applyCompat overlays one compat block, leaving unset fields alone.
func applyCompat(c Capabilities, o *Compat) Capabilities {
	if o == nil {
		return c
	}
	if o.ThinkingFormat != nil {
		if style, ok := thinkingFormats[*o.ThinkingFormat]; ok {
			c.Reasoning = style
		}
	}
	if o.Tokenizer != nil {
		if k, ok := tokenizerNames.lookup(*o.Tokenizer); ok {
			c.Tokenizer = k
		}
	}
	if len(o.ThinkTags) == 2 {
		c.ThinkOpen, c.ThinkClose = o.ThinkTags[0], o.ThinkTags[1]
	}
	c.MaxTokensField = orStr(c.MaxTokensField, o.MaxTokensField)
	c.ReasoningField = orStr(c.ReasoningField, o.ReasoningContentField)
	c.CacheFormat = orStr(c.CacheFormat, o.CacheControlFormat)

	c.DeveloperRole = orBool(c.DeveloperRole, o.SupportsDeveloperRole)
	c.SystemAsRole = orBool(c.SystemAsRole, o.SupportsSystemRole)
	c.Temperature = orBool(c.Temperature, o.SupportsTemperature)
	c.ParallelTools = orBool(c.ParallelTools, o.SupportsParallelTools)
	c.StreamUsage = orBool(c.StreamUsage, o.SupportsStreamUsage)
	c.ToolChoice = orBool(c.ToolChoice, o.SupportsToolChoice)
	c.Store = orBool(c.Store, o.SupportsStore)
	c.PromptCache = orBool(c.PromptCache, o.SupportsPromptCache)
	c.LongCache = orBool(c.LongCache, o.SupportsLongCache)
	c.Images = orBool(c.Images, o.SupportsImages)
	c.ReasoningReplay = orBool(c.ReasoningReplay, o.RequiresReasoningReplay)
	c.ReplayReasoning = orBool(c.ReplayReasoning, o.RequiresReasoningContent)

	// supportsReasoningEffort signals the model takes an effort
	// parameter rather than a token budget
	if o.SupportsReasoningEffort != nil && *o.SupportsReasoningEffort {
		c.Reasoning = ReasoningOpenAIEffort
	}
	if len(o.ExtraBody) > 0 {
		c.ExtraBody = maps.Clone(o.ExtraBody)
	}
	return c
}

func orBool(dst bool, p *bool) bool {
	if p != nil {
		return *p
	}
	return dst
}

// mergeExtra overlays src keys onto base verbatim body additions.
func mergeExtra(base, src map[string]json.RawMessage) map[string]json.RawMessage {
	out := maps.Clone(base)
	if out == nil {
		out = make(map[string]json.RawMessage, len(src))
	}
	for k, v := range src {
		out[k] = v
	}
	return out
}

// rawSamplingParams re-encodes opaque sampling params as verbatim body keys.
func rawSamplingParams(params map[string]any) map[string]json.RawMessage {
	if len(params) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage, len(params))
	for k, v := range params {
		b, err := json.Marshal(v)
		if err != nil {
			continue // a value that cannot marshal is dropped rather than failing the model
		}
		out[k] = b
	}
	return out
}

func orStr(dst string, p *string) string {
	if p != nil {
		return *p
	}
	return dst
}

// resolveModel builds a Model from a config entry against a provider's defaults.
func resolveModel(provider string, base Capabilities, providerCompat *Compat, mc ModelConfig) Model {
	caps := resolveCaps(base, providerCompat, mc.Compat)
	if mc.Reasoning != nil && *mc.Reasoning != ReasoningUnset {
		caps.Reasoning = *mc.Reasoning
	}
	if len(mc.LevelMap) > 0 {
		caps.LevelMap = maps.Clone(mc.LevelMap)
	}
	// sampling params ride the same verbatim body escape hatch as extraBody, so
	// every adapter that folds one in picks up both without a new code path.
	if len(mc.SamplingParams) > 0 {
		caps.ExtraBody = mergeExtra(caps.ExtraBody, rawSamplingParams(mc.SamplingParams))
	}
	if caps.Reasoning == ReasoningNone {
		caps.ReasoningReplay = false
	}

	m := Model{
		Provider: provider,
		ID:       mc.ID,
		Name:     mc.Name,
		Aliases:  slices.Clone(mc.Aliases),
		Input:    slices.Clone(mc.Input),
		Caps:     caps,
		Headers:  maps.Clone(mc.Headers),
	}
	if mc.ContextWindow != nil {
		m.ContextWindow = *mc.ContextWindow
	}
	if mc.MaxTokens != nil {
		m.MaxOutput = *mc.MaxTokens
	}
	if mc.ContextReserve != nil {
		m.ContextReserve = *mc.ContextReserve
	}
	if mc.CompactThreshold != nil {
		m.CompactThreshold = *mc.CompactThreshold
	}
	if len(m.Input) == 0 {
		m.Input = []Modality{ModalityText}
		if caps.Images {
			m.Input = append(m.Input, ModalityImage)
		}
	}
	return m
}
