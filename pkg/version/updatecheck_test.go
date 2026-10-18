package version

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testGlobals serializes the subtests that mutate package globals, so a future
// parallel parent cannot interleave an override's window with another reader.
var testGlobals sync.Mutex

// setVersionForTest overrides Version for a subtest and restores it on cleanup.
func setVersionForTest(t *testing.T, v string) {
	t.Helper()

	testGlobals.Lock()
	t.Cleanup(testGlobals.Unlock) // unlock runs after the restore below
	prev := Version
	Version = v
	t.Cleanup(func() { Version = prev })
}

func TestCheckUpdateNotice(t *testing.T) {
	t.Parallel()

	// fixed clock, no-op fetch used when the cached entry is still fresh
	now := time.Unix(1_000_000, 0)
	noFetch := func(context.Context) (string, error) { return "", errors.New("unexpected fetch") }
	optsAt := func(fn func(context.Context) (string, error), at time.Time) UpdateCheckOptions {
		return UpdateCheckOptions{Now: func() time.Time { return at }, Fetch: fn}
	}

	t.Run("no_update_when_current", func(t *testing.T) {
		setVersionForTest(t, "v0.1.5")
		path := filepath.Join(t.TempDir(), "remote.json")
		require.NoError(t, saveUpdateCache(path, UpdateCache{Version: "v0.1.5", CheckedAt: now.Unix()}))
		msg, err := CheckUpdateNotice(t.Context(), path, optsAt(noFetch, now))
		require.NoError(t, err)
		assert.Empty(t, msg)
	})

	t.Run("notices_when_older", func(t *testing.T) {
		setVersionForTest(t, "v0.1.4")
		path := filepath.Join(t.TempDir(), "remote.json")
		require.NoError(t, saveUpdateCache(path, UpdateCache{Version: "v0.1.5", CheckedAt: now.Unix()}))
		want := fmt.Sprintf("update available: ajent %s → v0.1.5 (run /update or --update)", Version)
		msg, err := CheckUpdateNotice(t.Context(), path, optsAt(noFetch, now))
		require.NoError(t, err)
		assert.Equal(t, want, msg)
	})

	t.Run("dev_never_notices", func(t *testing.T) {
		setVersionForTest(t, "dev")
		path := filepath.Join(t.TempDir(), "remote.json")
		require.NoError(t, saveUpdateCache(path, UpdateCache{Version: "v0.1.5", CheckedAt: now.Unix()}))
		msg, err := CheckUpdateNotice(t.Context(), path, optsAt(noFetch, now))
		require.NoError(t, err)
		assert.Empty(t, msg)
	})

	t.Run("respects_notice_interval", func(t *testing.T) {
		setVersionForTest(t, "v0.1.4")
		path := filepath.Join(t.TempDir(), "remote.json")
		require.NoError(t, saveUpdateCache(path, UpdateCache{
			Version: "v0.1.5", CheckedAt: now.Unix(),
			NoticedAt: now.Add(-time.Hour).Unix(), // within 2h
		}))
		msg, err := CheckUpdateNotice(t.Context(), path, optsAt(noFetch, now))
		require.NoError(t, err)
		assert.Empty(t, msg) // already noticed recently
	})

	t.Run("refetches_when_stale", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "remote.json")
		stale := now.Add(-(remoteVersionTTL + time.Hour))
		require.NoError(t, saveUpdateCache(path, UpdateCache{Version: "v0.1.0", CheckedAt: stale.Unix()}))
		var fetched bool
		fn := func(context.Context) (string, error) { fetched = true; return "v9.9.9", nil }
		setVersionForTest(t, "v0.1.5")
		msg, err := CheckUpdateNotice(t.Context(), path, optsAt(fn, now))
		require.NoError(t, err)
		assert.True(t, fetched)
		assert.Contains(t, msg, "v9.9.9")
		var c UpdateCache
		loadUpdateCache(path, &c)
		assert.Equal(t, "v9.9.9", c.Version)
	})

	t.Run("offline_still_notices_known", func(t *testing.T) {
		setVersionForTest(t, "v0.1.4")
		path := filepath.Join(t.TempDir(), "remote.json")
		stale := now.Add(-(remoteVersionTTL + time.Hour))
		cached := "v0.2.0"
		require.NoError(t, saveUpdateCache(path, UpdateCache{Version: cached, CheckedAt: stale.Unix()}))
		fn := func(context.Context) (string, error) { return "", errors.New("offline") }
		want := fmt.Sprintf("update available: ajent %s → %s (run /update or --update)", Version, cached)
		msg, err := CheckUpdateNotice(t.Context(), path, optsAt(fn, now))
		require.NoError(t, err)
		assert.Equal(t, want, msg)

		// the stale tag is kept, a later run retries the fetch
		var c UpdateCache
		loadUpdateCache(path, &c)
		assert.Equal(t, cached, c.Version)
		assert.Equal(t, stale.Unix(), c.CheckedAt)
	})

	t.Run("fetch_error_no_cache_returns_err", func(t *testing.T) {
		setVersionForTest(t, "v0.1.4")
		path := filepath.Join(t.TempDir(), "remote.json")
		stale := now.Add(-(remoteVersionTTL + time.Hour))
		require.NoError(t, saveUpdateCache(path, UpdateCache{CheckedAt: stale.Unix()}))
		fn := func(context.Context) (string, error) { return "", errors.New("offline") }
		_, err := CheckUpdateNotice(t.Context(), path, optsAt(fn, now))
		require.Error(t, err)
	})
}

func TestParseVersion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want semver
		ok   bool
	}{
		{"v1.2.3", semver{1, 2, 3}, true},
		{"v0.10.20", semver{0, 10, 20}, true},
		{"dev", semver{}, false},
		{"v1.2", semver{}, false},
		{"v1.2.3-rc1", semver{}, false},
		{" v1.2.3 ", semver{1, 2, 3}, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			v, ok := parseVersion(tc.in)
			assert.Equal(t, tc.ok, ok)
			if ok {
				assert.Equal(t, tc.want, v)
			}
		})
	}
}

func TestLoadUpdateCacheMissingFile(t *testing.T) {
	t.Parallel()

	var c UpdateCache
	loadUpdateCache(filepath.Join(t.TempDir(), "absent.json"), &c)
	assert.Empty(t, c.Version)

	data := []byte(`{"version":"v0.1.5","checkedAtUnix":100,"noticedAtUnix":200}`)
	require.NoError(t, os.WriteFile(filepath.Join(t.TempDir(), "x.json"), data, 0o600))
	var got UpdateCache
	loadUpdateCache("", &got) // ignored path: no-op
	assert.Empty(t, got.Version)
}

func TestFetchLatestVersion(t *testing.T) {
	t.Parallel()

	// swapURL points the fetch at srv for one subtest
	swapURL := func(t *testing.T, url string) {
		t.Helper()

		testGlobals.Lock()
		t.Cleanup(testGlobals.Unlock) // unlock runs after the restore below
		prev := githubTagsURL
		githubTagsURL = url
		t.Cleanup(func() { githubTagsURL = prev })
	}

	t.Run("returns_the_highest_clean_tag", func(t *testing.T) {
		var ua string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ua = r.Header.Get("User-Agent")
			_, _ = io.WriteString(w, `[{"name":"v0.2.0"},{"name":"v1.4.9"},{"name":"v1.4.10-rc1"},{"name":"nightly"}]`)
		}))
		t.Cleanup(srv.Close)
		swapURL(t, srv.URL)

		got, err := fetchLatestVersion(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "v1.4.9", got)
		assert.Equal(t, UserAgent(), ua)
	})

	t.Run("http_error_is_reported", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(srv.Close)
		swapURL(t, srv.URL)

		_, err := fetchLatestVersion(t.Context())
		require.Error(t, err)
	})

	t.Run("no_clean_tags_is_an_error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `[{"name":"nightly"}]`)
		}))
		t.Cleanup(srv.Close)
		swapURL(t, srv.URL)

		_, err := fetchLatestVersion(t.Context())
		require.Error(t, err)
	})

	t.Run("follows_paginated_pages", func(t *testing.T) {
		page1 := make([]ghTag, 0, tagsPerPage)
		for i := range tagsPerPage {
			page1 = append(page1, ghTag{Name: fmt.Sprintf("v1.0.%d", i)})
		}
		var pages []string // per_page/page from each request
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			pages = append(pages, q.Get("per_page")+"/"+q.Get("page"))
			if q.Get("page") == "1" {
				b, _ := json.Marshal(page1)
				_, _ = w.Write(b)
				return
			}
			_, _ = io.WriteString(w, `[{"name":"v9.9.9"},{"name":"nightly"}]`)
		}))
		t.Cleanup(srv.Close)
		swapURL(t, srv.URL)

		got, err := fetchLatestVersion(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "v9.9.9", got) // only reachable past page 1
		assert.Equal(t, []string{"100/1", "100/2"}, pages)
	})

	t.Run("stops_at_page_cap", func(t *testing.T) {
		var reqs int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reqs++
			tags := make([]ghTag, 0, tagsPerPage+1)
			for i := range tagsPerPage {
				tags = append(tags, ghTag{Name: fmt.Sprintf("v1.0.%d", i)})
			}
			top := "v2.0.0" // the highest tag within the cap
			if reqs > maxTagPages {
				top = "v9.9.9"
			}
			tags = append(tags, ghTag{Name: top})
			b, _ := json.Marshal(tags)
			_, _ = w.Write(b)
		}))
		t.Cleanup(srv.Close)
		swapURL(t, srv.URL)

		got, err := fetchLatestVersion(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "v2.0.0", got)
		assert.Equal(t, maxTagPages, reqs)
	})

	t.Run("empty_page_terminates", func(t *testing.T) {
		page1 := make([]ghTag, tagsPerPage)
		for i := range page1 {
			page1[i] = ghTag{Name: fmt.Sprintf("v3.%d.0", i)}
		}
		var reqs int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reqs++
			if reqs == 1 {
				b, _ := json.Marshal(page1)
				_, _ = w.Write(b)
				return
			}
			_, _ = io.WriteString(w, `[]`)
		}))
		t.Cleanup(srv.Close)
		swapURL(t, srv.URL)

		got, err := fetchLatestVersion(t.Context())
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("v3.%d.0", tagsPerPage-1), got)
		assert.Equal(t, 2, reqs) // an exact-multiple last page costs one empty fetch
	})
}
