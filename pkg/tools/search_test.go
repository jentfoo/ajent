package tools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

	// a glob matches the right files
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

	// a bare pattern matches at any depth
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

	// ** spans zero or more segments
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

	// a limit truncates with the shared footer naming the spill file
	t.Run("limit_param_truncates", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		for i := 0; i < 5; i++ {
			mkfile(dir, "f"+string(rune('a'+i))+".txt", "")
		}
		res, err := (&findTool{policy: policy, sessionID: "find-test"}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"*.txt","limit":2}`)), nil)
		require.NoError(t, err)
		out := textOf(res)
		assert.Equal(t, 3, strings.Count(out, ".txt")) // head + footer's spill name
		assert.Contains(t, out, "2/5 lines shown")     // truncation is named, not silent
		assert.Contains(t, out, "raise limit")
		m := regexp.MustCompile(`@([^;\s]+)`).FindStringSubmatch(out)
		require.NotNil(t, m)
		dat, err := os.ReadFile(m[1])
		require.NoError(t, err)
		assert.Equal(t, 5, strings.Count(string(dat), ".txt")) // spill holds every match
	})

	t.Run("empty_pattern_rejected", func(t *testing.T) {
		_, policy := newSearchEnv(t)
		res, err := (&findTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"  "}`)), nil)
		require.NoError(t, err)
		assert.True(t, res.IsError)
	})

	// no matches is empty, not an error
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

	// content mode names the file and line numbers
	t.Run("content_mode_finds_line_numbers", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "a.txt", "hello world\nfoo bar\n")

		res, err := (&grepTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"world"}`)), nil)
		require.NoError(t, err)
		assert.False(t, res.IsError)
		out := textOf(res)
		assert.Contains(t, out, "a.txt:1:") // content mode names the file and its line
	})

	t.Run("count_mode_reports_per_file", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "a.txt", "one one two\n")
		res, err := (&grepTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"one","mode":"count"}`)), nil)
		require.NoError(t, err)
		assert.False(t, res.IsError)
		// the count is matching lines, not occurrences: "one one two\n" matches once
		assert.Contains(t, textOf(res), "a.txt:1")
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

	// both the rg path and the Go fallback surface a bad pattern as an error
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
	assert.NotContains(t, out, "result cap") // an explicit limit is the model's choice, not a cut
}

func TestGrepDefaultCapNamed(t *testing.T) {
	t.Parallel()

	// reaching the default cap without an explicit limit is named, never silent
	testLimitsGate.Lock()
	t.Cleanup(testLimitsGate.Unlock) // unlock runs after the restore below
	orig := GrepResultLimit()
	t.Cleanup(func() { ApplyLimits(Limits{Grep: orig}) })
	ApplyLimits(Limits{Grep: Limit{Lines: 3, Bytes: 16 << 10}})

	for name, tool := range map[string]*grepTool{
		"rg-or-go": {policy: PathPolicy{Cwd: "."}, forceGo: false},
		"go-only":  {policy: PathPolicy{Cwd: "."}, forceGo: true},
	} {
		res, err := tool.Execute(t.Context(),
			callWith([]byte(`{"pattern":"func ","glob":"*.go","mode":"files"}`)), nil)
		require.NoError(t, err, name)
		assert.Contains(t, textOf(res), "result cap of 3 matches reached", name)
	}
}

func TestGrepFallback(t *testing.T) {
	t.Parallel()

	// context lines appear on both sides of a match
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

	// count mode output is deterministically sorted
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

	// the match budget cuts across a scan window exactly as the sequential walk
	// would: files after the cut never contribute, at any window position
	t.Run("budget_cuts_across_windows", func(t *testing.T) {
		testLimitsGate.Lock() // the bound in finalize reads the package limits
		defer testLimitsGate.Unlock()
		dir, policy := newSearchEnv(t)
		const files = 24 // spans several windows at any worker-derived window size
		for i := 1; i <= files; i++ {
			mkfile(dir, fmt.Sprintf("f%02d.txt", i), "hit\n")
		}

		res, err := (&grepTool{policy: policy, forceGo: true}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"hit","limit":5}`)), nil)
		require.NoError(t, err)
		assert.False(t, res.IsError)
		out := textOf(res)
		for i := 1; i <= 5; i++ {
			assert.Contains(t, out, fmt.Sprintf("f%02d.txt:1: hit", i))
		}
		assert.NotContains(t, out, "f06.txt")
	})

	// count mode spends the budget by match count, so a file inside the last
	// budgeted window can be partially counted, and a spent budget is named
	t.Run("count_mode_budget_across_windows", func(t *testing.T) {
		testLimitsGate.Lock()
		defer testLimitsGate.Unlock()
		dir, policy := newSearchEnv(t)
		mkfile(dir, "f01.txt", "hit\nhit\n")
		mkfile(dir, "f02.txt", "hit\nhit\n")
		mkfile(dir, "f03.txt", "hit\n")
		for i := 4; i <= 30; i++ {
			mkfile(dir, fmt.Sprintf("f%02d.txt", i), "hit\n")
		}

		res, err := (&grepTool{policy: policy, forceGo: true}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"hit","mode":"count","limit":5}`)), nil)
		require.NoError(t, err)
		assert.False(t, res.IsError)
		out := textOf(res)
		assert.Contains(t, out, "f01.txt:2")
		assert.Contains(t, out, "f02.txt:2")
		assert.Contains(t, out, "f03.txt:1")
		assert.NotContains(t, out, "f04.txt")
		assert.Contains(t, out, "result cap of 5 matches reached") // counts read as authoritative
	})

	// a count search under the budget stays silent, like content mode
	t.Run("count_mode_under_budget_not_noted", func(t *testing.T) {
		testLimitsGate.Lock()
		defer testLimitsGate.Unlock()
		dir, policy := newSearchEnv(t)
		mkfile(dir, "f01.txt", "hit\nhit\n")

		res, err := (&grepTool{policy: policy, forceGo: true}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"hit","mode":"count","limit":5}`)), nil)
		require.NoError(t, err)
		out := textOf(res)
		assert.Contains(t, out, "f01.txt:2")
		assert.NotContains(t, out, "result cap")
	})

	// a binary file holds no text matches; when one exists the result says so,
	// so an empty or short search is never mistaken for complete coverage
	t.Run("binary_file_noted_as_unsearched", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "a.txt", "hit\nhit\n")
		if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte("\x00\x01\x02hit"), 0o644); err != nil {
			t.Fatal(err)
		}

		res, err := (&grepTool{policy: policy, forceGo: true}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"hit","mode":"count","limit":5}`)), nil)
		require.NoError(t, err)
		assert.False(t, res.IsError)
		out := textOf(res)
		assert.Contains(t, out, "a.txt:2")
		assert.NotContains(t, out, "blob.bin") // the binary itself never emits a count
		assert.Contains(t, out, "1 file(s) not searched (binary/unreadable); results may be incomplete")
	})

	// with no unsearchable files the coverage note stays silent: zero token tax
	// on the common all-text search
	t.Run("no_note_when_everything_searched", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "a.txt", "hit\n")

		res, err := (&grepTool{policy: policy, forceGo: true}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"hit"}`)), nil)
		require.NoError(t, err)
		out := textOf(res)
		assert.Contains(t, out, "a.txt:1: hit")
		assert.NotContains(t, out, "not searched")
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

func TestFindGitRepoUsableNonAsciiPath(t *testing.T) {
	t.Parallel()

	dir, policy := newSearchEnv(t)
	mkfile(dir, "café.go", "x")
	mkfile(dir, "ignored.log", "y") // .gitignore'd below
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n"), 0o644))

	gitInit(t, dir)

	res, err := (&findTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`{"pattern":"caf*.go"}`)), nil)
	require.NoError(t, err)
	out := textOf(res)

	assert.Contains(t, out, "café.go") // usable relative path, not octal-escaped
	assert.NotContains(t, out, "ignored.log")
}

func TestGrepFallbackSkipsGitIgnored(t *testing.T) {
	t.Parallel()

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

func TestGrepFallbackCrlfStripsTrailingCarriage(t *testing.T) {
	t.Parallel()

	dir, policy := newSearchEnv(t)
	mkfile(dir, "a.txt", "match here\r\nother line\r\n")

	res, err := (&grepTool{policy: policy, forceGo: true}).Execute(t.Context(),
		callWith([]byte(`{"pattern":"match"}`)), nil)
	require.NoError(t, err)
	assert.False(t, res.IsError)
	out := textOf(res)
	assert.NotContains(t, out, "\r") // LF-only model-visible output
}

func TestRelToDotPrefixedFilename(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "..hidden.go", relTo("/a", "/a/..hidden.go"))
	assert.Equal(t, "/a/sibling", relTo("/a/b", "/a/sibling")) // true escape stays absolute
}

// TestFindScopedInsideGitRepo asserts a nested root never enumerates files
// outside its subtree.
func TestFindScopedInsideGitRepo(t *testing.T) {
	t.Parallel()

	// files outside the requested subtree must never appear in find results.
	dir, _ := newSearchEnv(t)
	mkfile(dir, "outside/leak.go", "x")
	mkfile(dir, "root/nested/inner.go", "y")
	gitInit(t, dir)

	nestedRoot := filepath.Join(dir, "root", "nested")
	policy := PathPolicy{Cwd: nestedRoot}
	res, err := (&findTool{policy: policy}).Execute(t.Context(),
		callWith([]byte(`{"pattern":"*.go"}`)), nil)
	require.NoError(t, err)

	out := textOf(res)
	assert.Contains(t, out, "inner.go")   // the in-scope file is found
	assert.NotContains(t, out, "leak.go") // a sibling outside scope never leaks
}

func TestGrepFallbackScopedInsideGitRepo(t *testing.T) {
	t.Parallel()

	dir, _ := newSearchEnv(t)
	mkfile(dir, "outside/leak.go", "needle\n")
	mkfile(dir, "root/nested/inner.go", "needle\n")
	gitInit(t, dir)

	nestedRoot := filepath.Join(dir, "root", "nested")
	policy := PathPolicy{Cwd: nestedRoot}
	res, err := (&grepTool{policy: policy, forceGo: true}).Execute(t.Context(),
		callWith([]byte(`{"pattern":"needle"}`)), nil)
	require.NoError(t, err)

	out := textOf(res)
	assert.Contains(t, out, "inner.go")   // the in-scope file is searched
	assert.NotContains(t, out, "leak.go") // a sibling outside scope never leaks
}
