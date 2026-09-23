package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitLog(t *testing.T) {
	t.Parallel()

	t.Run("lists_newest_first_with_metadata", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{}`)
		assert.False(t, isErr)
		assert.Regexp(t, `[0-9a-f]{7} 2026-09-01 12:02 merge side`, out)
		assert.Contains(t, out, g.m[:7])
		assert.Contains(t, out, "side edit of a.txt")
	})

	t.Run("file_filter_scopes_to_path", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"file":"a.txt"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "side edit of a.txt")
		assert.Contains(t, out, "add files")
	})

	t.Run("follows_rename_to_older_history", func(t *testing.T) {
		g := newLinearRepo(t)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"file":"moved.txt"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "edit moved")
		assert.Contains(t, out, "rename origin")
		assert.Contains(t, out, "add origin") // pre-rename history found
		assert.Contains(t, out, "renames followed: origin.txt -> moved.txt")
	})

	t.Run("old_name_still_finds_creation", func(t *testing.T) {
		g := newLinearRepo(t)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"file":"origin.txt"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "add origin")
		assert.NotContains(t, out, "edit moved")
	})

	t.Run("follow_limit_reports_more", func(t *testing.T) {
		g := newLinearRepo(t)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"file":"moved.txt","limit":1}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "edit moved")
		assert.Contains(t, out, "1 commit shown")
		assert.NotContains(t, out, "renames followed") // the rename commit was cut
	})

	t.Run("empty_scope_says_so", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"file":"ghost.txt"}`)
		assert.False(t, isErr)
		assert.Equal(t, "(no commits touch ghost.txt)", strings.TrimSpace(out))
	})

	t.Run("limit_bounds_output", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"limit":1}`)
		assert.False(t, isErr)
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		require.Len(t, lines, 2) // the commit line plus the more-history note
		assert.Contains(t, lines[0], "merge side")
		assert.Contains(t, lines[1], "1 commit shown")
	})

	t.Run("limit_at_history_end_has_no_note", func(t *testing.T) {
		g := newGitRepo(t) // exactly three commits
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"limit":3}`)
		assert.False(t, isErr)
		assert.NotContains(t, out, "shown") // nothing was cut
	})

	t.Run("unborn_head_errors_cleanly", func(t *testing.T) {
		dir := t.TempDir()
		_, err := git.PlainInit(dir, false)
		require.NoError(t, err)

		out, isErr := runGitTool(t, &gitLogTool{policy: PathPolicy{Cwd: dir}}, `{}`)
		assert.True(t, isErr)
		assert.Contains(t, out, "no commits yet")
	})
}

// newLinearRepo builds master -> c1 adds origin.txt, c2 renames it to
// moved.txt, c3 edits moved.txt: the smallest follow shape.
func newLinearRepo(t *testing.T) *gitRepo {
	t.Helper()

	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	commit := func(msg string, when time.Time) string {
		t.Helper()

		_, err := w.Add(".")
		require.NoError(t, err)
		h, err := w.Commit(msg, &git.CommitOptions{Author: gitAuthor(when)})
		require.NoError(t, err)
		return h.String()
	}
	require.NoError(t, os0Write(dir, "origin.txt", "one\n"))
	commit("add origin", base)
	require.NoError(t, os.Rename(filepath.Join(dir, "origin.txt"), filepath.Join(dir, "moved.txt")))
	commit("rename origin", base.Add(time.Minute))
	require.NoError(t, os0Write(dir, "moved.txt", "one\ntwo\n"))
	commit("edit moved", base.Add(2*time.Minute))
	return &gitRepo{dir: dir}
}
