package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jentfoo/ajent/pkg/config"
)

const (
	// CacheFileName holds discovery results between runs.
	CacheFileName = "models.json"
	cacheVersion  = 1
	// hosted catalogues change rarely, a local server's loaded model does not
	hostedTTL = 24 * time.Hour
	localTTL  = time.Minute
)

// CacheEntry is one provider's cached discovery result.
type CacheEntry struct {
	Source       string        `json:"source,omitempty"`
	Models       []ModelConfig `json:"models"`
	CheckedAt    int64         `json:"checkedAt"` // unix milliseconds
	LastModified string        `json:"lastModified,omitempty"`
	ETag         string        `json:"etag,omitempty"`
}

// Cache is the decoded discovery cache.
type Cache struct {
	Version   int                   `json:"version"`
	Providers map[string]CacheEntry `json:"providers"`
}

// LoadCache reads the discovery cache. A missing, unreadable or outdated cache
// yields an empty result rather than an error, since it is only ever a hint.
func LoadCache(path string) map[string]CacheEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var c Cache
	if err = json.Unmarshal(data, &c); err != nil || c.Version != cacheVersion {
		return nil
	}
	return c.Providers
}

// LoadUserCache reads the discovery cache from the configuration directory.
func LoadUserCache() map[string]CacheEntry {
	path, err := config.CachePath(CacheFileName)
	if err != nil {
		return nil
	}
	return LoadCache(path)
}

// SaveCache writes the discovery cache atomically.
func SaveCache(path string, entries map[string]CacheEntry) error {
	data, err := json.MarshalIndent(Cache{Version: cacheVersion, Providers: entries}, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(path, data, config.SecretPerm)
}

// SaveUserCache writes the discovery cache to the configuration directory.
func SaveUserCache(entries map[string]CacheEntry) error {
	path, err := config.CachePath(CacheFileName)
	if err != nil {
		return err
	}
	return SaveCache(path, entries)
}

// ttlFor returns how long a flavor's discovery result stays fresh.
func ttlFor(f Flavor) time.Duration {
	switch f {
	case FlavorLMStudio, FlavorLlamaCpp:
		return localTTL
	default:
		return hostedTTL
	}
}

// fresh reports whether an entry is still within its time to live.
func fresh(e CacheEntry, ttl time.Duration, now time.Time) bool {
	if e.CheckedAt == 0 || len(e.Models) == 0 {
		return false
	}
	return now.Sub(time.UnixMilli(e.CheckedAt)) < ttl
}

// modelParser turns a provider's discovery response into model entries.
type modelParser func(body []byte) ([]ModelConfig, error)

// openAIModelsPath is the standard chat-completions list endpoint every server in
// that family speaks. It backs up flavors whose own endpoint cannot describe what
// is loaded.
const openAIModelsPath = "/v1/models"

// resolveDiscoveryPath joins a discovery endpoint onto a provider's base URL,
// collapsing a /v1 prefix the base already carries. The OpenAI model list lives at
// "/v1/models" on a bare host and at "/models" relative to a base that ends in
// "/v1"; asking for both produces a 404.
func resolveDiscoveryPath(basePath, path string) string {
	if strings.HasPrefix(path, "/v1/") && hasV1Suffix(basePath) {
		return strings.TrimPrefix(path, "/v1")
	}
	return path
}

// hasV1Suffix reports whether a URL path already ends in the /v1 API prefix.
func hasV1Suffix(p string) bool {
	p = strings.TrimRight(p, "/")
	return p == "v1" || strings.HasSuffix(p, "/v1")
}

// discoveryCandidate is one endpoint a flavor can be asked for its model list.
type discoveryCandidate struct {
	path  string
	parse modelParser
}

// discoverySpec lists a flavor's endpoints in order; the first that yields usable
// models wins, so an unhelpful native response falls back to /v1/models.
type discoverySpec struct {
	candidates []discoveryCandidate
}

// discoverySpecs is the set of providers that can be asked what they serve.
// Anything absent is configuration only. The generic flavor discovers through the
// standard OpenAI list, opt-in via "discover": true like every other provider.
var discoverySpecs = map[Flavor]discoverySpec{
	FlavorOpenRouter: {candidates: []discoveryCandidate{{path: "/models", parse: parseOpenRouterModels}}},
	FlavorLMStudio:   {candidates: []discoveryCandidate{{path: "/api/v0/models", parse: parseLMStudioModels}}},
	// llama.cpp reports one loaded model via /props; in router mode that is
	// useless, so fall back to the OpenAI-compatible list.
	FlavorLlamaCpp: {candidates: []discoveryCandidate{
		{path: "/props", parse: parseLlamaProps},
		{path: openAIModelsPath, parse: parseOpenAIModels},
	}},
	FlavorGeneric: {candidates: []discoveryCandidate{{path: openAIModelsPath, parse: parseOpenAIModels}}},
}

// DiscoverOptions configures a discovery pass.
type DiscoverOptions struct {
	Env       func(string) string
	Transport http.RoundTripper // tests inject
	Log       func(HTTPLogEvent)
	Now       func() time.Time // defaults to time.Now
	Force     bool             // ignore the time to live
}

// Discover refreshes the cached model list for every provider that supports it
// and has not opted out, returning the updated cache and any warnings.
//
// It is never fatal: a provider that fails keeps whatever was cached before, so
// starting offline still works.
func Discover(ctx context.Context, f File, cache map[string]CacheEntry, opts DiscoverOptions) (map[string]CacheEntry, []string) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	out := maps.Clone(cache)
	if out == nil {
		out = make(map[string]CacheEntry)
	}

	var warnings []string
	for name, cfg := range f.Providers {
		if cfg.Disabled {
			continue
		}
		flavor := flavorFor(name, cfg)
		spec, ok := discoverySpecs[flavor]
		if !ok {
			continue
		}
		def := flavorDefaults[flavor]
		if !orBool(def.discover, cfg.Discover) {
			continue // explicitly opted out
		}
		prev := out[name]
		if !opts.Force && fresh(prev, ttlFor(flavor), now()) {
			continue
		}

		entry, err := discoverOne(ctx, name, cfg, flavor, spec, prev, now(), opts)
		if err != nil {
			warnings = append(warnings, "discovery for "+name+" failed: "+err.Error())
			continue // the stale entry stays in place
		}
		out[name] = entry
	}
	return out, warnings
}

// discoverOne builds a client for a provider and refreshes its entry, trying each
// candidate endpoint until one yields usable models. An unreachable server fails
// fast rather than walking the rest of the list.
func discoverOne(ctx context.Context, name string, cfg ProviderConfig, flavor Flavor,
	spec discoverySpec, prev CacheEntry, now time.Time, opts DiscoverOptions,
) (CacheEntry, error) {
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
		return prev, errors.New("no baseUrl")
	}
	headers := maps.Clone(cfg.Headers)
	if headers == nil {
		headers = make(map[string]string)
	}
	// a missing key is not fatal here, some endpoints list models unauthenticated
	if key, err := resolveKey(name, cfg.APIKey, cfg.APIKeyEnv, def.apiKeyEnv, env); err == nil {
		applyAuthHeader(headers, flavor, key)
	}

	client, err := newHTTPClient(clientOptions{
		provider: name, baseURL: baseURL, headers: headers,
		timeouts: mergeTimeouts(def.timeouts, cfg.Timeouts), retry: cfg.Retry,
		transport: opts.Transport, log: opts.Log,
	})
	if err != nil {
		return prev, err
	}
	var lastErr error
	for _, cand := range spec.candidates {
		e, err := discoverProvider(ctx, client, resolveDiscoveryPath(client.base.Path, cand.path), cand.parse, prev, now)
		if err == nil && len(e.Models) > 0 {
			return e, nil
		}
		// an unreachable server fails every endpoint; do not double the retry ladder
		if ctx.Err() != nil || (err != nil && serverDown(err)) {
			return prev, err
		}
		if lastErr == nil {
			lastErr = err // remember the first failure to report if none succeed
		}
	}
	if lastErr != nil {
		return prev, lastErr
	}
	return prev, errors.New("llm: discovery returned no models")
}

// serverDown reports whether a discovery failure means the endpoint is unreachable,
// as opposed to merely unhelpful. Only a transport-level error qualifies, so an HTTP
// or parse failure still falls through to the next candidate.
func serverDown(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return false // the server answered; a sibling endpoint may behave differently
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// discoverProvider performs a conditional GET and returns the refreshed entry.
// A 304 keeps the cached models and only bumps the check time.
func discoverProvider(ctx context.Context, c *httpClient, path string, parse modelParser, prev CacheEntry, now time.Time) (CacheEntry, error) {
	resp, err := c.doConditional(ctx, path, prev.ETag, prev.LastModified)
	if err != nil {
		return prev, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified {
		prev.CheckedAt = now.UnixMilli()
		return prev, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return prev, err
	}
	models, err := parse(body)
	if err != nil {
		return prev, err
	} else if len(models) == 0 {
		return prev, errors.New("llm: discovery returned no models")
	}
	return CacheEntry{
		Source:       c.base.String() + path,
		Models:       models,
		CheckedAt:    now.UnixMilli(),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	}, nil
}

// doConditional issues a GET carrying whatever validators the cache holds.
func (c *httpClient) doConditional(ctx context.Context, path, etag, lastModified string) (*http.Response, error) {
	h := make(map[string]string, 2)
	if etag != "" {
		h["If-None-Match"] = etag
	}
	if lastModified != "" {
		h["If-Modified-Since"] = lastModified
	}
	return c.do(ctx, httpReq{method: http.MethodGet, path: path, headers: h})
}
