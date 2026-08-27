package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setVersionForTest overrides config.Version for a subtest and restores it on
// cleanup.
func setVersionForTest(t *testing.T, v string) {
	t.Helper()
	prev := Version
	Version = v
	t.Cleanup(func() { Version = prev })
}

func TestCheckUpdateNotice(t *testing.T) {
	t.Parallel()

	// fixed clock; no-op fetch used when the cached entry is still fresh.
	now := time.Unix(1_000_000, 0)
	noFetch := func(context.Context) (string, error) { return "", errors.New("unexpected fetch") }
	optsAt := func(fn func(context.Context) (string, error), at time.Time) UpdateCheckOptions {
		return UpdateCheckOptions{Now: func() time.Time { return at }, Fetch: fn}
	}

	t.Run("no_update_when_current", func(t *testing.T) {
		setVersionForTest(t, "v0.1.5")
		path := filepath.Join(t.TempDir(), "remote.json")
		require.NoError(t, saveUpdateCache(path, UpdateCache{Version: "v0.1.5", CheckedAt: now.Unix()}))
		msg, err := CheckUpdateNotice(context.Background(), path,
			optsAt(noFetch, now))
		require.NoError(t, err)
		assert.Empty(t, msg)
	})

	t.Run("notices_when_older", func(t *testing.T) {
		setVersionForTest(t, "v0.1.4")
		path := filepath.Join(t.TempDir(), "remote.json")
		require.NoError(t, saveUpdateCache(path, UpdateCache{Version: "v0.1.5", CheckedAt: now.Unix()}))
		want := fmt.Sprintf("update available: ajent %s → v0.1.5 (run /update or --update)", Version)
		msg, err := CheckUpdateNotice(context.Background(), path,
			optsAt(noFetch, now))
		require.NoError(t, err)
		assert.Equal(t, want, msg)
	})

	t.Run("dev_never_notices", func(t *testing.T) {
		setVersionForTest(t, "dev")
		path := filepath.Join(t.TempDir(), "remote.json")
		require.NoError(t, saveUpdateCache(path, UpdateCache{Version: "v0.1.5", CheckedAt: now.Unix()}))
		msg, err := CheckUpdateNotice(context.Background(), path,
			optsAt(noFetch, now))
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
		msg, err := CheckUpdateNotice(context.Background(), path,
			optsAt(noFetch, now))
		require.NoError(t, err)
		assert.Empty(t, msg) // already noticed recently
	})

	t.Run("refetches_when_stale", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "remote.json")
		stale := now.Add(-(remoteVersionTTL + time.Hour))
		require.NoError(t, saveUpdateCache(path, UpdateCache{Version: "v0.1.0", CheckedAt: stale.Unix()}))
		fetched := false
		fn := func(context.Context) (string, error) { fetched = true; return "v9.9.9", nil }
		setVersionForTest(t, "v0.1.5")
		msg, err := CheckUpdateNotice(context.Background(), path,
			optsAt(fn, now))
		require.NoError(t, err)
		assert.True(t, fetched)
		assert.Contains(t, msg, "v9.9.9")
		var c UpdateCache
		loadUpdateCache(path, &c)
		assert.Equal(t, "v9.9.9", c.Version)
	})

	t.Run("fetch_error_returns_err", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "remote.json")
		stale := now.Add(-(remoteVersionTTL + time.Hour))
		require.NoError(t, saveUpdateCache(path, UpdateCache{Version: "v0.1.5", CheckedAt: stale.Unix()}))
		fn := func(context.Context) (string, error) { return "", errors.New("offline") }
		setVersionForTest(t, "v0.1.4")
		_, err := CheckUpdateNotice(context.Background(), path,
			optsAt(fn, now))
		require.Error(t, err)
	})
}

func TestParseVersion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want version
		ok   bool
	}{
		{"v1.2.3", version{1, 2, 3}, true},
		{"v0.10.20", version{0, 10, 20}, true},
		{"dev", version{}, false},
		{"v1.2", version{}, false},
		{"v1.2.3-rc1", version{}, false},
		{" v1.2.3 ", version{1, 2, 3}, true},
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
