package llm

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestRegistryRefresh(t *testing.T) {
	t.Parallel()

	t.Run("adds_discovered_models", func(t *testing.T) {
		var hits int
		srv := discoveryServer(t, "/models", "openrouter/models.json", &hits)
		f := File{Providers: map[string]ProviderConfig{"openrouter": {BaseURL: srv.URL}}}

		r, _ := NewRegistry(f, nil, RegistryOptions{Env: func(string) string { return "" }})
		assert.Empty(t, r.Models())

		cache, warnings := r.Refresh(t.Context(), DiscoverOptions{
			Env: func(string) string { return "" },
			Now: func() time.Time { return testNow },
		})
		assert.Empty(t, warnings)
		assert.Len(t, r.Models(), 2)
		assert.Len(t, cache["openrouter"].Models, 2)
	})
	t.Run("keeps_the_active_model", func(t *testing.T) {
		var hits int
		srv := discoveryServer(t, "/models", "openrouter/models.json", &hits)
		f := File{Providers: map[string]ProviderConfig{
			"openrouter": {BaseURL: srv.URL, Models: []ModelConfig{{ID: "z-ai/glm-5.2"}}},
		}}

		r, _ := NewRegistry(f, nil, RegistryOptions{Env: func(string) string { return "" }})
		before := r.Active().Key()
		require.NotEmpty(t, before)

		_, _ = r.Refresh(t.Context(), DiscoverOptions{
			Env: func(string) string { return "" },
			Now: func() time.Time { return testNow },
		})
		assert.Equal(t, before, r.Active().Key())
	})
	t.Run("declared_models_survive_a_refresh", func(t *testing.T) {
		var hits int
		srv := discoveryServer(t, "/models", "openrouter/models.json", &hits)
		f := File{Providers: map[string]ProviderConfig{
			"openrouter": {BaseURL: srv.URL, Models: []ModelConfig{{ID: "z-ai/glm-5.2"}}},
		}}

		r, _ := NewRegistry(f, nil, RegistryOptions{Env: func(string) string { return "" }})
		_, _ = r.Refresh(t.Context(), DiscoverOptions{
			Env: func(string) string { return "" },
			Now: func() time.Time { return testNow },
		})

		// the declared list stays the whole list, but gains what discovery knows
		require.Len(t, r.Models(), 1)
		assert.Equal(t, 800000, r.Models()[0].ContextWindow)
	})
}
