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
	_ = os.WriteFile(filepath.Join(e.cwd, "bin.dat"), []byte{1, 0, 2, 3}, 0o644)
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

func TestWriteNewFileCreatesParentsAndDiffs(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	res := e.writeExec(t.Context(), `{"path":"sub/out.txt","content":"hi\n"}`)
	assert.False(t, res.IsError)
	data, err := os.ReadFile(filepath.Join(e.cwd, "sub", "out.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hi\n", string(data))
}

func TestWriteRefusesUnreadOverwrite(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "original")
	res := e.writeExec(t.Context(), `{"path":"a.txt","content":"changed"}`)
	assert.True(t, res.IsError) // not read this session
	data, _ := os.ReadFile(filepath.Join(e.cwd, "a.txt"))
	assert.Equal(t, "original", string(data)) // untouched
}

func TestWriteAllowsAfterRead(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "old\n")
	_ = e.readExec(t.Context(), `{"path":"a.txt"}`)
	res := e.writeExec(t.Context(), `{"path":"a.txt","content":"new\n"}`)
	assert.False(t, res.IsError)
	data, _ := os.ReadFile(filepath.Join(e.cwd, "a.txt"))
	assert.Equal(t, "new\n", string(data))
}

func TestWriteRefusesStaleOverwriteAfterExternalChange(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "v1")
	_ = e.readExec(t.Context(), `{"path":"a.txt"}`)
	e.writeFile("a.txt", "changed externally") // not through the tracker
	res := e.writeExec(t.Context(), `{"path":"a.txt","content":"mine"}`)
	assert.True(t, res.IsError) // stale: file changed since read
}

func TestEditAppliesSingleMatch(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("code.go", "var x = 1\n")
	_ = e.readExec(t.Context(), `{"path":"code.go"}`)

	res := e.editExec(t.Context(),
		`{"path":"code.go","edits":[{"oldText":"x = 1","newText":"y = 2"}]}`)
	assert.False(t, res.IsError)
	data, _ := os.ReadFile(filepath.Join(e.cwd, "code.go"))
	assert.Equal(t, "var y = 2\n", string(data))
}

func TestEditZeroMatchIsActionable(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "the quick brown fox")
	_ = e.readExec(t.Context(), `{"path":"a.txt"}`)

	res := e.editExec(t.Context(),
		`{"path":"a.txt","edits":[{"oldText":"quick brwn fx","newText":"slow"}]}`)
	assert.True(t, res.IsError)
	out := textOf(res)
	assert.Contains(t, out, "no match") // names the failure
}

func TestEditAmbiguousMatchRequiresReplaceAll(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "aaa bbb aaa\n")
	_ = e.readExec(t.Context(), `{"path":"a.txt"}`)

	res := e.editExec(t.Context(),
		`{"path":"a.txt","edits":[{"oldText":"aaa","newText":"zzz"}]}`)
	assert.True(t, res.IsError) // two occurrences without replace_all
	out := textOf(res)
	assert.Contains(t, out, "2 occurrences")

	res = e.editExec(t.Context(),
		`{"path":"a.txt","edits":[{"oldText":"aaa","newText":"zzz","replace_all":true}]}`)
	assert.False(t, res.IsError)
	data, _ := os.ReadFile(filepath.Join(e.cwd, "a.txt"))
	assert.Equal(t, "zzz bbb zzz\n", string(data))
}

func TestEditMultiAtomicRollbackOnFailure(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	orig := "one\ntwo\nthree\n"
	e.writeFile("a.txt", orig)
	_ = e.readExec(t.Context(), `{"path":"a.txt"}`)

	// second edit's old text is missing; the whole batch must not apply
	res := e.editExec(t.Context(),
		`{"path":"a.txt","edits":[{"oldText":"one","newText":"uno"},{"oldText":"missing","newText":"nope"}]}`)
	assert.True(t, res.IsError)

	data, _ := os.ReadFile(filepath.Join(e.cwd, "a.txt"))
	assert.Equal(t, orig, string(data)) // byte-identical: first edit rolled back
}

func TestEditStaleCheckRefusesAfterExternalChange(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "original\n")
	_ = e.readExec(t.Context(), `{"path":"a.txt"}`)
	e.writeFile("a.txt", "changed externally\n")

	res := e.editExec(t.Context(),
		`{"path":"a.txt","edits":[{"oldText":"original","newText":"mine"}]}`)
	assert.True(t, res.IsError) // must re-read before editing
}

func TestEditEmptyOldTextRejected(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "x\n")
	_ = e.readExec(t.Context(), `{"path":"a.txt"}`)
	res := e.editExec(t.Context(),
		`{"path":"a.txt","edits":[{"oldText":"","newText":"y"}]}`)
	assert.True(t, res.IsError)
	assert.Contains(t, textOf(res), "empty oldText") // named, not silently skipped
}

func TestEditEmptyEditsRejected(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "x\n")
	_ = e.readExec(t.Context(), `{"path":"a.txt"}`)
	res := e.editExec(t.Context(), `{"path":"a.txt","edits":[]}`)
	assert.True(t, res.IsError)
}
