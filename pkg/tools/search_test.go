package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func newSearchEnv(t *testing.T) (string, PathPolicy) {
	t.Helper()
	dir := t.TempDir()
	return dir, PathPolicy{Cwd: dir}
}

// mkfile writes a file under dir.
func mkfile(dir, name, content string) {
	p := filepath.Join(dir, name)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(content), 0o644)
}

func TestFindMatchesGlob(t *testing.T) {
	t.Parallel()

	dir, policy := newSearchEnv(t)
	mkfile(dir, "a.go", "x")
	mkfile(dir, "b.txt", "y")

	res, _ := (&findTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`{"pattern":"*.go"}`)), nil)
	assert.False(t, res.IsError)
	out := textOf(res)
	assert.Contains(t, out, "a.go")
	assert.NotContains(t, out, "b.txt")
}

func TestFindBarePatternMatchesAtAnyDepth(t *testing.T) {
	t.Parallel()

	dir, policy := newSearchEnv(t)
	mkfile(dir, "top.go", "x")
	mkfile(dir, "pkg/tools/nested.go", "y")

	res, _ := (&findTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`{"pattern":"*.go"}`)), nil)
	assert.False(t, res.IsError)
	out := textOf(res)
	assert.Contains(t, out, "top.go")
	assert.Contains(t, out, filepath.Join("pkg", "tools", "nested.go")) // bare glob reaches any depth
}

func TestFindDoublestarMatchesRootAndNested(t *testing.T) {
	t.Parallel()

	dir, policy := newSearchEnv(t)
	mkfile(dir, "root.go", "x")
	mkfile(dir, "sub/inner.go", "y")
	mkfile(dir, "sub/skip.txt", "z")

	res, _ := (&findTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`{"pattern":"**/*.go"}`)), nil)
	assert.False(t, res.IsError)
	out := textOf(res)
	assert.Contains(t, out, "root.go") // ** spans zero segments too
	assert.Contains(t, out, filepath.Join("sub", "inner.go"))
	assert.NotContains(t, out, "skip.txt")
}

func TestFindLimitParamTruncates(t *testing.T) {
	t.Parallel()

	dir, policy := newSearchEnv(t)
	for i := 0; i < 5; i++ {
		mkfile(dir, "f"+string(rune('a'+i))+".txt", "")
	}
	res, _ := (&findTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`{"pattern":"*.txt","limit":2}`)), nil)
	assert.False(t, res.IsError)
	out := textOf(res)
	assert.Equal(t, 2, strings.Count(out, ".txt"))
	assert.Contains(t, out, "more results") // truncation is named, not silent
}

func TestFindEmptyPatternRejected(t *testing.T) {
	t.Parallel()

	_, policy := newSearchEnv(t)
	res, _ := (&findTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`{"pattern":"  "}`)), nil)
	assert.True(t, res.IsError)
}

func TestFindNoMatchIsEmptyNotError(t *testing.T) {
	t.Parallel()

	dir, policy := newSearchEnv(t)
	mkfile(dir, "a.md", "x")

	res, _ := (&findTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`{"pattern":"*.rs"}`)), nil)
	assert.False(t, res.IsError)
	assert.Empty(t, textOf(res))
}

func TestFindBoundedResults(t *testing.T) {
	t.Parallel()

	dir, policy := newSearchEnv(t)
	for i := 0; i < 20; i++ {
		mkfile(dir, "f"+string(rune('a'+i))+".txt", "")
	}
	res, _ := (&findTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`{"pattern":"*.txt"}`)), nil)
	assert.False(t, res.IsError)
	out := textOf(res)
	assert.LessOrEqual(t, strings.Count(out, ".txt"), FindResultLimit().Lines)
}

func TestFindMalformedArgsIsError(t *testing.T) {
	t.Parallel()

	_, policy := newSearchEnv(t)
	res, _ := (&findTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`not json`)), nil)
	assert.True(t, res.IsError)
}

func TestGrepContentModeFindsLineNumbers(t *testing.T) {
	t.Parallel()

	dir, policy := newSearchEnv(t)
	mkfile(dir, "a.txt", "hello world\nfoo bar\n")

	res, _ := (&grepTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`{"pattern":"world"}`)), nil)
	assert.False(t, res.IsError)
	out := textOf(res)
	assert.Contains(t, out, "a.txt") // content mode names the file
}

func TestGrepCountModeReportsPerFile(t *testing.T) {
	t.Parallel()

	dir, policy := newSearchEnv(t)
	mkfile(dir, "a.txt", "one one two\n")
	res, _ := (&grepTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`{"pattern":"one","mode":"count"}`)), nil)
	assert.False(t, res.IsError)
	assert.Contains(t, textOf(res), "a.txt") // count mode still names the file
}

func TestGrepFilesModeListsMatchingOnly(t *testing.T) {
	t.Parallel()

	dir, policy := newSearchEnv(t)
	mkfile(dir, "hit.txt", "needle")
	mkfile(dir, "miss.txt", "nothing")

	res, _ := (&grepTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`{"pattern":"needle","mode":"files"}`)), nil)
	assert.False(t, res.IsError)
	out := textOf(res)
	assert.Contains(t, out, "hit.txt")
	assert.NotContains(t, out, "miss.txt")
}

func TestGrepInvalidModeIsError(t *testing.T) {
	t.Parallel()

	_, policy := newSearchEnv(t)
	res, _ := (&grepTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`{"pattern":"x","mode":"bogus"}`)), nil)
	assert.True(t, res.IsError)
}

func TestGrepInvalidRegexIsActionableError(t *testing.T) {
	t.Parallel()

	dir, policy := newSearchEnv(t)
	mkfile(dir, "a.txt", "hello\n")

	// both the rg path and the Go fallback must surface a bad pattern as an
	// error, never as a silent empty result
	for name, tool := range map[string]*grepTool{
		"rg-or-go": {policy: policy},
		"go-only":  {policy: policy, forceGo: true},
	} {
		res, _ := tool.Execute(t.Context(),
			callWith([]byte(`{"pattern":"[unclosed"}`)), nil)
		assert.True(t, res.IsError, name)
		assert.Contains(t, textOf(res), "unclosed", name)
	}
}

func TestGrepFallbackHonoursShapeParams(t *testing.T) {
	t.Parallel()

	dir, policy := newSearchEnv(t)
	mkfile(dir, "a.go", "Hello world\nother line\n")
	mkfile(dir, "b.txt", "hello again\n")

	tool := &grepTool{policy: policy, forceGo: true}

	// ignoreCase reaches the capitalized match
	res, _ := tool.Execute(t.Context(),
		callWith([]byte(`{"pattern":"hello","ignoreCase":true}`)), nil)
	assert.Contains(t, textOf(res), "Hello world")

	// glob filters which files are searched
	res, _ = tool.Execute(t.Context(),
		callWith([]byte(`{"pattern":"hello","ignoreCase":true,"glob":"*.txt"}`)), nil)
	out := textOf(res)
	assert.Contains(t, out, "b.txt")
	assert.NotContains(t, out, "a.go")

	// literal treats regex metacharacters as plain text
	res, _ = tool.Execute(t.Context(),
		callWith([]byte(`{"pattern":"other line\n","literal":true}`)), nil)
	assert.Empty(t, textOf(res)) // the \n is literal, not a newline

	// limit caps the match count
	res, _ = tool.Execute(t.Context(),
		callWith([]byte(`{"pattern":"hello","ignoreCase":true,"limit":1}`)), nil)
	assert.Equal(t, 1, strings.Count(textOf(res), "hello")+strings.Count(textOf(res), "Hello"))
}

func TestGrepFallbackContextLines(t *testing.T) {
	t.Parallel()

	dir, policy := newSearchEnv(t)
	mkfile(dir, "a.txt", "before\nmatch here\nafter\n")

	res, _ := (&grepTool{policy: policy, forceGo: true}).Execute(t.Context(),
		callWith([]byte(`{"pattern":"match","context":1}`)), nil)
	assert.False(t, res.IsError)
	out := textOf(res)
	assert.Contains(t, out, ":1: before") // one line of context each side
	assert.Contains(t, out, ":2: match here")
	assert.Contains(t, out, ":3: after")
}

func TestGrepFallbackCountModeSorted(t *testing.T) {
	t.Parallel()

	dir, policy := newSearchEnv(t)
	mkfile(dir, "zeta.txt", "hit\n")
	mkfile(dir, "alpha.txt", "hit\nhit\n") // two matching lines

	res, _ := (&grepTool{policy: policy, forceGo: true}).Execute(t.Context(),
		callWith([]byte(`{"pattern":"hit","mode":"count"}`)), nil)
	assert.False(t, res.IsError)
	out := textOf(res)
	assert.Less(t, strings.Index(out, "alpha.txt"), strings.Index(out, "zeta.txt")) // deterministic order
	assert.Contains(t, out, "alpha.txt:2")
}
