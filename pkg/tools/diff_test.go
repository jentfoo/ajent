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

	// status entries are HEAD-relative, so candidate paths must also come from
	// HEAD's tree or files committed after the base are silently omitted
	t.Run("worktree_since_base", func(t *testing.T) {
		g := newGitRepo(t)
		r, err := git.PlainOpen(g.dir)
		require.NoError(t, err)
		w, err := r.Worktree()
		require.NoError(t, err)

		// a commit on master after base g.a: adds later/new.txt, clean against HEAD
		require.NoError(t, os0Write(g.dir, "later/new.txt", "later\n"))
		_, err = w.Add(".")
		require.NoError(t, err)
		_, err = w.Commit("add later file", &git.CommitOptions{
			Author: gitAuthor(time.Date(2026, 9, 1, 12, 5, 0, 0, time.UTC)),
		})
		require.NoError(t, err)

		fromBase := `{"from":"` + g.a + `","to":"worktree"}`

		t.Run("committed_addition_since_base", func(t *testing.T) {
			out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()}, fromBase)
			assert.False(t, isErr)
			assert.Contains(t, out, "diff --git a/later/new.txt b/later/new.txt")
			assert.Contains(t, out, "new file mode 100644")
			assert.Contains(t, out, "+later")
			assert.Contains(t, out, "+TWO") // committed edit since base still shown
		})

		t.Run("scoped_addition_since_base", func(t *testing.T) {
			out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
				`{"from":"`+g.a+`","to":"worktree","file":"later"}`)
			assert.False(t, isErr)
			assert.Contains(t, out, "later/new.txt")
			assert.NotContains(t, out, "a.txt")
		})

		// deleted on disk after being committed: absent from base and work tree
		t.Run("uncommitted_deletion_of_addition", func(t *testing.T) {
			require.NoError(t, os.Remove(filepath.Join(g.dir, "later", "new.txt")))
			out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()}, fromBase)
			assert.False(t, isErr)
			assert.NotContains(t, out, "later/new.txt")
		})

		t.Run("uncommitted_edit_since_base", func(t *testing.T) {
			g.write(t, "later/new.txt", "later edit\n")
			out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()}, fromBase)
			assert.False(t, isErr)
			assert.Contains(t, out, "+later edit") // work-tree content, not the commit
		})
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

	// committed renames pair even across a chmod, which go-git's detector alone
	// refuses to match
	t.Run("committed_rename_detection", func(t *testing.T) {
		t.Run("chmod_only_keeps_one_block", func(t *testing.T) {
			g, h := renameChmodRepo(t, 0o644, "echo hi\n", 0o755)
			out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()}, `{"from":"`+h+`"}`)
			assert.False(t, isErr)
			assert.Contains(t, out, "rename from old.sh")
			assert.Contains(t, out, "rename to new.sh")
			assert.Contains(t, out, "old mode 100644")
			assert.Contains(t, out, "new mode 100755")
			assert.NotContains(t, out, "new file mode")
			assert.NotContains(t, out, "deleted file mode")
			assert.NotContains(t, out, "@@") // content identical, no hunks
		})

		t.Run("chmod_with_edit_renders_hunks", func(t *testing.T) {
			g, h := renameChmodRepo(t, 0o644, "echo hi\nmore\n", 0o755)
			out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()}, `{"from":"`+h+`"}`)
			assert.False(t, isErr)
			assert.Contains(t, out, "rename from old.sh")
			assert.Contains(t, out, "new mode 100755")
			assert.Contains(t, out, "+more")
			assert.NotContains(t, out, "new file mode")
		})

		// the similarity path also skips executable sides, not just mode changes
		t.Run("executable_rename_edited_pairs", func(t *testing.T) {
			g, h := renameChmodRepo(t, 0o755, "echo hi\nmore\n", 0o755)
			out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()}, `{"from":"`+h+`"}`)
			assert.False(t, isErr)
			assert.Contains(t, out, "rename from old.sh")
			assert.Contains(t, out, "+more")
			assert.NotContains(t, out, "new file mode")
			assert.NotContains(t, out, "old mode") // modes equal, no mode lines
		})
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

	t.Run("file_scope_normalizes_separators", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.a+`","to":"`+g.b+`","file":"./a.txt/"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "+TWO") // never a false negative
	})

	t.Run("oversized_contents_withheld", func(t *testing.T) {
		g := newGitRepo(t)
		g.write(t, "big.txt", strings.Repeat("line\n", 120<<10)) // past gitBlobCeiling
		g.stage(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()}, `{"to":"worktree"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "diff --git a/big.txt b/big.txt")
		assert.Contains(t, out, "too large to show") // named, never rendered
		assert.NotContains(t, out, "+line")
	})

	t.Run("submodule_add_and_delete_named", func(t *testing.T) {
		g := newSubmoduleRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.pre+`","to":"`+g.base+`"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "submodule sub changed")

		out, isErr = runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.bump+`","to":"`+g.pre+`"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "submodule sub changed")
	})

	t.Run("submodule_scope_filters_note", func(t *testing.T) {
		g := newSubmoduleRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.pre+`","to":"`+g.bump+`","file":"sub"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "submodule sub changed")

		out, isErr = runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.pre+`","to":"`+g.bump+`","file":"a.txt"}`)
		assert.False(t, isErr)
		assert.NotContains(t, out, "submodule")
		assert.Contains(t, out, "(no changes)")
	})

	// the todo's headline case: a clean bump committed since base
	t.Run("worktree_names_committed_bump", func(t *testing.T) {
		g := newSubmoduleRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.base+`","to":"worktree"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "submodule sub changed")
	})

	// an unchecked-out pointer identical at base and HEAD is no change
	t.Run("worktree_silent_when_unchanged", func(t *testing.T) {
		g := newSubmoduleRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()}, `{"to":"worktree"}`)
		assert.False(t, isErr)
		assert.NotContains(t, out, "submodule")
		assert.Contains(t, out, "(no changes)")
	})

	t.Run("worktree_names_staged_move", func(t *testing.T) {
		g := newSubmoduleRepo(t)
		g.point(t, g.child1) // index back at child1 while HEAD holds child2
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()}, `{"to":"worktree"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "submodule sub changed")
	})

	// a gitlink added since base with no checkout on disk still gets its note
	t.Run("worktree_names_submodule_added", func(t *testing.T) {
		g := newSubmoduleRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.pre+`","to":"worktree"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "submodule sub changed")
	})

	// a directory replacing a tracked file renders the deletion; its contents
	// stay untracked, never a submodule note
	t.Run("dir_replacement_not_submodule", func(t *testing.T) {
		g := newGitRepo(t)
		require.NoError(t, os.Remove(filepath.Join(g.dir, "bin.dat")))
		require.NoError(t, os0Write(g.dir, "bin.dat/inner.txt", "x\n"))

		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()}, `{"to":"worktree"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "diff --git a/bin.dat b/bin.dat")
		assert.Contains(t, out, "deleted file mode 100644")
		assert.NotContains(t, out, "submodule")
		assert.Contains(t, out, "untracked file not shown")
	})
}

// TestGitDiffWideBoundTruncates narrows the shared git limit, so it runs in the
// sequential phase rather than parallel with every limit reader.
func TestGitDiffWideBoundTruncates(t *testing.T) {
	applyTestLimits(t, Limits{Git: Limit{Lines: 3}})

	g := newGitRepo(t)
	for _, name := range []string{"x1.txt", "x2.txt"} {
		g.write(t, name, "content of "+name+"\n")
	}
	g.stage(t)

	out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()}, `{"to":"worktree"}`)
	assert.False(t, isErr)
	assert.Contains(t, out, "truncated:")
	assert.Contains(t, out, "1 more changed path not shown") // rendering stopped at the bound
	assert.NotContains(t, out, "+content of x2.txt")
}
