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

		var calls int
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

	// a non-collision open failure (permissions, ENOENT) must carry no path,
	// or callers point footers at a file that was never created
	t.Run("open_error_yields_no_path", func(t *testing.T) {
		dir := t.TempDir()
		f, path, err := openSpill(dir, "bash", func() string { return "a/b" }) // no dir/bash-a/
		require.Error(t, err)
		assert.Empty(t, path)
		if f != nil {
			t.Cleanup(func() { _ = f.Close() })
		}
	})
}

func TestDiscardSpill(t *testing.T) {
	t.Parallel()

	f, path, err := openSpill(t.TempDir(), "bash", randSuffix)
	require.NoError(t, err)

	discardSpill(f, path)

	_, writeErr := f.WriteString("x")
	require.ErrorIs(t, writeErr, os.ErrClosed) // closed too, no fd leak
	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr)) // removed, not left empty
}

func TestSpillerWrite(t *testing.T) {
	t.Parallel()

	t.Run("untouched_claims_none", func(t *testing.T) {
		sp := newSpiller("claim-test", "bash")
		assert.Equal(t, spillNone, sp.claim().state)
	})

	t.Run("success_claims_complete", func(t *testing.T) {
		sp := newSpiller("claim-test", "bash")
		_, err := sp.Write([]byte("kept\n"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = sp.close() })

		c := sp.claim()
		assert.Equal(t, spillComplete, c.state)
		assert.NotEmpty(t, c.path) // the footer may point here
	})

	t.Run("write_failure_drops_partial", func(t *testing.T) {
		sp := newSpiller("claim-test", "bash")
		_, err := sp.Write([]byte("kept\n"))
		require.NoError(t, err)
		path := sp.path
		require.NoError(t, sp.f.Close()) // force the next write to fail

		_, err = sp.Write([]byte("more\n"))
		require.ErrorIs(t, err, os.ErrClosed)

		c := sp.claim()
		assert.Equal(t, spillFailed, c.state)
		assert.Empty(t, c.path) // the failed claim names no file
		_, statErr := os.Stat(path)
		assert.True(t, os.IsNotExist(statErr)) // nothing partial left to point at
	})

	t.Run("failed_write_stays_dead", func(t *testing.T) {
		sp := newSpiller("claim-test", "bash")
		_, err := sp.Write([]byte("kept\n"))
		require.NoError(t, err)
		path := sp.path
		require.NoError(t, sp.f.Close()) // force the next write to fail

		_, err = sp.Write([]byte("more\n"))
		require.ErrorIs(t, err, os.ErrClosed)

		// every later write reports the recorded failure and touches nothing
		_, err = sp.Write([]byte("again\n"))
		require.ErrorIs(t, err, os.ErrClosed)
		c := sp.claim()
		assert.Equal(t, spillFailed, c.state)
		_, statErr := os.Stat(path)
		assert.True(t, os.IsNotExist(statErr))
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
