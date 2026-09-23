package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitDiff(t *testing.T) {
	t.Parallel()

	t.Run("commit_introduces_patch", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.b+`"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "diff --git a/a.txt b/a.txt")
		assert.Contains(t, out, "-two")
		assert.Contains(t, out, "+TWO")
	})

	t.Run("range_between_refs", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.a+`","to":"`+g.b+`"}`)
		assert.False(t, isErr)
		assert.Regexp(t, `index [0-9a-f]{7}\.\.[0-9a-f]{7} 100644`, out) // blob hashes, not commits
		assert.Contains(t, out, "+TWO")
	})

	t.Run("file_scope_limits_range", func(t *testing.T) {
		g := newGitRepo(t)
		g.write(t, "dir/nested.txt", "nested\n")
		g.stage(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"to":"worktree","file":"dir"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "+nested")
		assert.NotContains(t, out, "a.txt")
	})

	t.Run("file_scope_outside_scope_is_empty", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.a+`","to":"`+g.b+`","file":"other.txt"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "(no changes)")
	})

	t.Run("file_scope_counts_in_scope_untracked", func(t *testing.T) {
		g := newGitRepo(t)
		g.write(t, "a.txt", "edited\n")
		g.write(t, "ghost.txt", "untracked")
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"to":"worktree","file":"a.txt"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "+edited")
		assert.NotContains(t, out, "untracked") // out of scope: not counted, not named
	})

	t.Run("range_shorthand_in_from", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.a+`..`+g.b+`"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "+TWO")
	})

	t.Run("three_dot_range_rejected", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.a+`...`+g.b+`"}`)
		assert.True(t, isErr)
		assert.Contains(t, out, "three-dot")
	})

	t.Run("empty_range_side_rejected", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"..`+g.b+`"}`)
		assert.True(t, isErr)
		assert.Contains(t, out, "each side")
	})

	t.Run("empty_range_means_head_change", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()}, `{}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "merge commit; showing the diff against first parent")
	})

	t.Run("worktree_mode_shows_uncommitted", func(t *testing.T) {
		g := newGitRepo(t)
		g.write(t, "a.txt", "one\nTWO\nTHREE\n") // unstaged edit
		g.write(t, "wanderer.txt", "untracked\n")
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"to":"worktree"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "-three")
		assert.Contains(t, out, "+THREE")
		assert.Contains(t, out, "1 untracked file not shown")
	})

	t.Run("worktree_mode_from_ref", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.a+`","to":"worktree"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "+TWO") // committed since a
	})

	t.Run("worktree_only_in_to", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"worktree"}`)
		assert.True(t, isErr)
		assert.Contains(t, out, `put "worktree" in to`)
	})

	t.Run("binary_delta_named_not_rendered", func(t *testing.T) {
		g := newGitRepo(t)
		g.write(t, "bin.dat", "totally\x00different")
		g.stage(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"to":"worktree"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "Binary files a/bin.dat and b/bin.dat differ")
		assert.NotContains(t, out, "+totally")
	})

	t.Run("staged_rename_renders_delete_and_add", func(t *testing.T) {
		g := newGitRepo(t)
		require.NoError(t, os.Rename(filepath.Join(g.dir, "a.txt"), filepath.Join(g.dir, "renamed.txt")))
		g.stage(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()}, `{"to":"worktree"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "deleted file mode")
		assert.Contains(t, out, "new file mode")
	})

	t.Run("no_changes_reads_as_answer", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.b+`","to":"`+g.m+`"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "(no changes)")
	})

	t.Run("unknown_revision_errors", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"ghost","to":"worktree"}`)
		assert.True(t, isErr)
		assert.Contains(t, out, "unknown revision")
	})
}
