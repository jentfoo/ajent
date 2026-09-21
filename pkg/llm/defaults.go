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
	// vLLM's spelling; selected by the supportsThinkingTokenBudget alias.
	fieldThinkingTokenBudget = "thinking_token_budget"
	thinkOpenTag             = "<think>"
	thinkCloseTag            = "</think>"
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

// chatCaps is the generic chat-completions baseline shared by the hosted
// OpenAI-compatible flavors; vendor detection layers quirks over it.
func chatCaps() Capabilities {
	return Capabilities{
		Dialect:                 DialectOpenAICompletions,
		Reasoning:               true, // hosted openai-completions families reason by default
		Tokenizer:               TokenizerLocalEstimate,
		MaxTokensField:          fieldMaxTokens,
		SystemAsRole:            true,
		Temperature:             true,
		ToolChoice:              true,
		SupportsReasoningEffort: true,
		SupportsFinishReason:    true,
		SupportsStrict:          true,
	}
}

// zaiCaps is the GLM baseline: reasoning always on, reasoning_content replay
// and the zai thinking format, plus tool-call streaming.
func zaiCaps() Capabilities {
	caps := chatCaps()
	caps.Reasoning = true
	caps.Thinking = ThinkingZAI
	caps.ReasoningField = fieldReasoningConten
	caps.ParallelTools = true
	caps.StreamUsage = true
	caps.ZaiToolStream = true
	return caps
}

// anthropicCaps is the Messages-API baseline for anthropic-compatible hosted
// endpoints; they speak the wire shape but not anthropic's cache or tokenizer.
func anthropicCaps() Capabilities {
	return Capabilities{
		Dialect:         DialectAnthropic,
		Reasoning:       true,
		Thinking:        ThinkingAnthropic,
		ReasoningReplay: true,
		Tokenizer:       TokenizerLocalEstimate,
		MaxTokensField:  fieldMaxTokens,
		ParallelTools:   true,
		Temperature:     true,
		ToolChoice:      true,
	}
}

// flavorDefaults is the entire compiled in catalogue. Adding a model here would
// be a staleness bug waiting to happen, so it holds endpoints and quirks only.
var flavorDefaults = map[Flavor]flavorDefault{
	FlavorAnthropic: {
		dialect:   DialectAnthropic,
		baseURL:   "https://api.anthropic.com",
		apiKeyEnv: "ANTHROPIC_API_KEY",
		discover:  true,
		caps: Capabilities{
			Dialect:         DialectAnthropic,
			Reasoning:       true,
			Thinking:        ThinkingAnthropic,
			ReasoningReplay: true,
			PromptCache:     true,
			CacheFormat:     providerAnthropic,
			Tokenizer:       TokenizerRemoteCount,
			MaxTokensField:  fieldMaxTokens,
			ParallelTools:   true,
			Images:          true,
			Temperature:     true,
			ToolChoice:      true,
			// both default on so a bare anthropic entry gets them
			EagerToolInputStreaming: true,
			CacheControlOnTools:     true,
		},
	},
	FlavorOpenAI: {
		dialect:   DialectOpenAIResponses,
		baseURL:   "https://api.openai.com/v1",
		apiKeyEnv: "OPENAI_API_KEY",
		discover:  true,
		caps: Capabilities{
			Dialect:         DialectOpenAIResponses,
			Reasoning:       true,
			Thinking:        ThinkingOpenAI,
			ReasoningReplay: true,
			PromptCache:     true,
			// api.openai.com accepts the routing key; generic openai-compatible
			// endpoints opt in via supportsExplicitPromptCacheMode
			SupportsExplicitPromptCache: true,
			Tokenizer:                   TokenizerLocalEstimate,
			MaxTokensField:              fieldMaxOutputTokens,
			SystemAsRole:                true,
			DeveloperRole:               true,
			ParallelTools:               true,
			StreamUsage:                 true,
			Images:                      true,
			Temperature:                 true,
			ToolChoice:                  true,
		},
	},
	// z.ai serves GLM over chat-completions; the coding-plan endpoints share the
	// wire format of the general one, so only the base URL differs.
	FlavorZAI: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://api.z.ai/api/coding/paas/v4",
		apiKeyEnv: "ZAI_API_KEY",
		discover:  true,
		caps:      zaiCaps(),
	},
	FlavorZaiCodingCN: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://open.bigmodel.cn/api/coding/paas/v4",
		apiKeyEnv: "ZAI_CODING_CN_API_KEY",
		discover:  true,
		caps:      zaiCaps(),
	},
	FlavorOpenRouter: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://openrouter.ai/api/v1",
		apiKeyEnv: "OPENROUTER_API_KEY",
		discover:  true,
		caps: Capabilities{
			Dialect:        DialectOpenAICompletions,
			Reasoning:      true,
			Thinking:       ThinkingOpenRouter,
			ReasoningField: fieldReasoning,
			// the openrouter default emits reasoning_effort for generic levels
			SupportsReasoningEffort: true,
			SupportsFinishReason:    true,
			SupportsStrict:          true,
			PromptCache:             true,
			Tokenizer:               TokenizerLocalEstimate,
			MaxTokensField:          fieldMaxTokens,
			SystemAsRole:            true,
			ParallelTools:           true,
			StreamUsage:             true,
			Images:                  true,
			Temperature:             true,
			ToolChoice:              true,
		},
	},
	// Hosted chat-completions providers. Each matches a detect.go family by name
	// or base URL, so the shared baseline plus detection carries the quirks.
	FlavorDeepSeek: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://api.deepseek.com",
		apiKeyEnv: "DEEPSEEK_API_KEY",
		discover:  true,
		caps:      chatCaps(),
	},
	FlavorTogether: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://api.together.ai/v1",
		apiKeyEnv: "TOGETHER_API_KEY",
		discover:  true,
		caps:      chatCaps(),
	},
	FlavorXAI: {
		dialect:   DialectOpenAIResponses,
		baseURL:   "https://api.x.ai/v1",
		apiKeyEnv: "XAI_API_KEY",
		discover:  true,
		caps: Capabilities{
			Dialect:         DialectOpenAIResponses,
			Reasoning:       true,
			Thinking:        ThinkingOpenAI,
			ReasoningReplay: true,
			Tokenizer:       TokenizerLocalEstimate,
			MaxTokensField:  fieldMaxOutputTokens,
			SystemAsRole:    true,
			ParallelTools:   true,
			StreamUsage:     true,
			Temperature:     true,
			ToolChoice:      true,
		},
	},
	FlavorGroq: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://api.groq.com/openai/v1",
		apiKeyEnv: "GROQ_API_KEY",
		discover:  true,
		caps:      chatCaps(),
	},
	FlavorMistral: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://api.mistral.ai/v1",
		apiKeyEnv: "MISTRAL_API_KEY",
		discover:  true,
		caps:      chatCaps(),
	},
	FlavorMoonshotAI: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://api.moonshot.ai/v1",
		apiKeyEnv: "MOONSHOT_API_KEY",
		discover:  true,
		caps:      chatCaps(),
	},
	FlavorMoonshotCN: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://api.moonshot.cn/v1",
		apiKeyEnv: "MOONSHOT_API_KEY",
		discover:  true,
		caps:      chatCaps(),
	},
	// Google's OpenAI-compatible surface in front of Gemini.
	FlavorGoogle: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://generativelanguage.googleapis.com/v1beta/openai",
		apiKeyEnv: "GEMINI_API_KEY",
		discover:  true,
		caps:      chatCaps(),
	},
	FlavorCerebras: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://api.cerebras.ai/v1",
		apiKeyEnv: "CEREBRAS_API_KEY",
		discover:  true,
		caps:      chatCaps(),
	},
	FlavorNVIDIA: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://integrate.api.nvidia.com/v1",
		apiKeyEnv: "NVIDIA_API_KEY",
		discover:  true,
		caps:      chatCaps(),
	},
	FlavorHuggingFace: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://router.huggingface.co/v1",
		apiKeyEnv: "HF_TOKEN",
		discover:  true,
		caps:      chatCaps(),
	},
	FlavorBaseten: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://inference.baseten.co/v1",
		apiKeyEnv: "BASETEN_API_KEY",
		discover:  true,
		caps:      chatCaps(),
	},
	FlavorAntLing: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://api.ant-ling.com/v1",
		apiKeyEnv: "ANT_LING_API_KEY",
		discover:  true,
		caps:      chatCaps(),
	},
	FlavorQwenPlan: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1",
		apiKeyEnv: "QWEN_TOKEN_PLAN_API_KEY",
		discover:  true,
		caps:      chatCaps(),
	},
	FlavorQwenPlanCN: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1",
		apiKeyEnv: "QWEN_TOKEN_PLAN_CN_API_KEY",
		discover:  true,
		caps:      chatCaps(),
	},
	FlavorXiaomi: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://api.xiaomimimo.com/v1",
		apiKeyEnv: "XIAOMI_API_KEY",
		discover:  true,
		caps:      chatCaps(),
	},
	FlavorXiaomiCN: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://token-plan-cn.xiaomimimo.com/v1",
		apiKeyEnv: "XIAOMI_TOKEN_PLAN_CN_API_KEY",
		discover:  true,
		caps:      chatCaps(),
	},
	FlavorXiaomiSGP: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://token-plan-sgp.xiaomimimo.com/v1",
		apiKeyEnv: "XIAOMI_TOKEN_PLAN_SGP_API_KEY",
		discover:  true,
		caps:      chatCaps(),
	},
	FlavorXiaomiAMS: {
		dialect:   DialectOpenAICompletions,
		baseURL:   "https://token-plan-ams.xiaomimimo.com/v1",
		apiKeyEnv: "XIAOMI_TOKEN_PLAN_AMS_API_KEY",
		discover:  true,
		caps:      chatCaps(),
	},
	// Anthropic-protocol endpoints served by other vendors. None offer a model
	// list, so the wizard asks for one id and config declares the rest.
	FlavorKimi: {
		dialect:   DialectAnthropic,
		baseURL:   "https://api.kimi.com/coding",
		apiKeyEnv: "KIMI_API_KEY",
		caps:      anthropicCaps(),
	},
	FlavorMiniMax: {
		dialect:   DialectAnthropic,
		baseURL:   "https://api.minimax.io/anthropic",
		apiKeyEnv: "MINIMAX_API_KEY",
		caps:      anthropicCaps(),
	},
	FlavorMiniMaxCN: {
		dialect:   DialectAnthropic,
		baseURL:   "https://api.minimaxi.com/anthropic",
		apiKeyEnv: "MINIMAX_CN_API_KEY",
		caps:      anthropicCaps(),
	},
	FlavorFireworks: {
		dialect:   DialectAnthropic,
		baseURL:   "https://api.fireworks.ai/inference",
		apiKeyEnv: "FIREWORKS_API_KEY",
		caps:      anthropicCaps(),
	},
	FlavorLMStudio: {
		dialect:  DialectOpenAICompletions,
		baseURL:  "http://localhost:1234/v1",
		discover: true,
		// a just in time model load holds the headers and then the stream for
		// minutes, so neither bound applies
		timeouts: Timeouts{Header: dur(0), Idle: dur(0), Connect: dur(5 * time.Second)},
		caps: Capabilities{
			Dialect:        DialectOpenAICompletions,
			Reasoning:      true,
			Thinking:       ThinkingThinkTags,
			ReasoningField: fieldReasoningConten,
			ThinkOpen:      thinkOpenTag,
			ThinkClose:     thinkCloseTag,
			Tokenizer:      TokenizerLocalEstimate,
			MaxTokensField: fieldMaxTokens,
			SystemAsRole:   true,
			StreamUsage:    true,
			Temperature:    true,
			ToolChoice:     true,
			// the chat-completions default emits reasoning_effort
			SupportsReasoningEffort: true,
			SupportsFinishReason:    true,
			SupportsStrict:          true,
		},
	},
	FlavorLlamaCpp: {
		dialect:  DialectOpenAICompletions,
		baseURL:  "http://localhost:8080",
		discover: true,
		timeouts: Timeouts{Idle: dur(0), Connect: dur(5 * time.Second)},
		caps: Capabilities{
			Dialect:        DialectOpenAICompletions,
			Reasoning:      true,
			Thinking:       ThinkingThinkTags,
			ReasoningField: fieldReasoningConten,
			ThinkOpen:      thinkOpenTag,
			ThinkClose:     thinkCloseTag,
			Tokenizer:      TokenizerRemoteTokenize,
			MaxTokensField: fieldMaxTokens,
			SystemAsRole:   true,
			// the chat-completions default emits reasoning_effort
			SupportsReasoningEffort: true,
			// older builds reject the unknown stream_options key, discovery
			// turns it back on when the build reports support
			StreamUsage:          false,
			Temperature:          true,
			ToolChoice:           true,
			SupportsFinishReason: true,
			SupportsStrict:       true,
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
			// the chat-completions default emits reasoning_effort
			SupportsReasoningEffort: true,
			SupportsFinishReason:    true,
			SupportsStrict:          true,
		},
	},
}

// flavorFor returns the flavor for a provider entry, defaulting to the one its
// configuration key names so a provider called "lmstudio" needs no flavor field.
func flavorFor(name string, cfg ProviderConfig) Flavor {
	if cfg.Flavor != FlavorUnset && cfg.Flavor != FlavorUnknown {
		return cfg.Flavor
	} else if f, ok := flavorNames.lookup(name); ok && f != FlavorUnset && f != FlavorUnknown {
		return f
	}
	return FlavorGeneric
}

// FlavorProfile is a flavor's compiled-in endpoint and credential defaults.
type FlavorProfile struct {
	BaseURL   string
	APIKeyEnv string
	Discover  bool
}

// LookupFlavor returns the profile for a flavor name.
func LookupFlavor(name string) (FlavorProfile, bool) {
	f, ok := flavorNames.lookup(name)
	if !ok || f == FlavorUnset || f == FlavorUnknown {
		return FlavorProfile{}, false
	}
	def := flavorDefaults[f]
	return FlavorProfile{BaseURL: def.baseURL, APIKeyEnv: def.apiKeyEnv, Discover: def.discover}, true
}

// dialectFor returns the wire dialect a provider speaks: its configured api,
// or the flavor default when unset.
func dialectFor(cfg ProviderConfig, defDialect Dialect) Dialect {
	if cfg.API != DialectUnset {
		return cfg.API
	}
	return defDialect
}

// resolveBaseURL returns the endpoint a provider talks to: its configured base,
// or the flavor default when unset.
func resolveBaseURL(cfg, def string) string {
	if cfg != "" {
		return cfg
	}
	return def
}

// modelEndpoint returns the dialect and base URL one model talks over: its own
// when set, otherwise the provider's.
func modelEndpoint(ctx modelContext, mc ModelConfig) (Dialect, string) {
	dialect, baseURL := ctx.dialect, ctx.baseURL
	if mc.API != DialectUnset {
		dialect = mc.API
	}
	if mc.BaseURL != "" {
		baseURL = mc.BaseURL
	}
	return dialect, baseURL
}

// resolveCaps layers detection and compat blocks over the flavor defaults in
// order, later winning field by field.
func resolveCaps(base Capabilities, layers ...*Compat) Capabilities {
	for _, l := range layers {
		base = applyCompat(base, l)
	}
	return base
}

// applyCompat overlays one compat block, leaving unset fields alone. ThinkingFormat
// resolves through the enum lookup and is left unchanged on an unknown value,
// which compatWarnings reports separately.
func applyCompat(c Capabilities, o *Compat) Capabilities {
	if o == nil {
		return c
	}
	if o.ThinkingFormat != nil {
		if f, ok := parseThinkingFormat(*o.ThinkingFormat); ok {
			c.Thinking = f
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
	c.SessionAffinityFormat = orStr(c.SessionAffinityFormat, o.SessionAffinityFormat)
	c.DeferredTools = orStr(c.DeferredTools, o.DeferredTools)

	c.DeveloperRole = orBool(c.DeveloperRole, o.SupportsDeveloperRole)
	c.SystemAsRole = orBool(c.SystemAsRole, o.SupportsSystemRole)
	c.Temperature = orBool(c.Temperature, o.SupportsTemperature)
	c.ParallelTools = orBool(c.ParallelTools, o.SupportsParallelTools)
	// supportsStreamUsage is ajent's historical name; supportsUsageInStreaming is canonical
	c.StreamUsage = orBool(orBool(c.StreamUsage, o.SupportsUsageInStreaming), o.SupportsStreamUsage)
	c.ToolChoice = orBool(c.ToolChoice, o.SupportsToolChoice)
	c.Store = orBool(c.Store, o.SupportsStore)
	c.PromptCache = orBool(c.PromptCache, o.SupportsPromptCache)
	c.LongCache = orBool(c.LongCache, o.SupportsLongCache)
	c.Images = orBool(c.Images, o.SupportsImages)
	c.ReasoningReplay = orBool(c.ReasoningReplay, o.RequiresReasoningReplay)
	c.ReplayReasoning = orBool(c.ReplayReasoning, o.RequiresReasoningContent)

	// supportsReasoningEffort is a pure gate now; it never picks the encoding.
	c.SupportsReasoningEffort = orBool(c.SupportsReasoningEffort, o.SupportsReasoningEffort)
	c.SupportsFinishReason = orBool(c.SupportsFinishReason, o.SupportsFinishReason)
	c.SupportsStrict = orBool(c.SupportsStrict, o.SupportsStrictMode)
	c.SupportsStrictTools = orBool(c.SupportsStrictTools, o.SupportsStrictTools)
	c.SupportsGrammarTools = orBool(c.SupportsGrammarTools, o.SupportsOpenAIGrammarTools)
	// supportsThinkingTokenBudget is the boolean alias for thinkingTokenBudgetField,
	// so the canonical spelling wins when a block carries both
	if o.SupportsThinkingTokenBudget != nil {
		c.ThinkingBudgetField = ""
		if *o.SupportsThinkingTokenBudget {
			c.ThinkingBudgetField = fieldThinkingTokenBudget
		}
	}
	c.ThinkingBudgetField = orStr(c.ThinkingBudgetField, o.ThinkingTokenBudgetField)
	c.ForceAdaptiveThinking = orBool(c.ForceAdaptiveThinking, o.ForceAdaptiveThinking)
	c.AllowEmptySignature = orBool(c.AllowEmptySignature, o.AllowEmptySignature)
	c.RequiresThinkingAsText = orBool(c.RequiresThinkingAsText, o.RequiresThinkingAsText)
	c.RequiresToolResultName = orBool(c.RequiresToolResultName, o.RequiresToolResultName)
	c.RequiresAssistantAfterToolResult = orBool(c.RequiresAssistantAfterToolResult, o.RequiresAssistantAfterToolResult)
	c.EagerToolInputStreaming = orBool(c.EagerToolInputStreaming, o.EagerToolInputStreaming)
	c.CacheControlOnTools = orBool(c.CacheControlOnTools, o.SupportsCacheControlOnTools)
	c.ToolReferences = orBool(c.ToolReferences, o.SupportsToolReferences)
	c.SupportsAdditionalTools = orBool(c.SupportsAdditionalTools, o.SupportsAdditionalTools)
	c.SupportsToolSearch = orBool(c.SupportsToolSearch, o.SupportsToolSearch)
	c.SupportsExplicitPromptCache = orBool(c.SupportsExplicitPromptCache, o.SupportsExplicitPromptCache)
	c.ZaiToolStream = orBool(c.ZaiToolStream, o.ZaiToolStream)
	c.SessionAffinity = orBool(c.SessionAffinity, o.SendSessionAffinityHeaders)

	if len(o.ChatTemplateKwargs) > 0 {
		c.ChatTemplateKwargs = maps.Clone(o.ChatTemplateKwargs)
	}
	if len(o.ChatTemplateArgs) > 0 {
		c.ChatTemplateArgs = maps.Clone(o.ChatTemplateArgs)
	}
	if len(o.ExtraBody) > 0 {
		c.ExtraBody = maps.Clone(o.ExtraBody)
	}
	if len(o.OpenRouterRouting) > 0 {
		c.OpenRouterRouting = o.OpenRouterRouting
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

// defaultContextWindow and defaultMaxTokens fill what a model entry omits, so
// an id-only line is a complete one.
const (
	defaultContextWindow = 128000
	defaultMaxTokens     = 16384
)

// modelContext carries the provider level fallbacks a model resolves against.
type modelContext struct {
	provider string
	dialect  Dialect // the provider's, which ModelConfig.API overrides
	baseURL  string  // the provider's, which ModelConfig.BaseURL overrides
}

// applyModelDefaults fills what a model entry left unset with the schema defaults. It
// runs after configuration and discovery have merged, so a discovered value wins.
func applyModelDefaults(mc ModelConfig) ModelConfig {
	if mc.Name == "" {
		mc.Name = mc.ID
	}
	if mc.ContextWindow == nil {
		mc.ContextWindow = ptrOf(defaultContextWindow)
	}
	if mc.MaxTokens == nil {
		mc.MaxTokens = ptrOf(defaultMaxTokens)
	}
	return mc
}

// resolveModel builds a Model from a config entry against a provider's defaults, layering
// flavor caps then detection then the compat blocks, and finally the schema defaults.
// Detection runs only for chat-completions and never overrides configured compat.
func resolveModel(ctx modelContext, base Capabilities, providerCompat *Compat, mc ModelConfig) Model {
	mc = applyModelDefaults(mc)
	dialect, baseURL := modelEndpoint(ctx, mc)

	layers := []*Compat{providerCompat, mc.Compat}
	if dialect == DialectOpenAICompletions {
		d := detectCompat(ctx.provider, baseURL, mc.ID)
		layers = append([]*Compat{&d}, layers...)
	}
	caps := resolveCaps(base, layers...)
	caps.Dialect = dialect

	if mc.Reasoning != nil {
		caps.Reasoning = *mc.Reasoning
	}
	var maxOut int
	if mc.MaxTokens != nil {
		maxOut = *mc.MaxTokens
	}
	if len(mc.LevelMap) > 0 {
		caps.LevelMap = maps.Clone(mc.LevelMap)
	}
	// sampling params ride the same verbatim body escape hatch as extraBody, so
	// every adapter that folds one in picks up both without a new code path.
	if len(mc.SamplingParams) > 0 {
		caps.ExtraBody = mergeExtra(caps.ExtraBody, rawSamplingParams(mc.SamplingParams))
	}
	if !caps.Reasoning {
		caps.ReasoningReplay = false
	} else {
		// start from the ladder, then overlay configured rungs so a partial map keeps the rest of it
		caps.Budgets = defaultBudgets(maxOut)
		for l, b := range mc.ThinkingBudgets {
			caps.Budgets[l] = b
		}
	}

	m := Model{
		Provider: ctx.provider,
		ID:       mc.ID,
		Name:     mc.Name,
		BaseURL:  baseURL,
		Aliases:  slices.Clone(mc.Aliases),
		Input:    slices.Clone(mc.Input),
		Caps:     caps,
		Headers:  maps.Clone(mc.Headers),
	}
	if maxOut > 0 {
		m.MaxOutput = maxOut
	}
	if mc.ContextWindow != nil {
		m.ContextWindow = *mc.ContextWindow
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
