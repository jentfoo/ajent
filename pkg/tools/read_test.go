package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadHappyPath(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "line one\nline two\n")
	res := e.readExec(t.Context(), `{"path":"a.txt"}`)
	assert.False(t, res.IsError)
	require.Len(t, res.Content, 1)
	out := textOf(res)
	assert.Contains(t, out, "     1\tline one") // line-numbered output
	assert.Contains(t, out, "     2\tline two")
	// display reports counts of what was read in without exposing the content.
	// numberLines emits a trailing empty row for the final newline, so 3 rows
	// whose capped text totals 16 runes (the blank adds none).
	assert.Equal(t, "read a.txt (3 lines, 16 chars)", res.Display)
}

func TestReadDisplayCountsHonorOffsetAndLimit(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	e.writeFile("big.txt", b.String()) // each line: "line N" = 6 runes + newline

	res := e.readExec(t.Context(), `{"path":"big.txt","offset":2,"limit":3}`) // lines 2-4
	assert.False(t, res.IsError)
	out := textOf(res)
	assert.Contains(t, out, "     2\tline 2")
	assert.NotContains(t, out, "line 5") // limit respected
	// only the read-in slice is counted: lines 2..4 = 3 lines, each "line N" (6 runes)
	assert.Equal(t, "read big.txt (3 lines, 18 chars)", res.Display)
}

func TestReadMissingFileIsErrorResult(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	res := e.readExec(t.Context(), `{"path":"nope.txt"}`)
	assert.True(t, res.IsError)
	assert.Contains(t, textOf(res), "no such file")
}

func TestReadMalformedArgsIsErrorResult(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	res := e.readExec(t.Context(), `not json`)
	assert.True(t, res.IsError) // a bad accumulator must degrade, not panic
}

func TestReadBinaryRefused(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	require.NoError(t, os.WriteFile(filepath.Join(e.cwd, "bin.dat"), []byte{1, 0, 2, 3}, 0o644))
	res := e.readExec(t.Context(), `{"path":"bin.dat"}`)
	assert.True(t, res.IsError)
	assert.Contains(t, textOf(res), "binary")
}

func TestReadTruncationMarkerNamesNextOffset(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	var b strings.Builder
	for i := 1; i <= 3000; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	e.writeFile("big.txt", b.String())

	res := e.readExec(t.Context(), `{"path":"big.txt","limit":200}`)
	assert.False(t, res.IsError)
	out := textOf(res)
	assert.Contains(t, out, "... truncated at line 200, read again with offset=201")
}

func TestReadObservesTrackerForEditStaleCheck(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "hello\nworld\n")
	assert.False(t, e.readExec(t.Context(), `{"path":"a.txt"}`).IsError)
	_, ok := e.tracker.Records()[filepath.Join(e.cwd, "a.txt")]
	assert.True(t, ok) // read records the file so edit/write can check staleness
}
