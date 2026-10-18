package tools

import (
	"os"
	"path/filepath"
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
