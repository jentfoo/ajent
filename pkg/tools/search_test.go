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
			callWith([]byte(`{"pattern":"*.go"}`)), agent.NewOutput(agent.NopSink{}, "c"))
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
			callWith([]byte(`{"pattern":"*.go"}`)), agent.NewOutput(agent.NopSink{}, "c"))
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
			callWith([]byte(`{"pattern":"**/*.go"}`)), agent.NewOutput(agent.NopSink{}, "c"))
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
			callWith([]byte(`{"pattern":"*.txt","limit":2}`)), agent.NewOutput(agent.NopSink{}, "c"))
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
			callWith([]byte(`{"pattern":"  "}`)), agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		assert.True(t, res.IsError)
	})

	// no matches is empty, not an error
	t.Run("no_match_is_empty_not_error", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "a.md", "x")

		res, err := (&findTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"*.rs"}`)), agent.NewOutput(agent.NopSink{}, "c"))
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
			callWith([]byte(`{"pattern":"*.txt"}`)), agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		out := textOf(res)
		assert.LessOrEqual(t, strings.Count(out, ".txt"), FindResultLimit().Lines)
	})

	t.Run("malformed_args_is_error", func(t *testing.T) {
		_, policy := newSearchEnv(t)
		res, err := (&findTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`not json`)), agent.NewOutput(agent.NopSink{}, "c"))
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
			callWith([]byte(`{"pattern":"world"}`)), agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		assert.False(t, res.IsError)
		out := textOf(res)
		assert.Contains(t, out, "a.txt:1:") // content mode names the file and its line
	})

	t.Run("count_mode_reports_per_file", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "a.txt", "one one two\n")
		res, err := (&grepTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"one","mode":"count"}`)), agent.NewOutput(agent.NopSink{}, "c"))
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
			callWith([]byte(`{"pattern":"needle","mode":"files"}`)), agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		assert.False(t, res.IsError)
		out := textOf(res)
		assert.Contains(t, out, "hit.txt")
		assert.NotContains(t, out, "miss.txt")
	})

	t.Run("invalid_mode_is_error", func(t *testing.T) {
		_, policy := newSearchEnv(t)
		res, err := (&grepTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"x","mode":"bogus"}`)), agent.NewOutput(agent.NopSink{}, "c"))
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
				callWith([]byte(`{"pattern":"[unclosed"}`)), agent.NewOutput(agent.NopSink{}, "c"))
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
				callWith([]byte(`{"pattern":"needle"}`)), agent.NewOutput(agent.NopSink{}, "c"))
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
		res, err := tool.Execute(t.Context(), callWith([]byte(args)), agent.NewOutput(agent.NopSink{}, "c"))
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
			callWith([]byte(`{"pattern":"func ","glob":"*.go","mode":"files"}`)), agent.NewOutput(agent.NopSink{}, "c"))
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
			callWith([]byte(`{"pattern":"match","context":1}`)), agent.NewOutput(agent.NopSink{}, "c"))
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
			callWith([]byte(`{"pattern":"hit","mode":"count"}`)), agent.NewOutput(agent.NopSink{}, "c"))
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
			callWith([]byte(`{"pattern":"hit","limit":5}`)), agent.NewOutput(agent.NopSink{}, "c"))
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
			callWith([]byte(`{"pattern":"hit","mode":"count","limit":5}`)), agent.NewOutput(agent.NopSink{}, "c"))
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
			callWith([]byte(`{"pattern":"hit","mode":"count","limit":5}`)), agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		out := textOf(res)
		assert.Contains(t, out, "f01.txt:2")
		assert.NotContains(t, out, "result cap")
	})

	// an exact fill still names the cap on the fallback: telling a natural end from
	// more files would mean scanning past the budget, so its note is accepted cost
	t.Run("count_exact_fill_still_noted", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		for i := 1; i <= 3; i++ {
			mkfile(dir, fmt.Sprintf("f%02d.txt", i), "hit\n")
		}

		res, err := (&grepTool{policy: policy, forceGo: true}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"hit","mode":"count","limit":3}`)), agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		out := textOf(res)
		assert.Contains(t, out, "f01.txt:1")
		assert.Contains(t, out, "result cap of 3 matches reached") // conservative on the fallback
	})

	// a binary file holds no text matches. When one exists the result says so,
	// an empty or short search never being mistaken for complete coverage
	t.Run("binary_file_noted_as_unsearched", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "a.txt", "hit\nhit\n")
		if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte("\x00\x01\x02hit"), 0o644); err != nil {
			t.Fatal(err)
		}

		res, err := (&grepTool{policy: policy, forceGo: true}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"hit","mode":"count","limit":5}`)), agent.NewOutput(agent.NopSink{}, "c"))
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
			callWith([]byte(`{"pattern":"hit"}`)), agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		out := textOf(res)
		assert.Contains(t, out, "a.txt:1: hit")
		assert.NotContains(t, out, "not searched")
	})

	// an explicit limit spent under context names the cap, mirroring the rg block
	// path where a cut withholds whole matches even under the model's own max
	t.Run("context_limit_names_cap", func(t *testing.T) {
		testLimitsGate.Lock()
		defer testLimitsGate.Unlock()
		dir, policy := newSearchEnv(t)
		for i := 1; i <= 6; i++ {
			mkfile(dir, fmt.Sprintf("f%02d.txt", i), "pre-a\npre-b\nmatchline\npost-a\npost-b\n")
		}

		res, err := (&grepTool{policy: policy, forceGo: true}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"matchline","context":2,"limit":5}`)), agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		out := textOf(res)
		assert.Equal(t, 5, strings.Count(out, "matchline")) // the sixth block never emitted
		assert.Contains(t, out, "result cap of 5 matches reached")
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
		callWith([]byte(`{"pattern":"caf*.go"}`)), agent.NewOutput(agent.NopSink{}, "c"))
	require.NoError(t, err)
	out := textOf(res)

	assert.Contains(t, out, "café.go") // usable relative path, not octal-escaped
	assert.NotContains(t, out, "ignored.log")
}

func TestGrepRgPaths(t *testing.T) {
	t.Parallel()

	if !rgOnPath() {
		t.Skip("ripgrep not on PATH")
	}
	// runRg and finalize read the package limits other tests mutate
	testLimitsGate.Lock()
	t.Cleanup(testLimitsGate.Unlock)

	// a small count search stays silent: nothing was withheld, yet the rg path
	// used to append a cap footer unconditionally
	t.Run("count_under_budget_silent", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "a.txt", "one\n")
		mkfile(dir, "b.txt", "one\n")

		res, err := (&grepTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"one","mode":"count"}`)), agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		out := textOf(res)
		assert.Contains(t, out, ".txt:1")
		assert.NotContains(t, out, "result cap")
	})

	// count output that spends the budget names the cap, mirroring the fallback
	t.Run("count_cut_names_cap", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		for i := 1; i <= 5; i++ {
			mkfile(dir, fmt.Sprintf("f%02d.txt", i), "matchline\n")
		}

		res, err := (&grepTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"matchline","mode":"count","limit":2}`)), agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		out := textOf(res)
		assert.Equal(t, 2, strings.Count(out, ".txt:")) // a further file would be withheld whole
		assert.Contains(t, out, "result cap of 2 matches reached")
	})

	// a search filling the default budget exactly completed on its own: nothing
	// followed the fill, so a cap note would send the model chasing matches that
	// do not exist (an explicit limit stays silent by its own rule either way)
	t.Run("exact_fill_silent", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "a.txt", "hit one\nmiss\nhit two\n")
		mkfile(dir, "b.txt", "hit three\n")

		orig := GrepResultLimit()
		t.Cleanup(func() { ApplyLimits(Limits{Grep: orig}) })
		ApplyLimits(Limits{Grep: Limit{Lines: 3, Bytes: 16 << 10}})

		res, err := (&grepTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"hit"}`)), agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		out := textOf(res)
		assert.Equal(t, 2, strings.Count(out, "a.txt:")) // all three matches streamed
		assert.Contains(t, out, "b.txt:1:")
		assert.NotContains(t, out, "result cap")
	})

	// a single file holding more matches than the default budget still names the
	// cap: no per-file bound may end rg at exactly max, which would dress real
	// withholding up as a natural end once an exact fill stays silent
	t.Run("single_file_over_budget_names_cap", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "a.txt", "hit\nhit\nhit\nhit") // no final newline: the last line still reports

		orig := GrepResultLimit()
		t.Cleanup(func() { ApplyLimits(Limits{Grep: orig}) })
		ApplyLimits(Limits{Grep: Limit{Lines: 3, Bytes: 16 << 10}})

		res, err := (&grepTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"hit"}`)), agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		out := textOf(res)
		assert.Equal(t, 3, strings.Count(out, "a.txt:")) // three of the four matches shown
		assert.Contains(t, out, "result cap of 3 matches reached")
	})

	// count output filling its budget exactly is silent too: rg ended right after,
	// so no file's count was withheld or trimmed
	t.Run("count_exact_fill_silent", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "a.txt", "hit\nhit\n")
		mkfile(dir, "b.txt", "hit\n")

		res, err := (&grepTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"hit","mode":"count","limit":3}`)), agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		out := textOf(res)
		assert.Contains(t, out, "a.txt:2")
		assert.Contains(t, out, "b.txt:1")
		assert.NotContains(t, out, "result cap")
	})

	// a count trimmed partway hides its remainder even when rg ends right after,
	// so the cap stays named though the budget filled without a further file
	t.Run("count_trim_names_cap", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "a.txt", "hit\nhit\nhit\n")

		res, err := (&grepTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"hit","mode":"count","limit":2}`)), agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		out := textOf(res)
		assert.Contains(t, out, "a.txt:2") // the third match is trimmed off the count
		assert.Contains(t, out, "result cap of 2 matches reached")
	})

	// context blocks are bounded by an explicit match limit too: each match adds
	// at most 2*context+1 lines plus its group separator, so two matches stay
	// within 12 streamed lines and a third block never appears
	t.Run("block_explicit_limit_bounds", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		for i := 1; i <= 6; i++ {
			mkfile(dir, fmt.Sprintf("f%02d.txt", i), "pre-a\npre-b\nmatchline\npost-a\npost-b\n")
		}

		res, err := (&grepTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"matchline","context":2,"limit":2}`)), agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		out := textOf(res)
		assert.Equal(t, 2, strings.Count(out, "matchline"))
		assert.Contains(t, out, "result cap of 2 matches reached") // the explicit budget is named
	})

	// without an explicit limit a context stream rides to the match budget, so a
	// completed search never claims a match cap; finalize names the output bound
	// it applied and spills the complete text instead
	t.Run("block_output_bound_spills", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		for i := 1; i <= 30; i++ {
			mkfile(dir, fmt.Sprintf("f%02d.txt", i), "pre-a\npre-b\nmatchline\npost-a\npost-b\n")
		}

		res, err := (&grepTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"matchline","context":2}`)), agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		out := textOf(res)
		assert.NotContains(t, out, "result cap") // every match streamed; a cap note would be false
		assert.Contains(t, out, "truncated:")    // the output bound is named where it fired
		m := regexp.MustCompile(`@([^;\s]+)`).FindStringSubmatch(out)
		require.NotNil(t, m)
		dat, err := os.ReadFile(m[1])
		require.NoError(t, err)
		assert.Equal(t, 30, strings.Count(string(dat), "matchline")) // the spill holds every block
	})

	// a pattern occurring in printed paths must not spend the match budget on
	// context lines: only true matches count toward the cap, and a further file's
	// leading context never prints orphaned once the budget is spent
	t.Run("block_path_not_spending_budget", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
			mkfile(dir, filepath.Join("needle", name), "before\nneedle match\nafter\n")
		}

		res, err := (&grepTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"needle","context":1,"limit":2}`)), agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		out := textOf(res)
		assert.Equal(t, 2, strings.Count(out, "needle match")) // both real matches streamed
		assert.Contains(t, out, "result cap of 2 matches reached")
		assert.Equal(t, 2, strings.Count(out, "-3-after")) // the cut block keeps its trailing context
		assert.Equal(t, 1, strings.Count(out, "\n--\n"))   // a third block never starts: one separator only
	})

	// context blocks filling the budget at a natural end stay silent too, and the
	// final block still rides its trailing context lines whole
	t.Run("block_exact_fill_silent", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "a.txt", "top\nhit one\nbottom\n")
		mkfile(dir, "b.txt", "top\nhit two\nbottom\n")

		res, err := (&grepTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"hit","context":1,"limit":2}`)), agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		out := textOf(res)
		assert.NotContains(t, out, "result cap") // both matches streamed, nothing withheld
		assert.Contains(t, out, "b.txt-3-bottom")
	})

	// a non-UTF-8 line still prints: rg delivers its raw bytes when text is invalid
	t.Run("block_non_utf8_line_prints", func(t *testing.T) {
		dir, policy := newSearchEnv(t)
		mkfile(dir, "lat.txt", "\xe9caf\nneedle\n")

		res, err := (&grepTool{policy: policy}).Execute(t.Context(),
			callWith([]byte(`{"pattern":"needle","context":1}`)), agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)
		out := textOf(res)
		assert.Contains(t, out, "lat.txt:2:")        // the match keeps its attribution
		assert.Contains(t, out, "lat.txt-1-\xe9caf") // the invalid context line is not dropped
	})
}

func TestGrepFallbackSkipsGitIgnored(t *testing.T) {
	t.Parallel()

	dir, policy := newSearchEnv(t)
	mkfile(dir, "keep.go", "needle\n")
	mkfile(dir, "dist/bundle.js", "needle\n") // .gitignore'd
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("dist/\n"), 0o644))

	gitInit(t, dir)

	res, err := (&grepTool{policy: policy, forceGo: true}).Execute(t.Context(),
		callWith([]byte(`{"pattern":"needle"}`)), agent.NewOutput(agent.NopSink{}, "c"))
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
		callWith([]byte(`{"pattern":"match"}`)), agent.NewOutput(agent.NopSink{}, "c"))
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

func TestRelLine(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "sub/a.txt:1:x\n", relLine("/root/dir", "/root/dir/sub/a.txt:1:x\n"))
	assert.Equal(t, "sub/a.txt\n", relLine("/root/dir", "/root/dir/sub/a.txt\n")) // files mode
	assert.Equal(t, "--\n", relLine("/root/dir", "--\n"))                         // group separator untouched
	assert.Equal(t, "a.txt:2\n", relLine("/root/a.txt", "/root/a.txt:2\n"))       // single file names itself
	assert.Equal(t, "/elsewhere/x:1:y", relLine("/root/dir", "/elsewhere/x:1:y"))
	assert.Equal(t, "a.txt:1:x", relLine("/root/dir", "/root/dir/a.txt:1:x")) // no newline stays bare
}

func TestGrepRel(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "sub/a.txt", grepRel("/root/dir", "/root/dir/sub/a.txt"))
	// a hidden file keeps its dot prefix and an escape stays absolute, like relTo
	assert.Equal(t, "..hidden.go", grepRel("/a", "/a/..hidden.go"))
	assert.Equal(t, "/a/sibling", grepRel("/a/b", "/a/sibling"))
	// searching a single file attributes to that file's own name
	assert.Equal(t, "a.txt", grepRel("/root/dir/a.txt", "/root/dir/a.txt"))
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
		callWith([]byte(`{"pattern":"*.go"}`)), agent.NewOutput(agent.NopSink{}, "c"))
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
		callWith([]byte(`{"pattern":"needle"}`)), agent.NewOutput(agent.NopSink{}, "c"))
	require.NoError(t, err)

	out := textOf(res)
	assert.Contains(t, out, "inner.go")   // the in-scope file is searched
	assert.NotContains(t, out, "leak.go") // a sibling outside scope never leaks
}

func TestGrepSingleFileSearch(t *testing.T) {
	t.Parallel()

	for name, forceGo := range map[string]bool{"rg-or-go": false, "go-only": true} {
		t.Run(name, func(t *testing.T) {
			testLimitsGate.Lock()
			t.Cleanup(testLimitsGate.Unlock)
			if !forceGo && !rgOnPath() {
				t.Skip("ripgrep not on PATH")
			}
			dir, policy := newSearchEnv(t)
			mkfile(dir, "a.txt", "hit one\nmiss\nhit two\n")
			tool := &grepTool{policy: policy, forceGo: forceGo}

			run := func(args string) agent.ToolResult {
				res, err := tool.Execute(t.Context(), callWith([]byte(args)), agent.NewOutput(agent.NopSink{}, "c"))
				require.NoError(t, err)
				assert.False(t, res.IsError)
				return res
			}

			// count mode reports the file's matches instead of an empty result
			out := textOf(run(fmt.Sprintf(`{"pattern":"hit","mode":"count","path":%q}`, filepath.Join(dir, "a.txt"))))
			assert.Equal(t, "a.txt:2", strings.TrimSpace(out))

			// content mode keeps file attribution on every line
			out = textOf(run(fmt.Sprintf(`{"pattern":"hit","path":%q}`, filepath.Join(dir, "a.txt"))))
			assert.Contains(t, out, "a.txt:1:")
			assert.Contains(t, out, "a.txt:3:")

			// files mode names the file rather than a bare "."
			out = textOf(run(fmt.Sprintf(`{"pattern":"hit","mode":"files","path":%q}`, filepath.Join(dir, "a.txt"))))
			assert.Equal(t, "a.txt", strings.TrimSpace(out))
		})
	}
}

func TestGrepNaturalCompletionNotCapped(t *testing.T) {
	t.Parallel()

	var content strings.Builder
	for i := 0; i < 20; i++ {
		content.WriteString(strings.Repeat("pad\n", 5) + "needle\n")
	}

	for name, forceGo := range map[string]bool{"rg-or-go": false, "go-only": true} {
		t.Run(name, func(t *testing.T) {
			testLimitsGate.Lock() // the match budget defaults to GrepResultLimit
			t.Cleanup(testLimitsGate.Unlock)
			if !forceGo && !rgOnPath() {
				t.Skip("ripgrep not on PATH")
			}
			dir, policy := newSearchEnv(t)
			mkfile(dir, "a.txt", content.String())

			res, err := (&grepTool{policy: policy, forceGo: forceGo}).Execute(t.Context(),
				callWith([]byte(`{"pattern":"needle","context":5}`)), agent.NewOutput(agent.NopSink{}, "c"))
			require.NoError(t, err)
			out := textOf(res)
			assert.Contains(t, out, "a.txt:")
			assert.NotContains(t, out, "result cap") // 20 matches completed the search
		})
	}
}

func TestGrepPathShapeRootRelative(t *testing.T) {
	t.Parallel()

	for name, forceGo := range map[string]bool{"rg-or-go": false, "go-only": true} {
		t.Run(name, func(t *testing.T) {
			testLimitsGate.Lock()
			t.Cleanup(testLimitsGate.Unlock)
			if !forceGo && !rgOnPath() {
				t.Skip("ripgrep not on PATH")
			}
			dir, policy := newSearchEnv(t)
			mkfile(dir, "sub/a.txt", "needle\n")

			res, err := (&grepTool{policy: policy, forceGo: forceGo}).Execute(t.Context(),
				callWith([]byte(`{"pattern":"needle"}`)), agent.NewOutput(agent.NopSink{}, "c"))
			require.NoError(t, err)
			out := textOf(res)
			assert.Contains(t, out, filepath.Join("sub", "a.txt")+":1:") // one shape either way
			assert.NotContains(t, out, dir)                              // never absolute
		})
	}
}
