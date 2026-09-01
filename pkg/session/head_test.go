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

	// a written cursor round-trips through ReadHead.
	t.Run("round_trip", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "s.jsonl")
		require.NoError(t, WriteHead(p, "abc123"))

		cur, ok := ReadHead(dir)
		require.True(t, ok)
		assert.Equal(t, HeadCursor{File: "s.jsonl", ID: "abc123"}, cur)
	})

	t.Run("missing_and_corrupt_fallback", func(t *testing.T) {
		cases := []struct {
			name  string
			setup func(dir string)
		}{
			{"no_head_file", func(string) {}},
			{"garbage_json", func(dir string) {
				p := filepath.Join(dir, "HEAD")
				require.NoError(t, os.WriteFile(p, []byte("not json"), 0o600))
			}},
			{"empty_cursor", func(dir string) {
				p := filepath.Join(dir, "HEAD")
				require.NoError(t, os.WriteFile(p, []byte(`{"file":"","id":""}`), 0o600))
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				dir := t.TempDir()
				tc.setup(dir)
				_, ok := ReadHead(dir)
				assert.False(t, ok)
			})
		}
	})

	// a later write replaces the earlier cursor.
	t.Run("overwrites_previous", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "s.jsonl")
		require.NoError(t, WriteHead(p, "first"))
		require.NoError(t, WriteHead(p, "second"))

		cur, ok := ReadHead(dir)
		require.True(t, ok)
		assert.Equal(t, "second", cur.ID)
	})
}

func TestHeadFor(t *testing.T) {
	t.Parallel()

	// no cursor falls back to the file tail; a persisted branch wins over it.
	t.Run("prefers_persisted_over_tail", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "s.jsonl")
		entries := []Entry{
			{ID: "root", Type: TypeSession},
			{ID: "a", ParentID: "root", Type: TypeMessage, Data: msgData("m1")},
			{ID: "b", ParentID: "a", Type: TypeMessage, Data: msgData("m2")}, // tail
		}
		assert.Equal(t, "b", headFor(p, entries))

		require.NoError(t, WriteHead(p, "a"))
		assert.Equal(t, "a", headFor(p, entries))
	})

	// a cursor for another session must not steer this one.
	t.Run("ignores_cursor_pointing_elsewhere", func(t *testing.T) {
		dir := t.TempDir()
		p1 := filepath.Join(dir, "one.jsonl")
		p2 := filepath.Join(dir, "two.jsonl")
		e2 := []Entry{{ID: "y", Type: TypeSession}}

		require.NoError(t, WriteHead(p1, "x"))
		assert.Equal(t, Head(e2), headFor(p2, e2))
	})
}
