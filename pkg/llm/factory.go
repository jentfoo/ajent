package llm

import (
	"fmt"
	"maps"
	"net/http"
	"os"
)

// ProviderOptions configures provider construction.
type ProviderOptions struct {
	Env       func(string) string // defaults to os.Getenv
	Transport http.RoundTripper   // tests inject
	Log       func(HTTPLogEvent)

	// Dialect and BaseURL are the model's resolved endpoint, overriding what the
	// provider entry and flavor would give. Callers holding a Model pass its
	// values so the adapter speaks exactly what the registry resolved.
	Dialect Dialect
	BaseURL string
}

// NewProvider builds the adapter serving one configured provider.
func NewProvider(name string, cfg ProviderConfig, flavor Flavor, opts ProviderOptions) (Provider, error) {
	env := opts.Env
	if env == nil {
		env = os.Getenv
	}
	def := flavorDefaults[flavor]

	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = resolveBaseURL(cfg.BaseURL, def.baseURL)
	}
	if baseURL == "" {
		return nil, fmt.Errorf("llm: provider %s has no baseUrl", name)
	}

	dialect := opts.Dialect
	if dialect == DialectUnset {
		dialect = dialectFor(cfg, def.dialect)
	}

	headers := maps.Clone(cfg.Headers)
	if headers == nil {
		headers = make(map[string]string)
	}
	if def.apiKeyEnv != "" || cfg.APIKey != "" || cfg.APIKeyEnv != "" {
		key, err := resolveKey(name, cfg.APIKey, cfg.APIKeyEnv, def.apiKeyEnv, env)
		if err != nil {
			return nil, err
		}
		applyAuthHeader(headers, flavor, key)
	}

	client, err := newHTTPClient(clientOptions{
		provider:  name,
		baseURL:   baseURL,
		headers:   headers,
		timeouts:  mergeTimeouts(def.timeouts, cfg.Timeouts),
		retry:     cfg.Retry,
		transport: opts.Transport,
		log:       opts.Log,
	})
	if err != nil {
		return nil, err
	}

	switch dialect {
	case DialectAnthropic:
		return newAnthropicProvider(name, client), nil
	case DialectOpenAIResponses:
		return newResponsesProvider(name, client), nil
	case DialectOpenAICompletions:
		return &compatProvider{client: client, profile: profileFor(name, flavor, cfg)}, nil
	default:
		return nil, fmt.Errorf("llm: provider %s has no api dialect", name)
	}
}

// anthropicVersion is the Messages API version header value.
const anthropicVersion = "2023-06-01"

// applyAuthHeader sets the credential header a flavor expects.
//
// It keys on flavor rather than the resolved dialect, which phase 17 corrects.
// Per-model api made that gap newly reachable: a model declaring
// api:"anthropic-messages" under a generic-flavor provider gets
// Authorization: Bearer instead of x-api-key, and no anthropic-version header.
func applyAuthHeader(headers map[string]string, flavor Flavor, key string) {
	if key == "" {
		return
	}
	switch flavor {
	case FlavorAnthropic:
		headers["x-api-key"] = key
		if _, ok := headers["anthropic-version"]; !ok {
			headers["anthropic-version"] = anthropicVersion
		}
	default:
		headers["Authorization"] = "Bearer " + key
	}
}

// mergeTimeouts overlays configured timeouts on the flavor defaults.
func mergeTimeouts(def, cfg Timeouts) Timeouts {
	out := def
	if cfg.Connect != nil {
		out.Connect = cfg.Connect
	}
	if cfg.TLS != nil {
		out.TLS = cfg.TLS
	}
	if cfg.Header != nil {
		out.Header = cfg.Header
	}
	if cfg.Idle != nil {
		out.Idle = cfg.Idle
	}
	if cfg.Total != nil {
		out.Total = cfg.Total
	}
	return out
}

// profileFor returns the chat-completions profile for a flavor.
func profileFor(name string, flavor Flavor, cfg ProviderConfig) compatProfile {
	p := compatProfile{name: name, classify: compatClassifier(name, flavor)}
	switch flavor {
	case FlavorLlamaCpp:
		p.path = "/v1/chat/completions"
		p.decorate = decorateLlamaCpp
	case FlavorOpenRouter:
		p.decorate = decorateOpenRouter(cfg.Routing)
		p.extra = openRouterExtra
	case FlavorLMStudio:
		p.decorate = decorateLMStudio
	}
	return p
}
