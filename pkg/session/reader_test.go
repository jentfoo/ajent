package session

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeLines(t *testing.T, p string, lines []string) {
	t.Helper()
	f, err := os.Create(p)
	require.NoError(t, err)
	for _, l := range lines {
		_, err = f.WriteString(l + "\n")
		require.NoError(t, err)
	}
	require.NoError(t, f.Close())
}

func TestReadToleratesGarbageAndTruncation(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	writeLines(t, p, []string{
		`{"id":"a","type":"session","ts":1,"data":{"version":1}}`,
		"garbage line not json", // middle garbage becomes a warning
		``,
		`{"id":"b","parentId":"a","type":"notice","ts":2,"data":{"message":"m"}}`,
	})

	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, _ = f.WriteString(`{"id":"c","parentId":"b","type":"notice","ts":3,"data":{"message":"partial"}}`)
	require.NoError(t, f.Close()) // trailing line without newline: partial write

	entries, warns, rerr := Read(p)
	require.NoError(t, rerr)
	assert.Len(t, entries, 2) // garbage + blank skipped
	assert.NotEmpty(t, warns) // the garbage line warned
	assert.Equal(t, []string{"a", "b"}, ids(entries))
}

func TestReadRejectsNewerVersion(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	writeLines(t, p, []string{`{"id":"a","type":"session","ts":1,"data":{"version":99}}`})

	_, _, err := Read(p)
	assert.ErrorContains(t, err, "newer than supported v1")
}

func TestReadKeepsUnknownEntryType(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	writeLines(t, p, []string{
		`{"id":"a","type":"session","ts":1,"data":{"version":1}}`,
		`{"id":"b","parentId":"a","type":"future_thing","ts":2,"data":{"x":1}}`,
	})

	entries, warns, err := Read(p)
	require.NoError(t, err)
	assert.Empty(t, warns)
	assert.Len(t, entries, 2)
	assert.Equal(t, Type("future_thing"), entries[1].Type)
}

func TestBranchFollowsHeadIgnoresSibling(t *testing.T) {
	t.Parallel()

	entries := []Entry{
		{ID: "root", Type: TypeSession},
		{ID: "a", ParentID: "root", Type: TypeMessage, Data: msgData("m1")},
		{ID: "b", ParentID: "a", Type: TypeMessage, Data: msgData("m2")}, // head
	}
	assert.Equal(t, []string{"root", "a", "b"}, ids(Branch(entries, "b")))

	forked := slices.Clone(entries)
	forked = append(forked, Entry{ID: "c", ParentID: "a", Type: TypeNotice, Data: noticeData("sibling")})
	// Branch from b still follows root->a->b and ignores the fork c.
	assert.Equal(t, []string{"root", "a", "b"}, ids(Branch(forked, "b")))
}

func TestHeadReturnsLastID(t *testing.T) {
	t.Parallel()

	var entries []Entry
	assert.Empty(t, Head(entries))
	entries = append([]Entry{}, Entry{ID: "x"}, Entry{ID: "y"})
	assert.Equal(t, "y", Head(entries))
}
