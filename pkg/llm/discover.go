package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"time"

	"github.com/jentfoo/ajent/pkg/config"
)

const (
	// CacheFileName holds discovery results between runs.
	CacheFileName = "models-cache.json"
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

// Cache is the decoded models-cache.json.
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
	path, err := config.UserPath(CacheFileName)
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
	path, err := config.UserPath(CacheFileName)
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

// discoverySpec is how one flavor lists its own models.
type discoverySpec struct {
	path  string
	parse modelParser
}

// discoverySpecs is the set of providers that can be asked what they serve.
// Anything absent is configuration only.
var discoverySpecs = map[Flavor]discoverySpec{
	FlavorOpenRouter: {path: "/models", parse: parseOpenRouterModels},
	FlavorLMStudio:   {path: "/api/v0/models", parse: parseLMStudioModels},
	FlavorLlamaCpp:   {path: "/props", parse: parseLlamaProps},
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
		if !boolOr(cfg.Discover, def.discover) {
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

// discoverOne builds a client for a provider and refreshes its entry.
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
	return discoverProvider(ctx, client, spec.path, spec.parse, prev, now)
}

// boolOr returns the pointer's value, or alt when it is unset.
func boolOr(p *bool, alt bool) bool {
	if p != nil {
		return *p
	}
	return alt
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
