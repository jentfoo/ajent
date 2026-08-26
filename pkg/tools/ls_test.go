package tools

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLs(t *testing.T) {
	t.Parallel()

	// entries are sorted with directories suffixed.
	t.Run("lists_entries_sorted_with_dir_suffix", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "b.txt", "x")
		mkfile(dir, "a.txt", "y")
		mkfile(dir, "sub/inner.txt", "z")

		res, err := (&lsTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{}`)), nil)
		require.NoError(t, err)
		out := textOf(res)
		lines := strings.Split(out, "\n")
		assert.Equal(t, []string{"a.txt", "b.txt", "sub/"}, lines) // sorted, dirs suffixed
	})

	t.Run("includes_dotfiles", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, ".hidden", "x")
		mkfile(dir, "visible.txt", "y")

		res, err := (&lsTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{}`)), nil)
		require.NoError(t, err)
		assert.Contains(t, textOf(res), ".hidden")
	})

	// a limit truncates with an explicit marker.
	t.Run("limit_truncates_with_marker", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		for i := 0; i < 5; i++ {
			mkfile(dir, "f"+string(rune('a'+i))+".txt", "")
		}

		res, err := (&lsTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"limit":2}`)), nil)
		require.NoError(t, err)
		out := textOf(res)
		assert.Equal(t, 2, strings.Count(out, ".txt"))
		assert.Contains(t, out, "3 more entries") // truncation is named, not silent
	})

	// a wildcard lists matching files sorted.
	t.Run("wildcard_lists_matching_files_sorted", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "b.md", "x")
		mkfile(dir, "a.md", "y")
		mkfile(dir, "skip.txt", "z")

		res, err := (&lsTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"path":"*.md"}`)), nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"a.md", "b.md"}, strings.Split(textOf(res), "\n"))
	})

	t.Run("wildcard_no_match_is_error", func(t *testing.T) {
		_, policy := newSearchEnv(t)
		res, err := (&lsTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"path":"*.rs"}`)), nil)
		require.NoError(t, err)
		assert.True(t, res.IsError)
	})

	t.Run("wildcard_limit_truncates", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		for i := 0; i < 5; i++ {
			mkfile(dir, "f"+string(rune('a'+i))+".txt", "")
		}

		res, err := (&lsTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"path":"*.txt","limit":2}`)), nil)
		require.NoError(t, err)
		out := textOf(res)
		assert.Equal(t, 2, strings.Count(out, ".txt"))
		assert.Contains(t, out, "more files matched") // truncation is named, not silent
	})

	t.Run("missing_dir_is_error", func(t *testing.T) {
		_, policy := newSearchEnv(t)
		res, err := (&lsTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"path":"nope"}`)), nil)
		require.NoError(t, err)
		assert.True(t, res.IsError)
	})

	t.Run("malformed_args_is_error", func(t *testing.T) {
		_, policy := newSearchEnv(t)
		res, err := (&lsTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`not json`)), nil)
		require.NoError(t, err)
		assert.True(t, res.IsError)
	})
}
