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
		require.NoError(t, writeHead(p, "abc123"))

		id, ok := readHead(p)
		require.True(t, ok)
		assert.Equal(t, "abc123", id)
	})

	t.Run("missing_and_corrupt_fallback", func(t *testing.T) {
		cases := []struct {
			name    string
			content string // written to the sidecar; unset leaves it absent
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
		require.NoError(t, writeHead(p, "first"))
		require.NoError(t, writeHead(p, "second"))

		id, ok := readHead(p)
		require.True(t, ok)
		assert.Equal(t, "second", id)
	})

	// each transcript owns its own cursor, so a sibling's never leaks into it.
	t.Run("sidecars_are_independent", func(t *testing.T) {
		dir := t.TempDir()
		p1 := filepath.Join(dir, "one.jsonl")
		p2 := filepath.Join(dir, "two.jsonl")
		require.NoError(t, writeHead(p1, "x"))
		require.NoError(t, writeHead(p2, "y"))

		id1, ok := readHead(p1)
		require.True(t, ok)
		assert.Equal(t, "x", id1)
		id2, ok := readHead(p2)
		require.True(t, ok)
		assert.Equal(t, "y", id2)
	})
}

func TestRemoveHead(t *testing.T) {
	t.Parallel()

	t.Run("drops_cursor", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		require.NoError(t, writeHead(p, "abc"))
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

	entries := []Entry{
		{ID: "root", Type: TypeSession},
		{ID: "a", ParentID: "root", Type: TypeMessage, Data: msgData("m1")},
		{ID: "b", ParentID: "a", Type: TypeMessage, Data: msgData("m2")}, // tail
	}

	t.Run("prefers_persisted_over_tail", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		assert.Equal(t, "b", headFor(p, entries))

		require.NoError(t, writeHead(p, "a"))
		assert.Equal(t, "a", headFor(p, entries))
	})

	// a cursor naming an entry this file no longer holds degrades to the tail.
	t.Run("unresolvable_id_falls_back", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		require.NoError(t, writeHead(p, "gone"))
		assert.Equal(t, "b", headFor(p, entries))
	})

	// a sibling session's cursor must not steer this one.
	t.Run("ignores_sibling_cursor", func(t *testing.T) {
		dir := t.TempDir()
		p1 := filepath.Join(dir, "one.jsonl")
		p2 := filepath.Join(dir, "two.jsonl")
		e2 := []Entry{{ID: "y", Type: TypeSession}}

		require.NoError(t, writeHead(p1, "a"))
		assert.Equal(t, "y", headFor(p2, e2))
	})
}
