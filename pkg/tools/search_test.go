package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestFind(t *testing.T) {
	t.Parallel()

	// a glob matches the right files.
	t.Run("matches_glob", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "a.go", "x")
		mkfile(dir, "b.txt", "y")

		res, err := (&findTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"*.go"}`)), nil)
		require.NoError(t, err)
		out := textOf(res)
		assert.Contains(t, out, "a.go")
		assert.NotContains(t, out, "b.txt")
	})

	// a bare pattern matches at any depth.
	t.Run("bare_pattern_matches_at_any_depth", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "top.go", "x")
		mkfile(dir, "pkg/tools/nested.go", "y")

		res, err := (&findTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"*.go"}`)), nil)
		require.NoError(t, err)
		out := textOf(res)
		assert.Contains(t, out, "top.go")
		assert.Contains(t, out, filepath.Join("pkg", "tools", "nested.go")) // bare glob reaches any depth
	})

	// ** spans zero or more segments.
	t.Run("doublestar_matches_root_and_nested", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "root.go", "x")
		mkfile(dir, "sub/inner.go", "y")
		mkfile(dir, "sub/skip.txt", "z")

		res, err := (&findTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"**/*.go"}`)), nil)
		require.NoError(t, err)
		out := textOf(res)
		assert.Contains(t, out, "root.go") // ** spans zero segments too
		assert.Contains(t, out, filepath.Join("sub", "inner.go"))
		assert.NotContains(t, out, "skip.txt")
	})

	// a limit truncates with an explicit marker.
	t.Run("limit_param_truncates", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		for i := 0; i < 5; i++ {
			mkfile(dir, "f"+string(rune('a'+i))+".txt", "")
		}
		res, err := (&findTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"*.txt","limit":2}`)), nil)
		require.NoError(t, err)
		out := textOf(res)
		assert.Equal(t, 2, strings.Count(out, ".txt"))
		assert.Contains(t, out, "more results") // truncation is named, not silent
	})

	t.Run("empty_pattern_rejected", func(t *testing.T) {
		_, policy := newSearchEnv(t)
		res, err := (&findTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"  "}`)), nil)
		require.NoError(t, err)
		assert.True(t, res.IsError)
	})

	// no matches is empty, not an error.
	t.Run("no_match_is_empty_not_error", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "a.md", "x")

		res, err := (&findTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"*.rs"}`)), nil)
		require.NoError(t, err)
		assert.False(t, res.IsError)
		assert.Empty(t, textOf(res))
	})

	t.Run("bounded_results", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		for i := 0; i < 20; i++ {
			mkfile(dir, "f"+string(rune('a'+i))+".txt", "")
		}
		res, err := (&findTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"*.txt"}`)), nil)
		require.NoError(t, err)
		out := textOf(res)
		assert.LessOrEqual(t, strings.Count(out, ".txt"), FindResultLimit().Lines)
	})

	t.Run("malformed_args_is_error", func(t *testing.T) {
		_, policy := newSearchEnv(t)
		res, err := (&findTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`not json`)), nil)
		require.NoError(t, err)
		assert.True(t, res.IsError)
	})
}

func TestGrep(t *testing.T) {
	t.Parallel()

	// content mode names the file and line numbers.
	t.Run("content_mode_finds_line_numbers", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "a.txt", "hello world\nfoo bar\n")

		res, err := (&grepTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"world"}`)), nil)
		require.NoError(t, err)
		assert.False(t, res.IsError)
		out := textOf(res)
		assert.Contains(t, out, "a.txt") // content mode names the file
	})

	t.Run("count_mode_reports_per_file", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "a.txt", "one one two\n")
		res, err := (&grepTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"one","mode":"count"}`)), nil)
		require.NoError(t, err)
		assert.False(t, res.IsError)
		assert.Contains(t, textOf(res), "a.txt") // count mode still names the file
	})

	// files mode lists only matching files.
	t.Run("files_mode_lists_matching_only", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "hit.txt", "needle")
		mkfile(dir, "miss.txt", "nothing")

		res, err := (&grepTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"needle","mode":"files"}`)), nil)
		require.NoError(t, err)
		assert.False(t, res.IsError)
		out := textOf(res)
		assert.Contains(t, out, "hit.txt")
		assert.NotContains(t, out, "miss.txt")
	})

	t.Run("invalid_mode_is_error", func(t *testing.T) {
		_, policy := newSearchEnv(t)
		res, err := (&grepTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"x","mode":"bogus"}`)), nil)
		require.NoError(t, err)
		assert.True(t, res.IsError)
	})

	// both the rg path and the Go fallback surface a bad pattern as an error.
	t.Run("invalid_regex_is_actionable_error", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "a.txt", "hello\n")

		for name, tool := range map[string]*grepTool{
			"rg-or-go": {policy: policy},
			"go-only":  {policy: policy, forceGo: true},
		} {
			res, err := tool.Execute(t.Context(),
				callWith([]byte(`{"pattern":"[unclosed"}`)), nil)
			require.NoError(t, err, name)
			assert.True(t, res.IsError, name)
			assert.Contains(t, textOf(res), "unclosed", name)
		}
	})

	// a match line longer than MaxLineRunes is capped for the model and the full
	// text spills to disk, both when rg answers and on the Go fallback.
	t.Run("minified_line_capped_and_spilled", func(t *testing.T) {
		for _, forceGo := range []bool{false, true} {
			dir, policy := newSearchEnv(t)
			mkfile(dir, "min.txt", "needle "+strings.Repeat("y", MaxLineRunes+500)+"\n")

			res, err := (&grepTool{policy: policy, forceGo: forceGo}).Execute(t.Context(),
				callWith([]byte(`{"pattern":"needle"}`)), nil)
			require.NoError(t, err)
			assert.False(t, res.IsError)
			out := textOf(res)
			for _, ln := range strings.Split(out, "\n") {
				assert.LessOrEqual(t, len([]rune(ln)), MaxLineRunes+100) // footer carries the spill note
			}
			assert.Regexp(t, `full output in @\S+`, out)
		}
	})
}

func TestGrepFallbackHonoursShapeParams(t *testing.T) {
	t.Parallel()

	dir, policy := newSearchEnv(t)
	mkfile(dir, "a.go", "Hello world\nother line\n")
	mkfile(dir, "b.txt", "hello again\n")

	tool := &grepTool{policy: policy, forceGo: true}

	assertGrepResult := func(args string) agent.ToolResult {
		res, err := tool.Execute(t.Context(), callWith([]byte(args)), nil)
		require.NoError(t, err)
		return res
	}

	// ignoreCase reaches the capitalized match
	out := textOf(assertGrepResult(`{"pattern":"hello","ignoreCase":true}`))
	assert.Contains(t, out, "Hello world")

	// glob filters which files are searched
	out = textOf(assertGrepResult(`{"pattern":"hello","ignoreCase":true,"glob":"*.txt"}`))
	assert.Contains(t, out, "b.txt")
	assert.NotContains(t, out, "a.go")

	// literal treats regex metacharacters as plain text
	res := assertGrepResult(`{"pattern":"other line\n","literal":true}`)
	assert.False(t, res.IsError)
	out = textOf(res)
	assert.Empty(t, out) // the \n is literal, not a newline

	// limit caps the match count
	out = textOf(assertGrepResult(`{"pattern":"hello","ignoreCase":true,"limit":1}`))
	assert.Equal(t, 1, strings.Count(out, "hello")+strings.Count(out, "Hello"))
}

func TestGrepFallback(t *testing.T) {
	t.Parallel()

	// context lines appear on both sides of a match.
	t.Run("context_lines", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "a.txt", "before\nmatch here\nafter\n")

		res, err := (&grepTool{policy: policy, forceGo: true}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"match","context":1}`)), nil)
		require.NoError(t, err)
		assert.False(t, res.IsError)
		out := textOf(res)
		assert.Contains(t, out, ":1: before") // one line of context each side
		assert.Contains(t, out, ":2: match here")
		assert.Contains(t, out, ":3: after")
	})

	// count mode output is deterministically sorted.
	t.Run("count_mode_sorted", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "zeta.txt", "hit\n")
		mkfile(dir, "alpha.txt", "hit\nhit\n") // two matching lines

		res, err := (&grepTool{policy: policy, forceGo: true}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"hit","mode":"count"}`)), nil)
		require.NoError(t, err)
		assert.False(t, res.IsError)
		out := textOf(res)
		assert.Less(t, strings.Index(out, "alpha.txt"), strings.Index(out, "zeta.txt")) // deterministic order
		assert.Contains(t, out, "alpha.txt:2")
	})
}

// gitInit turns dir into a fresh repo with everything tracked.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	init := exec.CommandContext(t.Context(), "git", "init", "-q")
	init.Dir = dir
	if err := init.Run(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	add := exec.CommandContext(t.Context(), "git", "add", "-A")
	add.Dir = dir
	require.NoError(t, add.Run())
}

// TestFindGitRepoUsableNonAsciiPath exercises the git ls-files -z path: a
// non-ASCII filename must come back as a usable relative path and .gitignore'd
// files are excluded.
func TestFindGitRepoUsableNonAsciiPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		pattern string
	}{
		{"non_ascii_filename", "caf*.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, policy := newSearchEnv(t)
			mkfile(dir, "café.go", "x")
			mkfile(dir, "ignored.log", "y") // .gitignore'd below
			require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n"), 0o644))

			gitInit(t, dir)

			res, err := (&findTool{policy: policy}).Execute(t.Context(),
				callWith([]byte(`{"pattern":`+strconv.Quote(tc.pattern)+`}`)), nil)
			require.NoError(t, err)
			out := textOf(res)

			assert.Contains(t, out, "café.go") // usable relative path, not octal-escaped
			assert.NotContains(t, out, "ignored.log")
		})
	}
}

// TestGrepFallbackSkipsGitIgnored asserts the Go fallback honours .gitignore via
// git ls-files, matching what rg does.
func TestGrepFallbackSkipsGitIgnored(t *testing.T) {
	dir, policy := newSearchEnv(t)
	mkfile(dir, "keep.go", "needle\n")
	mkfile(dir, "dist/bundle.js", "needle\n") // .gitignore'd
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("dist/\n"), 0o644))

	gitInit(t, dir)

	res, err := (&grepTool{policy: policy, forceGo: true}).Execute(t.Context(),
		callWith([]byte(`{"pattern":"needle"}`)), nil)
	require.NoError(t, err)
	out := textOf(res)

	assert.Contains(t, out, "keep.go")
	assert.NotContains(t, out, "bundle.js") // ignored subtree pruned
}

// TestGrepFallbackCrlfStripsTrailingCarriage returns no trailing \r on either path.
func TestGrepFallbackCrlfStripsTrailingCarriage(t *testing.T) {
	dir, policy := newSearchEnv(t)
	mkfile(dir, "a.txt", "match here\r\nother line\r\n")

	for name, tool := range map[string]*grepTool{
		"go-only": {policy: policy, forceGo: true},
	} {
		res, err := tool.Execute(t.Context(),
			callWith([]byte(`{"pattern":"match"}`)), nil)
		require.NoError(t, err, name)
		assert.False(t, res.IsError, name)
		out := textOf(res)
		assert.NotContains(t, out, "\r", name) // LF-only model-visible output
	}
}

// TestRelToDotPrefixedFilename treats a ..-prefixed filename as inside the root.
func TestRelToDotPrefixedFilename(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "..hidden.go", relTo("/a", "/a/..hidden.go"))
	assert.Equal(t, "/a/sibling", relTo("/a/b", "/a/sibling")) // true escape stays absolute
}
