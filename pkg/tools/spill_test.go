package tools

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenSpill(t *testing.T) {
	t.Parallel()

	t.Run("distinct_paths_per_call", func(t *testing.T) {
		dir := t.TempDir()
		suffix := randSuffix
		f1, p1, err := openSpill(dir, "bash", suffix)
		require.NoError(t, err)
		t.Cleanup(func() { _ = f1.Close() })
		f2, p2, err := openSpill(dir, "bash", suffix)
		require.NoError(t, err)
		t.Cleanup(func() { _ = f2.Close() })

		assert.NotEqual(t, p1, p2)
	})

	t.Run("retries_on_collision", func(t *testing.T) {
		dir := t.TempDir()
		const taken = "bash-collide.txt"
		f, err := os.Create(filepath.Join(dir, taken))
		require.NoError(t, err)
		t.Cleanup(func() { _ = f.Close() })

		calls := 0
		suffix := func() string {
			calls++
			if calls == 1 {
				return "collide"
			}
			return fmt.Sprintf("fresh-%d", calls)
		}
		out, path, err := openSpill(dir, "bash", suffix)
		require.NoError(t, err)
		t.Cleanup(func() { _ = out.Close() })

		assert.NotEqual(t, taken, filepath.Base(path))
	})

	t.Run("gives_up_after_exhaustion", func(t *testing.T) {
		dir := t.TempDir()
		f, err := os.Create(filepath.Join(dir, "bash-stuck.txt"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = f.Close() })

		suffix := func() string { return "stuck" }
		out, _, err := openSpill(dir, "bash", suffix)
		require.ErrorIs(t, err, os.ErrExist)
		if out != nil {
			t.Cleanup(func() { _ = out.Close() })
		}
	})
}

func TestFallbackSuffix(t *testing.T) {
	t.Parallel()

	a := fallbackSuffix()
	b := fallbackSuffix()
	assert.NotEqual(t, a, b)

	wantPrefix := strconv.Itoa(os.Getpid()) + "-"
	assert.True(t, strings.HasPrefix(a, wantPrefix))
}

func TestRandSuffixFormat(t *testing.T) {
	t.Parallel()

	s := randSuffix()
	// fallback carries a "-" separator and is covered by TestFallbackSuffix
	if strings.Contains(s, "-") {
		return
	}
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	assert.Len(t, b, 4)
}
