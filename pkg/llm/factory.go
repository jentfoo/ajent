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
}

// NewProvider builds the adapter serving one configured provider.
func NewProvider(name string, cfg ProviderConfig, flavor Flavor, opts ProviderOptions) (Provider, error) {
	env := opts.Env
	if env == nil {
		env = os.Getenv
	}
	def := flavorDefaults[flavor]

	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = def.baseURL
	}
	if baseURL == "" {
		return nil, fmt.Errorf("llm: provider %s has no baseUrl", name)
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

	dialect := cfg.API
	if dialect == DialectUnset {
		dialect = def.dialect
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
