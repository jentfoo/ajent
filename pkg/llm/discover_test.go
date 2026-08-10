package llm

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	assert.Equal(t, localTTL, ttlFor(FlavorLMStudio))
	assert.Equal(t, localTTL, ttlFor(FlavorLlamaCpp))
	assert.Equal(t, hostedTTL, ttlFor(FlavorOpenRouter))
	assert.Equal(t, hostedTTL, ttlFor(FlavorAnthropic))
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
