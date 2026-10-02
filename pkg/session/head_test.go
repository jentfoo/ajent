package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteReadHead(t *testing.T) {
	t.Parallel()

	t.Run("round_trip", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		require.NoError(t, writeHead(p, "abc123", 7))

		cur, ok := readHead(p)
		require.True(t, ok)
		assert.Equal(t, "abc123", cur.ID)
		assert.Equal(t, 7, cur.N)
	})

	t.Run("missing_and_corrupt_fallback", func(t *testing.T) {
		cases := []struct {
			name    string
			content string // written to the sidecar, unset leaves it absent
		}{
			{name: "no_sidecar"},
			{name: "garbage_json", content: "not json"},
			{name: "empty_cursor", content: `{"id":""}`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				p := filepath.Join(t.TempDir(), "s.jsonl")
				if tc.content != "" {
					require.NoError(t, os.WriteFile(headPath(p), []byte(tc.content), 0o600))
				}
				_, ok := readHead(p)
				assert.False(t, ok)
			})
		}
	})

	t.Run("overwrites_previous", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		require.NoError(t, writeHead(p, "first", 1))
		require.NoError(t, writeHead(p, "second", 2))

		cur, ok := readHead(p)
		require.True(t, ok)
		assert.Equal(t, "second", cur.ID)
	})

	// each transcript owns its own cursor, so a sibling's never leaks into it
	t.Run("sidecars_are_independent", func(t *testing.T) {
		dir := t.TempDir()
		p1 := filepath.Join(dir, "one.jsonl")
		p2 := filepath.Join(dir, "two.jsonl")
		require.NoError(t, writeHead(p1, "x", 1))
		require.NoError(t, writeHead(p2, "y", 1))

		cur1, ok := readHead(p1)
		require.True(t, ok)
		assert.Equal(t, "x", cur1.ID)
		cur2, ok := readHead(p2)
		require.True(t, ok)
		assert.Equal(t, "y", cur2.ID)
	})
}

func TestRemoveHead(t *testing.T) {
	t.Parallel()

	t.Run("drops_cursor", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		require.NoError(t, writeHead(p, "abc", 1))
		require.NoError(t, removeHead(p))

		_, ok := readHead(p)
		assert.False(t, ok)
	})

	t.Run("missing_is_not_an_error", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		require.NoError(t, removeHead(p))
		require.NoError(t, removeHead(p))
	})
}

func TestHeadFor(t *testing.T) {
	t.Parallel()

	// root -> a -> b, b is the file tail
	entries := []Entry{
		{ID: "root", Type: TypeSession},
		{ID: "a", ParentID: "root", Type: TypeMessage, Data: msgData("m1")},
		{ID: "b", ParentID: "a", Type: TypeMessage, Data: msgData("m2")},
	}

	t.Run("no_cursor_falls_to_tail", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		assert.Equal(t, "b", headFor(p, entries))
	})

	t.Run("prefers_persisted_over_tail", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		require.NoError(t, writeHead(p, "b", len(entries)))
		assert.Equal(t, "b", headFor(p, entries))

		require.NoError(t, writeHead(p, "a", len(entries)))
		assert.Equal(t, "a", headFor(p, entries))
	})

	// appends past the counted length in one chain are unsynced writes from a
	// crash, so the tail wins over the stale cursor
	t.Run("unsynced_appends_fall_to_tail", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		require.NoError(t, writeHead(p, "a", 2))
		assert.Equal(t, "b", headFor(p, entries))
	})

	// growth past the cursor that breaks the parent chain is a fork, not a crash
	t.Run("forked_growth_keeps_cursor", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		require.NoError(t, writeHead(p, "a", 1)) // entries[1:] start at a, whose parent is root
		assert.Equal(t, "a", headFor(p, entries))
	})

	// a pre-count cursor has nothing to judge growth with, so it is trusted
	t.Run("legacy_cursor_trusted", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		require.NoError(t, writeHead(p, "a", 0))
		assert.Equal(t, "a", headFor(p, entries))
	})

	// a file shorter than the counted length was truncated, keep the cursor
	t.Run("short_file_keeps_cursor", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		require.NoError(t, writeHead(p, "b", len(entries)+2))
		assert.Equal(t, "b", headFor(p, entries))
	})

	// a cursor naming an entry this file no longer holds degrades to the tail
	t.Run("unresolvable_id_falls_back", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		require.NoError(t, writeHead(p, "gone", 5))
		assert.Equal(t, "b", headFor(p, entries))
	})

	// a sibling session's cursor must not steer this one
	t.Run("ignores_sibling_cursor", func(t *testing.T) {
		dir := t.TempDir()
		p1 := filepath.Join(dir, "one.jsonl")
		p2 := filepath.Join(dir, "two.jsonl")
		e2 := []Entry{{ID: "y", Type: TypeSession}}

		require.NoError(t, writeHead(p1, "a", 2))
		assert.Equal(t, "y", headFor(p2, e2))
	})
}
