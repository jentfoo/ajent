package llm

import "strings"

// isOpenRouterEndpoint reports whether an endpoint is openrouter, keyed on the
// provider name or base URL case-insensitively. Shared by capability detection
// and the compat profile so both agree on what the endpoint is, whatever key
// names it.
func isOpenRouterEndpoint(provider, baseURL string) bool {
	return strings.EqualFold(provider, openRouterAffinityFormat) ||
		strings.Contains(strings.ToLower(baseURL), "openrouter.ai")
}

// detectCompat derives the quirks it auto-detects for a chat-completions
// provider from its name and base URL, returning a sparse Compat that layers
// under configured compat blocks. It returns a zero Compat when no vendor family
// matches, so generic endpoints keep their flavor defaults.
//
// Detection never inspects a model id except openrouter's anthropic/ and openai/
// prefixes, and it never sets Reasoning (that comes from the model entry).
func detectCompat(provider, baseURL, modelID string) Compat {
	var c Compat

	// normalized once so no family check can drift on configured casing
	provider = strings.ToLower(provider)
	baseURL = strings.ToLower(baseURL)

	isZai := provider == "zai" || provider == "zai-coding-cn" ||
		strings.Contains(baseURL, "api.z.ai") || strings.Contains(baseURL, "open.bigmodel.cn")
	isTogether := provider == "together" ||
		strings.Contains(baseURL, "api.together.ai") || strings.Contains(baseURL, "api.together.xyz")
	isMoonshot := provider == "moonshotai" || provider == "moonshotai-cn" ||
		strings.Contains(baseURL, "api.moonshot.")
	isOpenRouter := isOpenRouterEndpoint(provider, baseURL)
	isCFWorkersAI := provider == "cloudflare-workers-ai" || strings.Contains(baseURL, "api.cloudflare.com")
	isCFGateway := provider == "cloudflare-ai-gateway" || strings.Contains(baseURL, "gateway.ai.cloudflare.com")
	isNvidia := provider == "nvidia" || strings.Contains(baseURL, "integrate.api.nvidia.com")
	isAntLing := provider == "ant-ling" || strings.Contains(baseURL, "api.ant-ling.com")
	isDeepSeek := provider == "deepseek" || strings.Contains(baseURL, "deepseek.com")
	isOpenCode := provider == "opencode" || strings.Contains(baseURL, "opencode.ai")
	isOpenAI := provider == "openai" || strings.Contains(baseURL, "api.openai.com")
	isCerebras := provider == "cerebras" || strings.Contains(baseURL, "cerebras.ai")
	isGrok := provider == "xai" || strings.Contains(baseURL, "api.x.ai")
	isChutes := strings.Contains(baseURL, "chutes.ai")

	matched := isZai || isTogether || isMoonshot || isOpenRouter || isCFWorkersAI ||
		isCFGateway || isNvidia || isAntLing || isDeepSeek || isOpenAI ||
		isCerebras || isGrok || isChutes || isOpenCode
	if !matched {
		return c
	}

	isNonStandard := isNvidia || isCerebras || isGrok || isTogether || isChutes ||
		isDeepSeek || isZai || isMoonshot || isOpenCode || isCFWorkersAI ||
		isCFGateway || isAntLing

	useMaxTokens := isChutes || isDeepSeek || isMoonshot ||
		isCFGateway || isTogether || isNvidia || isAntLing || isZai

	openRouterDevRoleModel := isOpenRouter &&
		(strings.HasPrefix(modelID, "anthropic/") || strings.HasPrefix(modelID, "openai/"))
	// cache-control format keys on the literal provider name, not the base URL
	var cacheControlFormat string
	if provider == openRouterAffinityFormat && strings.HasPrefix(modelID, "anthropic/") {
		cacheControlFormat = providerAnthropic
	}

	c.SupportsStore = ptrOf(!isNonStandard)
	c.SupportsDeveloperRole = ptrOf(openRouterDevRoleModel || (!isNonStandard && !isOpenRouter))
	c.SupportsReasoningEffort = ptrOf(!isGrok && !isZai && !isMoonshot && !isTogether &&
		!isCFGateway && !isNvidia && !isAntLing)
	// canonical, so a merged block can never carry both spellings
	c.SupportsUsageInStreaming = ptrOf(true) // stream usage is supported by default
	c.SupportsFinishReason = ptrOf(true)
	c.SupportsStrictMode = ptrOf(!isMoonshot && !isTogether && !isCFGateway && !isNvidia)

	maxTokensField := "max_completion_tokens"
	if useMaxTokens {
		maxTokensField = fieldMaxTokens
	}
	c.MaxTokensField = &maxTokensField

	c.RequiresToolResultName = ptrOf(false)
	c.RequiresAssistantAfterToolResult = ptrOf(false)
	c.RequiresThinkingAsText = ptrOf(false)
	c.RequiresReasoningContent = ptrOf(isDeepSeek)
	if isOpenCode {
		// opencode-go reasoning maps onto the reasoning_content field
		reasoningField := fieldReasoningConten
		c.ReasoningContentField = &reasoningField
	}

	switch {
	case isDeepSeek:
		c.ThinkingFormat = ptrOf("deepseek")
	case isZai:
		c.ThinkingFormat = ptrOf("zai")
	case isTogether:
		c.ThinkingFormat = ptrOf("together")
	case isAntLing:
		c.ThinkingFormat = ptrOf("ant-ling")
	case isOpenRouter:
		c.ThinkingFormat = ptrOf(openRouterAffinityFormat)
	default:
		c.ThinkingFormat = ptrOf(providerOpenAI)
	}

	if cacheControlFormat != "" {
		c.CacheControlFormat = &cacheControlFormat
	}
	sessionAffinity := providerOpenAI
	if isOpenRouter {
		sessionAffinity = openRouterAffinityFormat
	}
	c.SessionAffinityFormat = &sessionAffinity

	longCache := !isTogether && !isCFWorkersAI && !isCFGateway && !isNvidia && !isAntLing
	c.SupportsLongCache = ptrOf(longCache)

	// the routing key is verified on api.openai.com only
	c.SupportsExplicitPromptCache = ptrOf(false)

	return c
}
