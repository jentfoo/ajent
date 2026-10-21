package projinit

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDocFiles(t *testing.T) {
	t.Parallel()

	t.Run("readmes_sorted", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "README.rst", "README.md", "CONTRIBUTING.md", "main.go")
		paths, existing := docFiles(dir)
		assert.Equal(t, []string{"README.md", "README.rst"}, paths)
		assert.False(t, existing)
	})

	t.Run("agents_file_last", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "README.md", "AGENTS.md")
		paths, existing := docFiles(dir)
		assert.Equal(t, []string{"README.md", "AGENTS.md"}, paths)
		assert.True(t, existing)
	})

	t.Run("directory_is_not_a_doc", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "READMEs"), 0o755))
		paths, existing := docFiles(dir)
		assert.Empty(t, paths)
		assert.False(t, existing)
	})

	t.Run("empty_project", func(t *testing.T) {
		paths, existing := docFiles(t.TempDir())
		assert.Empty(t, paths)
		assert.False(t, existing)
	})
}

func TestCodeSlices(t *testing.T) {
	t.Parallel()

	// tree builds n files spread over the named directories.
	tree := func(t *testing.T, perDir int, dirs ...string) string {
		t.Helper()

		dir := t.TempDir()
		paths := make([]string, 0, perDir*len(dirs))
		for _, d := range dirs {
			for i := range perDir {
				paths = append(paths, d+"/f"+strconv.Itoa(i)+".go")
			}
		}
		writeTree(t, dir, paths...)
		return dir
	}

	t.Run("small_repo_one_slice", func(t *testing.T) {
		got := codeSlices(t.Context(), tree(t, 5, "pkg", "cmd"))
		assert.Len(t, got, 1)
		assert.ElementsMatch(t, []string{"pkg/", "cmd/"}, got[0])
	})

	t.Run("large_repo_caps_at_four", func(t *testing.T) {
		got := codeSlices(t.Context(), tree(t, 200, "a", "b", "c", "d", "e"))
		assert.Len(t, got, maxCodeAgents)
	})

	t.Run("count_capped_by_dirs", func(t *testing.T) {
		got := codeSlices(t.Context(), tree(t, 700, "only"))
		assert.Len(t, got, 1)
	})

	t.Run("slices_are_disjoint", func(t *testing.T) {
		dirs := []string{"a", "b", "c", "d", "e", "f"}
		got := codeSlices(t.Context(), tree(t, 60, dirs...))
		all := make([]string, 0, len(dirs))
		for _, s := range got {
			all = append(all, s...)
		}
		for _, d := range dirs {
			assert.Equal(t, 1, count(all, d+"/"), d)
		}
	})

	t.Run("root_files_are_one_unit", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "pkg/a.go", "main.go", "go.mod")
		got := codeSlices(t.Context(), dir)
		require.NotEmpty(t, got)
		// loose files collapse to one phrase rather than a scatter of singletons
		assert.Contains(t, got[0], "the files at the repository root")
	})

	t.Run("skips_files_other_stages_read", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "pkg/a.go", "README.md", "AGENTS.md", "Makefile", "LICENSE", "go.sum")
		got := codeSlices(t.Context(), dir)
		require.Len(t, got, 1)
		assert.Equal(t, []string{"pkg/"}, got[0]) // nothing left at the root to survey
	})

	t.Run("dominant_directory_opens_up", func(t *testing.T) {
		dir := t.TempDir()
		paths := make([]string, 0, 241)
		for _, sub := range []string{"a", "b", "c", "d"} {
			for i := range 60 {
				paths = append(paths, "pkg/"+sub+"/f"+strconv.Itoa(i)+".go")
			}
		}
		paths = append(paths, "docs/readme.txt")
		writeTree(t, dir, paths...)

		got := codeSlices(t.Context(), dir)
		require.Greater(t, len(got), 1)
		all := make([]string, 0, len(got))
		for _, s := range got {
			all = append(all, s...)
		}
		// pkg/ would swamp a slice on its own, so the split descends into it
		assert.NotContains(t, all, "pkg/")
		assert.Subset(t, all, []string{"pkg/a/", "pkg/b/", "pkg/c/", "pkg/d/"})
	})

	t.Run("hidden_and_vendor_skipped", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "pkg/a.go", ".github/workflows/ci.yml", "node_modules/x/y.js")
		got := codeSlices(t.Context(), dir)
		all := make([]string, 0, len(got))
		for _, s := range got {
			all = append(all, s...)
		}
		assert.NotContains(t, all, ".github/")
		assert.NotContains(t, all, "node_modules/")
	})

	t.Run("bare_directory", func(t *testing.T) {
		assert.Equal(t, [][]string{{"."}}, codeSlices(t.Context(), t.TempDir()))
	})
}

func TestAgentCount(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		files int
		want  int
	}{
		{"tiny_one_pass", 10, 1},
		{"just_under_small", smallRepo - 1, 1},
		{"medium_two", smallRepo, 2},
		{"large_three", mediumRepo, 3},
		{"huge_four", largeRepo, 4},
		{"caps_at_four", largeRepo * 100, maxCodeAgents},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, agentCount(tc.files))
		})
	}
}

func TestBuildTasks(t *testing.T) {
	t.Parallel()

	t.Run("full_list_with_build_first", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "pkg/a.go", "cmd/b.go")
		got := surveyTasks(t.Context(), dir)
		require.Len(t, got, 2) // one build survey plus one codebase slice

		assert.Equal(t, buildLabel, got[0].label)
		for _, want := range []string{"Makefile", ".github/workflows", "CONTRIBUTING.md"} {
			assert.Contains(t, got[0].text, want)
		}
		for _, want := range []string{"pkg/", "cmd/"} {
			assert.Contains(t, got[1].text, want)
		}
		assert.NotContains(t, got[1].text, "Makefile") // the division is visible in what each reads
	})

	t.Run("single_slice_instruction", func(t *testing.T) {
		got := codeTask([]string{"pkg/", "main.go"})
		assert.Contains(t, got, "pkg/, main.go")
		assert.Contains(t, got, "Stay inside those paths")
		assert.True(t, strings.HasSuffix(got, summaryTail))
	})
}

func TestGroup(t *testing.T) {
	t.Parallel()

	files := []string{"main.go", "go.mod", "pkg/a/x.go", "pkg/a/y.go", "pkg/b/z.go", "pkg/top.go"}

	t.Run("top_level", func(t *testing.T) {
		got := group(files, "")
		assert.Equal(t, []unit{
			{path: "pkg", count: 4},
			{count: 2}, // main.go and go.mod as one loose unit
		}, got)
	})

	t.Run("one_level_down", func(t *testing.T) {
		got := group(files, "pkg/")
		assert.Equal(t, []unit{
			{path: "pkg/a", count: 2},
			{path: "pkg/b", count: 1},
			{dir: "pkg", count: 1},
		}, got)
	})

	t.Run("unknown_prefix", func(t *testing.T) {
		assert.Empty(t, group(files, "nope/"))
	})
}

func TestUnitLabel(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "pkg/agent/", unit{path: "pkg/agent"}.label())
	assert.Equal(t, "the files at the repository root", unit{}.label())
	assert.Equal(t, "the files directly in pkg/", unit{dir: "pkg"}.label())
}

func TestSliceUnits(t *testing.T) {
	t.Parallel()

	// wide spreads count files across dir's children.
	wide := func(dir string, kids []string, count int) []string {
		out := make([]string, 0, count*len(kids))
		for _, k := range kids {
			for i := range count {
				out = append(out, dir+"/"+k+"/f"+strconv.Itoa(i)+".go")
			}
		}
		return out
	}

	t.Run("balanced_tree_stays_shallow", func(t *testing.T) {
		files := append(wide("pkg", []string{"a"}, 10), wide("cmd", []string{"b"}, 10)...)
		got := sliceUnits(files, 2)
		assert.Equal(t, []unit{{path: "cmd", count: 10}, {path: "pkg", count: 10}}, got)
	})

	t.Run("dominant_dir_expands", func(t *testing.T) {
		files := append(wide("pkg", []string{"a", "b", "c"}, 20), "docs/x.md")
		got := sliceUnits(files, 2)
		paths := make([]string, 0, len(got))
		for _, u := range got {
			paths = append(paths, u.path)
		}
		assert.NotContains(t, paths, "pkg")
		assert.Subset(t, paths, []string{"pkg/a", "pkg/b", "pkg/c", "docs"})
	})

	t.Run("single_child_is_not_split", func(t *testing.T) {
		// descending would just rename pkg/ to pkg/only/, so the expansion stops
		got := sliceUnits(wide("pkg", []string{"only"}, 30), 2)
		assert.Equal(t, []unit{{path: "pkg", count: 30}}, got)
	})

	t.Run("empty", func(t *testing.T) {
		assert.Empty(t, sliceUnits(nil, 2))
	})
}

func TestSurveyable(t *testing.T) {
	t.Parallel()

	got := surveyable([]string{
		"main.go", "README.md", "README", "AGENTS.md", "Makefile", "LICENSE", "go.sum",
		".github/workflows/ci.yml", "node_modules/x/y.js", "pkg/a.go", "docs/Makefile",
	})
	assert.Equal(t, []string{"main.go", "pkg/a.go", "docs/Makefile"}, got)
}

func TestLargest(t *testing.T) {
	t.Parallel()

	assert.Equal(t, -1, largest(nil))
	assert.Equal(t, 1, largest([]unit{{count: 2}, {count: 9}, {count: 9}})) // first of a tie
}

func TestGitLsFiles(t *testing.T) {
	t.Parallel()

	// paths arrive cwd-relative with .gitignore honoured, so ignored trees never
	// inflate a slice
	t.Run("honours_gitignore_relative", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "main.go", "pkg/a.go")
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n"), 0o644))
		writeTree(t, dir, "junk.log")
		gitInit(t, dir)

		assert.ElementsMatch(t, []string{".gitignore", "main.go", "pkg/a.go"}, gitLsFiles(t.Context(), dir))
	})

	// -z framing must keep whitespace as name content: a whole-output trim mangles
	// the first entry's leading run or drops an all-whitespace one
	t.Run("leading_and_trailing_space_kept", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, " y", "z ", "z.txt") // leading space sorts first in the index
		gitInit(t, dir)

		assert.Equal(t, []string{" y", "z ", "z.txt"}, gitLsFiles(t.Context(), dir))
	})

	// text framing quotes non-ASCII names (core.quotePath), so -z must keep them raw
	t.Run("non_ascii_name_unquoted", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "café.go", "z.txt")
		gitInit(t, dir)

		assert.Equal(t, []string{"café.go", "z.txt"}, gitLsFiles(t.Context(), dir))
	})

	// -c lists deleted-but-tracked files; only paths a sub-agent could read count
	t.Run("deleted_tracked_dropped", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "a.go", "gone.go")
		gitInit(t, dir)
		require.NoError(t, os.Remove(filepath.Join(dir, "gone.go")))

		assert.Equal(t, []string{"a.go"}, gitLsFiles(t.Context(), dir))
	})

	t.Run("tracked_symlink_dropped", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "a.go")
		require.NoError(t, os.Symlink("a.go", filepath.Join(dir, "link.go")))
		gitInit(t, dir)

		assert.Equal(t, []string{"a.go"}, gitLsFiles(t.Context(), dir))
	})

	// an unmerged path is listed once per stage, so the survey must not count it twice
	t.Run("unmerged_entries_deduplicated", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "conflict.txt", "z.txt")
		gitInit(t, dir)

		// restage conflict.txt as an unmerged path, the way a conflicted merge leaves it
		sha, err := exec.CommandContext(t.Context(), "git", "-C", dir,
			"hash-object", "-w", "conflict.txt").Output()
		require.NoError(t, err)
		var info strings.Builder
		for _, stage := range []int{1, 2, 3} {
			fmt.Fprintf(&info, "100644 %s %d\tconflict.txt\n", strings.TrimSpace(string(sha)), stage)
		}
		idx := exec.CommandContext(t.Context(), "git", "-C", dir, "update-index", "--index-info")
		idx.Stdin = strings.NewReader(info.String())
		require.NoError(t, idx.Run())

		assert.Equal(t, []string{"conflict.txt", "z.txt"}, gitLsFiles(t.Context(), dir))
	})
}

func TestWalkFiles(t *testing.T) {
	t.Parallel()

	// the fallback agrees with the git path and pkg/tools: links are never listed
	t.Run("symlinks_never_listed", func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, "a.go")
		require.NoError(t, os.Symlink("a.go", filepath.Join(dir, "link.go")))

		assert.Equal(t, []string{"a.go"}, walkFiles(t.Context(), dir))
	})
}
