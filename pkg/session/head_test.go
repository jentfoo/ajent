package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteReadHeadRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	require.NoError(t, WriteHead(p, "abc123"))

	cur, ok := ReadHead(dir)
	require.True(t, ok)
	assert.Equal(t, HeadCursor{File: "s.jsonl", ID: "abc123"}, cur)
}

func TestReadHeadMissingAndCorruptFallback(t *testing.T) {
	t.Parallel()

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
}

func TestWriteHeadOverwritesPrevious(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	require.NoError(t, WriteHead(p, "first"))
	require.NoError(t, WriteHead(p, "second"))

	cur, ok := ReadHead(dir)
	require.True(t, ok)
	assert.Equal(t, "second", cur.ID)
}

func TestHeadForPrefersPersistedOverTail(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	entries := []Entry{
		{ID: "root", Type: TypeSession},
		{ID: "a", ParentID: "root", Type: TypeMessage, Data: msgData("m1")},
		{ID: "b", ParentID: "a", Type: TypeMessage, Data: msgData("m2")}, // tail
	}
	assert.Equal(t, "b", headFor(p, entries), "no cursor falls back to the file tail")

	require.NoError(t, WriteHead(p, "a"))
	assert.Equal(t, "a", headFor(p, entries), "the persisted branch wins over tail")
}

func TestHeadForIgnoresCursorPointingElsewhere(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	p1 := filepath.Join(dir, "one.jsonl")
	p2 := filepath.Join(dir, "two.jsonl")
	e2 := []Entry{{ID: "y", Type: TypeSession}}

	require.NoError(t, WriteHead(p1, "x"))
	assert.Equal(t, Head(e2), headFor(p2, e2),
		"a cursor for another session must not steer this one")
}
