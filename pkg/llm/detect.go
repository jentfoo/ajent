package llm

import "strings"

// detectCompat derives the quirks pi auto-detects for a chat-completions
// provider from its name and base URL, returning a sparse Compat that layers
// under configured compat blocks. It returns a zero Compat when no vendor family
// matches, so generic endpoints keep their flavor defaults.
//
// Detection never inspects a model id except openrouter's anthropic/ and openai/
// prefixes, and it never sets Reasoning (that comes from the model entry).
func detectCompat(provider, baseURL, modelID string) Compat {
	var c Compat

	isZai := provider == "zai" || provider == "zai-coding-cn" ||
		strings.Contains(baseURL, "api.z.ai") || strings.Contains(baseURL, "open.bigmodel.cn")
	isTogether := provider == "together" ||
		strings.Contains(baseURL, "api.together.ai") || strings.Contains(baseURL, "api.together.xyz")
	isMoonshot := provider == "moonshotai" || provider == "moonshotai-cn" ||
		strings.Contains(baseURL, "api.moonshot.")
	isOpenRouter := provider == "openrouter" || strings.Contains(baseURL, "openrouter.ai")
	isCFWorkersAI := provider == "cloudflare-workers-ai" || strings.Contains(baseURL, "api.cloudflare.com")
	isCFGateway := provider == "cloudflare-ai-gateway" || strings.Contains(baseURL, "gateway.ai.cloudflare.com")
	isNvidia := provider == "nvidia" || strings.Contains(baseURL, "integrate.api.nvidia.com")
	isAntLing := provider == "ant-ling" || strings.Contains(baseURL, "api.ant-ling.com")
	isDeepSeek := provider == "deepseek" || strings.Contains(strings.ToLower(baseURL), "deepseek.com")
	isOpenCode := provider == "opencode" || strings.Contains(baseURL, "opencode.ai")

	matched := isZai || isTogether || isMoonshot || isOpenRouter || isCFWorkersAI ||
		isCFGateway || isNvidia || isAntLing || isDeepSeek ||
		provider == "cerebras" || strings.Contains(baseURL, "cerebras.ai") ||
		provider == "xai" || strings.Contains(baseURL, "api.x.ai") ||
		strings.Contains(baseURL, "chutes.ai") || isOpenCode
	if !matched {
		return c
	}

	isNonStandard := isNvidia || provider == "cerebras" || strings.Contains(baseURL, "cerebras.ai") ||
		provider == "xai" || strings.Contains(baseURL, "api.x.ai") || isTogether ||
		strings.Contains(baseURL, "chutes.ai") || isDeepSeek || isZai || isMoonshot ||
		isOpenCode || isCFWorkersAI || isCFGateway || isAntLing

	useMaxTokens := strings.Contains(baseURL, "chutes.ai") || isDeepSeek || isMoonshot ||
		isCFGateway || isTogether || isNvidia || isAntLing || isZai

	isGrok := provider == "xai" || strings.Contains(baseURL, "api.x.ai")
	openRouterDevRoleModel := isOpenRouter &&
		(strings.HasPrefix(modelID, "anthropic/") || strings.HasPrefix(modelID, "openai/"))
	// pi keys this on the literal provider name, not the base URL
	cacheControlFormat := ""
	if provider == "openrouter" && strings.HasPrefix(modelID, "anthropic/") {
		cacheControlFormat = "anthropic"
	}

	c.SupportsStore = ptrOf(!isNonStandard)
	c.SupportsDeveloperRole = ptrOf(openRouterDevRoleModel || (!isNonStandard && !isOpenRouter))
	c.SupportsReasoningEffort = ptrOf(!isGrok && !isZai && !isMoonshot && !isTogether &&
		!isCFGateway && !isNvidia && !isAntLing)
	c.SupportsStreamUsage = ptrOf(true) // pi's supportsUsageInStreaming default
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
		// pi remaps opencode-go reasoning to the reasoning_content field
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
		c.ThinkingFormat = ptrOf("openrouter")
	default:
		c.ThinkingFormat = ptrOf("openai")
	}

	if cacheControlFormat != "" {
		c.CacheControlFormat = &cacheControlFormat
	}
	sessionAffinity := "openai"
	if isOpenRouter {
		sessionAffinity = "openrouter"
	}
	c.SessionAffinityFormat = &sessionAffinity

	longCache := !isTogether && !isCFWorkersAI && !isCFGateway && !isNvidia && !isAntLing
	c.SupportsLongCache = ptrOf(longCache)

	return c
}
