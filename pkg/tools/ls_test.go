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

	res, _ := (&lsTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`{}`)), nil)
	assert.False(t, res.IsError)
	out := textOf(res)
	lines := strings.Split(out, "\n")
	assert.Equal(t, []string{"a.txt", "b.txt", "sub/"}, lines) // sorted, dirs suffixed
}

func TestLsIncludesDotfiles(t *testing.T) {
	t.Parallel()

	dir, policy := newSearchEnv(t)
	mkfile(dir, ".hidden", "x")
	mkfile(dir, "visible.txt", "y")

	res, _ := (&lsTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`{}`)), nil)
	assert.Contains(t, textOf(res), ".hidden")
}

func TestLsLimitTruncatesWithMarker(t *testing.T) {
	t.Parallel()

	dir, policy := newSearchEnv(t)
	for i := 0; i < 5; i++ {
		mkfile(dir, "f"+string(rune('a'+i))+".txt", "")
	}

	res, _ := (&lsTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`{"limit":2}`)), nil)
	assert.False(t, res.IsError)
	out := textOf(res)
	assert.Equal(t, 2, strings.Count(out, ".txt"))
	assert.Contains(t, out, "3 more entries") // truncation is named, not silent
}

func TestLsMissingDirIsError(t *testing.T) {
	t.Parallel()

	_, policy := newSearchEnv(t)
	res, _ := (&lsTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`{"path":"nope"}`)), nil)
	assert.True(t, res.IsError)
}

func TestLsMalformedArgsIsError(t *testing.T) {
	t.Parallel()

	_, policy := newSearchEnv(t)
	res, _ := (&lsTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`not json`)), nil)
	assert.True(t, res.IsError)
}

func TestLsRegisteredDisabledInBuiltins(t *testing.T) {
	t.Parallel()

	reg, err := Builtins(Options{Cwd: t.TempDir(), SessionID: "test"})
	require.NoError(t, err)
	assert.NotContains(t, reg.Names(), "ls") // off by default like find and grep
	assert.NotContains(t, reg.Names(), "find")
	assert.NotContains(t, reg.Names(), "grep")

	reg.SetEnabled([]string{"read", "ls"})
	assert.Contains(t, reg.Names(), "ls")
}
