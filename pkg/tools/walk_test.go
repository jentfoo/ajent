package tools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// linkTree builds a search root holding one regular file plus in-tree,
// escaping, broken and directory symlinks. Returns the root and the outside
// directory the escape links point into.
func linkTree(t *testing.T) (string, string) {
	t.Helper()

	dir := t.TempDir()
	outside := t.TempDir()
	mkfile(dir, "a.txt", "x")
	mkfile(outside, "secret.txt", "needle")
	require.NoError(t, os.Symlink("a.txt", filepath.Join(dir, "inside.txt")))
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "escape.txt")))
	require.NoError(t, os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "broken.txt")))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "linkdir")))
	return dir, outside
}

func TestAllWalk(t *testing.T) {
	t.Parallel()

	t.Run("regular_files_listed", func(t *testing.T) {
		dir := t.TempDir()
		mkfile(dir, "a.txt", "x")
		mkfile(dir, "sub/b.txt", "y")

		assert.Equal(t, []string{
			filepath.Join(dir, "a.txt"),
			filepath.Join(dir, "sub", "b.txt"),
		}, allWalk(t.Context(), dir))
	})

	t.Run("symlinks_never_listed", func(t *testing.T) {
		dir, _ := linkTree(t)

		assert.Equal(t, []string{filepath.Join(dir, "a.txt")}, allWalk(t.Context(), dir))
	})

	t.Run("single_file_root_listed", func(t *testing.T) {
		dir := t.TempDir()
		mkfile(dir, "a.txt", "x")

		assert.Equal(t, []string{filepath.Join(dir, "a.txt")}, allWalk(t.Context(), filepath.Join(dir, "a.txt")))
	})
}

func TestGitTrackedFiles(t *testing.T) {
	t.Parallel()

	// tracked plus untracked non-ignored paths arrive slash-relative, so callers
	// can consume the names directly or join them onto any root spelling
	t.Run("relative_names_untracked_kept", func(t *testing.T) {
		dir := t.TempDir()
		mkfile(dir, "main.go", "x")
		mkfile(dir, "pkg/a.go", "y")
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n"), 0o644))
		gitInit(t, dir)
		mkfile(dir, "untracked.txt", "z") // written after add -A stays untracked
		mkfile(dir, "junk.log", "l")      // and ignored

		assert.ElementsMatch(t,
			[]string{".gitignore", "main.go", "pkg/a.go", "untracked.txt"},
			GitTrackedFiles(t.Context(), dir))
	})

	t.Run("not_a_repo_empty", func(t *testing.T) {
		dir := t.TempDir()
		mkfile(dir, "a.txt", "x")

		assert.Empty(t, GitTrackedFiles(t.Context(), dir))
	})

	// -z framing must keep whitespace as name content: a whole-output trim
	// mangles the first entry's leading run or drops an all-whitespace one
	t.Run("leading_space_name_kept", func(t *testing.T) {
		dir := t.TempDir()
		mkfile(dir, " y", "x") // leading space, sorts first in the index
		mkfile(dir, "z ", "y") // trailing space, never trimmed even before
		mkfile(dir, "z.txt", "v")
		gitInit(t, dir)

		assert.Equal(t, []string{" y", "z ", "z.txt"}, GitTrackedFiles(t.Context(), dir))
	})

	t.Run("whitespace_only_name_kept", func(t *testing.T) {
		dir := t.TempDir()
		mkfile(dir, " ", "w") // all-whitespace name, sorts first
		mkfile(dir, "z.txt", "v")
		gitInit(t, dir)

		assert.Equal(t, []string{" ", "z.txt"}, GitTrackedFiles(t.Context(), dir))
	})

	// text framing quotes non-ASCII names (core.quotePath), so -z must keep them raw
	t.Run("non_ascii_name_unquoted", func(t *testing.T) {
		dir := t.TempDir()
		mkfile(dir, "café.go", "x")
		mkfile(dir, "z.txt", "y")
		gitInit(t, dir)

		assert.Equal(t, []string{"café.go", "z.txt"}, GitTrackedFiles(t.Context(), dir))
	})

	// -c lists deleted-but-tracked files; only paths a caller could read count
	t.Run("deleted_tracked_dropped", func(t *testing.T) {
		dir := t.TempDir()
		mkfile(dir, "a.go", "x")
		mkfile(dir, "gone.go", "y")
		gitInit(t, dir)
		require.NoError(t, os.Remove(filepath.Join(dir, "gone.go")))

		assert.Equal(t, []string{"a.go"}, GitTrackedFiles(t.Context(), dir))
	})

	t.Run("tracked_symlink_dropped", func(t *testing.T) {
		dir := t.TempDir()
		mkfile(dir, "a.go", "x")
		require.NoError(t, os.Symlink("a.go", filepath.Join(dir, "link.go")))
		gitInit(t, dir)

		assert.Equal(t, []string{"a.go"}, GitTrackedFiles(t.Context(), dir))
	})

	// an unmerged path is listed once per stage, so callers must not double-count it
	t.Run("unmerged_entries_deduplicated", func(t *testing.T) {
		dir := t.TempDir()
		mkfile(dir, "conflict.txt", "x")
		mkfile(dir, "z.txt", "y")
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

		assert.Equal(t, []string{"conflict.txt", "z.txt"}, GitTrackedFiles(t.Context(), dir))
	})
}

func TestRepoFiles(t *testing.T) {
	t.Parallel()

	t.Run("regular_files_listed", func(t *testing.T) {
		dir := t.TempDir()
		mkfile(dir, "a.txt", "x")
		mkfile(dir, "sub/b.txt", "y")

		assert.ElementsMatch(t, []string{
			filepath.Join(dir, "a.txt"),
			filepath.Join(dir, "sub", "b.txt"),
		}, repoFiles(t.Context(), dir))
	})

	// non-git path drops every flavor of link
	t.Run("symlinks_never_listed", func(t *testing.T) {
		dir, _ := linkTree(t)

		assert.Equal(t, []string{filepath.Join(dir, "a.txt")}, repoFiles(t.Context(), dir))
	})

	// a committed link is the escape scenario
	t.Run("git_tracked_symlink_dropped", func(t *testing.T) {
		dir, _ := linkTree(t)
		gitInit(t, dir)

		assert.Equal(t, []string{filepath.Join(dir, "a.txt")}, repoFiles(t.Context(), dir))
	})

	// -z framing must keep whitespace as name content: a whole-output trim
	// mangles the first entry's leading run or drops an all-whitespace one
	t.Run("leading_space_name_kept", func(t *testing.T) {
		dir := t.TempDir()
		mkfile(dir, " y", "x") // leading space, sorts first in the index
		mkfile(dir, "z ", "y") // trailing space, never trimmed even before
		mkfile(dir, "z.txt", "v")
		gitInit(t, dir)

		assert.Equal(t, []string{
			filepath.Join(dir, " y"),
			filepath.Join(dir, "z "),
			filepath.Join(dir, "z.txt"),
		}, repoFiles(t.Context(), dir))
	})

	t.Run("whitespace_only_name_kept", func(t *testing.T) {
		dir := t.TempDir()
		mkfile(dir, " ", "w") // all-whitespace name, sorts first
		mkfile(dir, "z.txt", "v")
		gitInit(t, dir)

		assert.Equal(t, []string{
			filepath.Join(dir, " "),
			filepath.Join(dir, "z.txt"),
		}, repoFiles(t.Context(), dir))
	})
}
