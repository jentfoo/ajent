package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testNow = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

// idParser reads a bare {"data":[{"id":...}]} list, standing in for a real one.
func idParser(body []byte) ([]ModelConfig, error) {
	var wire struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	out := make([]ModelConfig, len(wire.Data))
	for i, d := range wire.Data {
		out[i] = ModelConfig{ID: d.ID}
	}
	return out, nil
}

func TestServerDown(t *testing.T) {
	t.Parallel()

	serverErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"transport_error_is_down", serverErr, true},
		{"api_error_is_not_down", &APIError{Status: 404}, false},
		{"context_cancel_is_not_net_error", context.Canceled, false},
		{"nil_is_not_down", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, serverDown(tc.err))
		})
	}
}

// roundTripperFunc adapts a function to http.RoundTripper for transport injection.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// statusResponse builds an HTTP response whose body carries the given message,
// standing in for a real server that answered.
func statusResponse(status int, msg string, r *http.Request) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(msg)),
		Request:    r,
	}
}

func TestDiscoverProvider(t *testing.T) {
	t.Parallel()

	t.Run("fetches_and_stores_validators", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("ETag", `W/"abc"`)
			w.Header().Set("Last-Modified", "Sat, 09 Aug 2026 11:00:00 GMT")
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
		}))
		t.Cleanup(srv.Close)

		c, _, _ := testClient(t, srv.URL)
		got, err := discoverProvider(t.Context(), c, "/models", idParser, CacheEntry{}, testNow)
		require.NoError(t, err)

		assert.Len(t, got.Models, 2)
		assert.Equal(t, `W/"abc"`, got.ETag)
		assert.Equal(t, "Sat, 09 Aug 2026 11:00:00 GMT", got.LastModified)
		assert.Equal(t, testNow.UnixMilli(), got.CheckedAt)
		assert.Contains(t, got.Source, "/models")
	})
	t.Run("sends_conditional_headers", func(t *testing.T) {
		var gotETag, gotSince string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotETag, gotSince = r.Header.Get("If-None-Match"), r.Header.Get("If-Modified-Since")
			w.WriteHeader(http.StatusNotModified)
		}))
		t.Cleanup(srv.Close)

		prev := CacheEntry{
			Models:       []ModelConfig{{ID: "cached"}},
			ETag:         `W/"abc"`,
			LastModified: "Sat, 09 Aug 2026 11:00:00 GMT",
			CheckedAt:    testNow.Add(-time.Hour).UnixMilli(),
		}
		c, _, _ := testClient(t, srv.URL)
		got, err := discoverProvider(t.Context(), c, "/models", idParser, prev, testNow)
		require.NoError(t, err)

		assert.Equal(t, `W/"abc"`, gotETag)
		assert.Equal(t, "Sat, 09 Aug 2026 11:00:00 GMT", gotSince)
		assert.Equal(t, prev.Models, got.Models) // kept
		assert.Equal(t, testNow.UnixMilli(), got.CheckedAt)
	})
	t.Run("network_failure_keeps_the_stale_entry", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)

		prev := CacheEntry{Models: []ModelConfig{{ID: "cached"}}, CheckedAt: 42}
		c, _, _ := testClient(t, srv.URL)
		got, err := discoverProvider(t.Context(), c, "/models", idParser, prev, testNow)

		require.Error(t, err)
		assert.Equal(t, prev, got) // untouched, including CheckedAt
	})
	t.Run("unparseable_body_keeps_the_stale_entry", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`not json`))
		}))
		t.Cleanup(srv.Close)

		prev := CacheEntry{Models: []ModelConfig{{ID: "cached"}}, CheckedAt: 42}
		c, _, _ := testClient(t, srv.URL)
		got, err := discoverProvider(t.Context(), c, "/models", idParser, prev, testNow)

		require.Error(t, err)
		assert.Equal(t, prev, got)
	})
	t.Run("empty_result_is_an_error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		t.Cleanup(srv.Close)

		prev := CacheEntry{Models: []ModelConfig{{ID: "cached"}}}
		c, _, _ := testClient(t, srv.URL)
		_, err := discoverProvider(t.Context(), c, "/models", idParser, prev, testNow)
		assert.Error(t, err)
	})
	t.Run("parser_error_propagates", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{}`))
		}))
		t.Cleanup(srv.Close)

		want := errors.New("bad shape")
		c, _, _ := testClient(t, srv.URL)
		_, err := discoverProvider(t.Context(), c, "/models",
			func([]byte) ([]ModelConfig, error) { return nil, want }, CacheEntry{}, testNow)
		assert.ErrorIs(t, err, want)
	})
}

func TestFresh(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		entry    CacheEntry
		ttl      time.Duration
		expected bool
	}{
		{"within_ttl", CacheEntry{Models: []ModelConfig{{ID: "m"}},
			CheckedAt: testNow.Add(-time.Minute).UnixMilli()}, time.Hour, true},
		{"past_ttl", CacheEntry{Models: []ModelConfig{{ID: "m"}},
			CheckedAt: testNow.Add(-2 * time.Hour).UnixMilli()}, time.Hour, false},
		{"never_checked", CacheEntry{Models: []ModelConfig{{ID: "m"}}}, time.Hour, false},
		{"no_models", CacheEntry{CheckedAt: testNow.UnixMilli()}, time.Hour, false},
		{"zero_entry", CacheEntry{}, time.Hour, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, fresh(tc.entry, tc.ttl, testNow))
		})
	}
}

func TestTTLFor(t *testing.T) {
	t.Parallel()

	// a local server's loaded model changes far more often than a hosted catalogue
	tests := []struct {
		name     string
		flavor   Flavor
		expected time.Duration
	}{
		{"lmstudio", FlavorLMStudio, localTTL},
		{"llamacpp", FlavorLlamaCpp, localTTL},
		{"openrouter", FlavorOpenRouter, hostedTTL},
		{"anthropic", FlavorAnthropic, hostedTTL},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, ttlFor(tc.flavor))
		})
	}
}

func TestSaveCache(t *testing.T) {
	t.Parallel()

	t.Run("round_trips", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), CacheFileName)
		entries := map[string]CacheEntry{
			"openrouter": {Models: []ModelConfig{{ID: "m1", ContextWindow: ptr(1000)}},
				CheckedAt: testNow.UnixMilli(), ETag: `W/"x"`},
		}
		require.NoError(t, SaveCache(path, entries))

		got := LoadCache(path)
		require.Contains(t, got, "openrouter")
		assert.Equal(t, entries["openrouter"].ETag, got["openrouter"].ETag)
		require.Len(t, got["openrouter"].Models, 1)
		assert.Equal(t, 1000, *got["openrouter"].Models[0].ContextWindow)
	})
	t.Run("written_privately", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), CacheFileName)
		require.NoError(t, SaveCache(path, nil))
		assert.Empty(t, config.CheckSecretPerms(path))
	})
}

func TestLoadCache(t *testing.T) {
	t.Parallel()

	t.Run("missing_file_is_empty", func(t *testing.T) {
		assert.Empty(t, LoadCache(filepath.Join(t.TempDir(), "absent.json")))
	})
	t.Run("corrupt_file_is_empty", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), CacheFileName)
		require.NoError(t, os.WriteFile(path, []byte(`{`), 0o600))
		assert.Empty(t, LoadCache(path))
	})
	t.Run("wrong_version_is_discarded", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), CacheFileName)
		require.NoError(t, os.WriteFile(path,
			[]byte(`{"version":99,"providers":{"a":{"models":[{"id":"m"}]}}}`), 0o600))
		assert.Empty(t, LoadCache(path))
	})
}

// discoveryServer serves a fixture at the given path and counts the requests.
func discoveryServer(t *testing.T, path, fixture string, hits *int) *httptest.Server {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", fixture))
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		*hits++
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDiscover(t *testing.T) {
	t.Parallel()

	opts := func() DiscoverOptions {
		return DiscoverOptions{
			Env: func(string) string { return "" },
			Now: func() time.Time { return testNow },
		}
	}

	t.Run("populates_an_undeclared_provider", func(t *testing.T) {
		var hits int
		srv := discoveryServer(t, "/models", "openrouter/models.json", &hits)
		f := File{Providers: map[string]ProviderConfig{
			"openrouter": {BaseURL: srv.URL},
		}}

		cache, warnings := Discover(t.Context(), f, nil, opts())
		assert.Empty(t, warnings)
		assert.Equal(t, 1, hits)
		require.Contains(t, cache, "openrouter")
		assert.Len(t, cache["openrouter"].Models, 2)
	})
	t.Run("each_flavor_uses_its_own_endpoint", func(t *testing.T) {
		tests := []struct {
			flavor  string
			path    string
			fixture string
			models  int
		}{
			{"openrouter", "/models", "openrouter/models.json", 2},
			{"lmstudio", "/api/v0/models", "lmstudio/models.json", 2},
			{"llamacpp", "/props", "llamacpp/props.json", 1},
		}
		for _, tc := range tests {
			t.Run(tc.flavor, func(t *testing.T) {
				var hits int
				srv := discoveryServer(t, tc.path, tc.fixture, &hits)
				f := File{Providers: map[string]ProviderConfig{
					tc.flavor: {BaseURL: srv.URL},
				}}

				cache, warnings := Discover(t.Context(), f, nil, opts())
				assert.Empty(t, warnings)
				assert.Equal(t, 1, hits)
				assert.Len(t, cache[tc.flavor].Models, tc.models)
			})
		}
	})
	t.Run("generic_provider_discovers_via_openai_list", func(t *testing.T) {
		var hits int
		srv := discoveryServer(t, "/v1/models", "openaimodels/models.json", &hits)
		f := File{Providers: map[string]ProviderConfig{
			"lutra": {BaseURL: srv.URL, Discover: ptr(true)},
		}}

		cache, warnings := Discover(t.Context(), f, nil, opts())
		assert.Empty(t, warnings)
		require.Contains(t, cache, "lutra")
		assert.Len(t, cache["lutra"].Models, 3)
	})
	t.Run("generic_base_ending_in_v1_drops_the_prefix", func(t *testing.T) {
		// a base URL that already carries /v1 must not be asked for /v1/v1/models
		var hits int
		srv := discoveryServer(t, "/v1/models", "openaimodels/models.json", &hits)
		f := File{Providers: map[string]ProviderConfig{
			"lutra": {BaseURL: srv.URL + "/v1", Discover: ptr(true)},
		}}

		cache, warnings := Discover(t.Context(), f, nil, opts())
		assert.Empty(t, warnings)
		require.Contains(t, cache, "lutra")
		// a plain OpenAI server reports no status; every listed model stays available
		assert.Len(t, cache["lutra"].Models, 3)
		assert.Equal(t, 1, hits) // served at /v1/models once, not /v1/v1/models
	})
	t.Run("generic_without_opt_in_is_skipped", func(t *testing.T) {
		f := File{Providers: map[string]ProviderConfig{
			"lutra": {BaseURL: "http://127.0.0.1:1"},
		}}
		cache, warnings := Discover(t.Context(), f, nil, opts())
		assert.Empty(t, cache)
		assert.Empty(t, warnings)
	})
	t.Run("llamacpp_router_falls_back_to_openai_list", func(t *testing.T) {
		var propsHits, modelsHits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/props":
				propsHits++
				data, _ := os.ReadFile(filepath.Join("testdata", "llamacpp", "props_router.json"))
				_, _ = w.Write(data)
			case "/v1/models":
				modelsHits++
				data, _ := os.ReadFile(filepath.Join("testdata", "openaimodels", "models_router.json"))
				_, _ = w.Write(data)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)

		f := File{Providers: map[string]ProviderConfig{
			"llamacpp": {BaseURL: srv.URL},
		}}
		cache, warnings := Discover(t.Context(), f, nil, opts())
		assert.Empty(t, warnings)
		require.Contains(t, cache, "llamacpp")
		// only the loaded model surfaces; unloaded router entries are dropped
		models := cache["llamacpp"].Models
		require.Len(t, models, 1)
		assert.Equal(t, "unsloth/GLM-5.3-Flash-GGUF:Q6_K_XL", models[0].ID)
		require.NotNil(t, models[0].ContextWindow)
		assert.Equal(t, 667392, *models[0].ContextWindow) // meta.n_ctx
		assert.Equal(t, 1, propsHits)                     // consulted first
		assert.Equal(t, 1, modelsHits)                    // then the fallback won
	})
	t.Run("unreachable_server_skips_the_fallback", func(t *testing.T) {
		// a dead server fails every endpoint; only the primary is tried once its own
		// retry ladder is spent, rather than doubling it on /v1/models
		var trips atomic.Int32
		tr := roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
			trips.Add(1)
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		})
		o := opts()
		o.Transport = tr
		f := File{Providers: map[string]ProviderConfig{
			"llamacpp": {BaseURL: "http://127.0.0.1:9"},
		}}

		_, warnings := Discover(t.Context(), f, nil, o)
		require.Len(t, warnings, 1)
		assert.Equal(t, defaultAttempts, int(trips.Load())) // the primary's full ladder, no fallback
	})
	t.Run("http_failure_retries_the_next_candidate", func(t *testing.T) {
		// a reachable server that answers unhelpfully still falls through; the first
		// failure is the one reported when neither candidate yields models
		var trips atomic.Int32
		tr := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			trips.Add(1)
			if strings.HasSuffix(r.URL.Path, "/v1/models") {
				return statusResponse(http.StatusForbidden, "forbidden models", r), nil
			}
			return statusResponse(http.StatusBadRequest, "bad props", r), nil
		})
		o := opts()
		o.Transport = tr
		f := File{Providers: map[string]ProviderConfig{
			"llamacpp": {BaseURL: "http://127.0.0.1:9"},
		}}
		prev := map[string]CacheEntry{"llamacpp": {Models: []ModelConfig{{ID: "cached"}}}}

		cache, warnings := Discover(t.Context(), f, prev, o)
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "bad props")              // the first failure is reported
		assert.Equal(t, 2, int(trips.Load()))                     // both candidates tried once each
		assert.Equal(t, "cached", cache["llamacpp"].Models[0].ID) // stale entry kept
	})
	t.Run("fresh_cache_skips_the_network", func(t *testing.T) {
		var hits int
		srv := discoveryServer(t, "/models", "openrouter/models.json", &hits)
		f := File{Providers: map[string]ProviderConfig{"openrouter": {BaseURL: srv.URL}}}
		prev := map[string]CacheEntry{"openrouter": {
			Models:    []ModelConfig{{ID: "cached"}},
			CheckedAt: testNow.Add(-time.Minute).UnixMilli(),
		}}

		cache, _ := Discover(t.Context(), f, prev, opts())
		assert.Zero(t, hits)
		assert.Equal(t, "cached", cache["openrouter"].Models[0].ID)
	})
	t.Run("force_ignores_the_time_to_live", func(t *testing.T) {
		var hits int
		srv := discoveryServer(t, "/models", "openrouter/models.json", &hits)
		f := File{Providers: map[string]ProviderConfig{"openrouter": {BaseURL: srv.URL}}}
		prev := map[string]CacheEntry{"openrouter": {
			Models:    []ModelConfig{{ID: "cached"}},
			CheckedAt: testNow.UnixMilli(),
		}}

		o := opts()
		o.Force = true
		_, _ = Discover(t.Context(), f, prev, o)
		assert.Equal(t, 1, hits)
	})
	t.Run("local_providers_expire_sooner", func(t *testing.T) {
		// a loaded model changes far more often than a hosted catalogue
		var hits int
		srv := discoveryServer(t, "/props", "llamacpp/props.json", &hits)
		f := File{Providers: map[string]ProviderConfig{"llamacpp": {BaseURL: srv.URL}}}
		prev := map[string]CacheEntry{"llamacpp": {
			Models:    []ModelConfig{{ID: "cached"}},
			CheckedAt: testNow.Add(-5 * time.Minute).UnixMilli(), // fresh for hosted, stale here
		}}

		_, _ = Discover(t.Context(), f, prev, opts())
		assert.Equal(t, 1, hits)
	})
	t.Run("opt_out_is_honoured", func(t *testing.T) {
		var hits int
		srv := discoveryServer(t, "/models", "openrouter/models.json", &hits)
		f := File{Providers: map[string]ProviderConfig{
			"openrouter": {BaseURL: srv.URL, Discover: ptr(false)},
		}}

		_, warnings := Discover(t.Context(), f, nil, opts())
		assert.Zero(t, hits)
		assert.Empty(t, warnings)
	})
	t.Run("providers_without_discovery_are_skipped", func(t *testing.T) {
		f := File{Providers: map[string]ProviderConfig{
			"anthropic": {BaseURL: "http://127.0.0.1:1"},
		}}
		cache, warnings := Discover(t.Context(), f, nil, opts())
		assert.Empty(t, cache)
		assert.Empty(t, warnings)
	})
	t.Run("disabled_provider_is_skipped", func(t *testing.T) {
		var hits int
		srv := discoveryServer(t, "/models", "openrouter/models.json", &hits)
		f := File{Providers: map[string]ProviderConfig{
			"openrouter": {BaseURL: srv.URL, Disabled: true},
		}}

		_, _ = Discover(t.Context(), f, nil, opts())
		assert.Zero(t, hits)
	})
	t.Run("failure_warns_and_keeps_the_stale_entry", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)

		f := File{Providers: map[string]ProviderConfig{
			"openrouter": {BaseURL: srv.URL, Retry: RetryPolicy{Attempts: 1}},
		}}
		prev := map[string]CacheEntry{"openrouter": {Models: []ModelConfig{{ID: "cached"}}}}

		cache, warnings := Discover(t.Context(), f, prev, opts())
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "openrouter")
		assert.Equal(t, "cached", cache["openrouter"].Models[0].ID)
	})
	t.Run("offline_with_no_cache_yields_nothing", func(t *testing.T) {
		f := File{Providers: map[string]ProviderConfig{
			"lmstudio": {BaseURL: "http://127.0.0.1:1", Retry: RetryPolicy{Attempts: 1}},
		}}
		cache, warnings := Discover(t.Context(), f, nil, opts())
		assert.NotEmpty(t, warnings)
		assert.Empty(t, cache["lmstudio"].Models)
	})
	t.Run("source_cache_is_not_mutated", func(t *testing.T) {
		var hits int
		srv := discoveryServer(t, "/models", "openrouter/models.json", &hits)
		f := File{Providers: map[string]ProviderConfig{"openrouter": {BaseURL: srv.URL}}}
		prev := map[string]CacheEntry{"openrouter": {Models: []ModelConfig{{ID: "cached"}}}}

		_, _ = Discover(t.Context(), f, prev, opts())
		assert.Equal(t, "cached", prev["openrouter"].Models[0].ID)
	})
}
