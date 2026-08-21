package tools

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLsListsEntriesSortedWithDirSuffix(t *testing.T) {
	t.Parallel()

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
}

func TestLsIncludesDotfiles(t *testing.T) {
	t.Parallel()

	dir, policy := newSearchEnv(t)
	mkfile(dir, ".hidden", "x")
	mkfile(dir, "visible.txt", "y")

	res, err := (&lsTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`{}`)), nil)
	require.NoError(t, err)
	assert.Contains(t, textOf(res), ".hidden")
}

func TestLsLimitTruncatesWithMarker(t *testing.T) {
	t.Parallel()

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
}

func TestLsWildcardListsMatchingFilesSorted(t *testing.T) {
	t.Parallel()

	dir, policy := newSearchEnv(t)
	mkfile(dir, "b.md", "x")
	mkfile(dir, "a.md", "y")
	mkfile(dir, "skip.txt", "z")

	res, err := (&lsTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`{"path":"*.md"}`)), nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"a.md", "b.md"}, strings.Split(textOf(res), "\n"))
}

func TestLsWildcardNoMatchIsError(t *testing.T) {
	t.Parallel()

	_, policy := newSearchEnv(t)
	res, err := (&lsTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`{"path":"*.rs"}`)), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
}

func TestLsWildcardLimitTruncates(t *testing.T) {
	t.Parallel()

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
}

func TestLsMissingDirIsError(t *testing.T) {
	t.Parallel()

	_, policy := newSearchEnv(t)
	res, err := (&lsTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`{"path":"nope"}`)), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
}

func TestLsMalformedArgsIsError(t *testing.T) {
	t.Parallel()

	_, policy := newSearchEnv(t)
	res, err := (&lsTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`not json`)), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
}
