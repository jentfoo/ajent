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

	_, ok := ReadHead(t.TempDir())
	assert.False(t, ok, "no HEAD file is not a cursor")

	dir := t.TempDir()
	p := filepath.Join(dir, "HEAD")
	require.NoError(t, os.WriteFile(p, []byte("not json"), 0o600))
	_, ok = ReadHead(dir)
	assert.False(t, ok, "garbage HEAD falls back to tail recovery")

	empty := t.TempDir()
	p2 := filepath.Join(empty, "HEAD")
	require.NoError(t, os.WriteFile(p2, []byte(`{"file":"","id":""}`), 0o600))
	_, ok = ReadHead(empty)
	assert.False(t, ok, "an empty cursor is not valid")
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
	_ = []Entry{{ID: "x", Type: TypeSession}}
	e2 := []Entry{{ID: "y", Type: TypeSession}}

	require.NoError(t, WriteHead(p1, "x"))
	assert.Equal(t, Head(e2), headFor(p2, e2),
		"a cursor for another session must not steer this one")
}
