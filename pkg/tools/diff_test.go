package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
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
		assert.Contains(t, out, "diff --git a/sub b/sub")
		assert.Contains(t, out, "new file mode 160000")
		assert.Contains(t, out, "+Subproject commit "+g.child1)

		out, isErr = runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.bump+`","to":"`+g.pre+`"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "deleted file mode 160000")
		assert.Contains(t, out, "-Subproject commit "+g.child2)
	})

	t.Run("submodule_scope_filters_note", func(t *testing.T) {
		g := newSubmoduleRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.pre+`","to":"`+g.bump+`","file":"sub"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "+Subproject commit "+g.child2)

		out, isErr = runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.pre+`","to":"`+g.bump+`","file":"a.txt"}`)
		assert.False(t, isErr)
		assert.NotContains(t, out, "Subproject")
		assert.Contains(t, out, "(no changes)")
	})

	// the todo's headline case: a clean bump committed since base
	t.Run("worktree_names_committed_bump", func(t *testing.T) {
		g := newSubmoduleRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.base+`","to":"worktree"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "index "+shortHash(plumbing.NewHash(g.child1))+".."+
			shortHash(plumbing.NewHash(g.child2))+" 160000")
		assert.Contains(t, out, "-Subproject commit "+g.child1)
		assert.Contains(t, out, "+Subproject commit "+g.child2)
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
		assert.Contains(t, out, "-Subproject commit "+g.child2)
		assert.Contains(t, out, "+Subproject commit "+g.child1)
	})

	// a gitlink added since base with no checkout on disk still renders
	t.Run("worktree_names_submodule_added", func(t *testing.T) {
		g := newSubmoduleRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.pre+`","to":"worktree"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "new file mode 160000")
		assert.Contains(t, out, "+Subproject commit "+g.child2)
	})

	// index-only staging with no checkout and no .gitmodules: status reads
	// add-then-delete, which netted to nothing before the index pointer counted
	t.Run("worktree_names_staged_add_no_checkout", func(t *testing.T) {
		g := newSubmoduleRepo(t)
		g.stageGitlink(t, "fresh", g.child2)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.pre+`","to":"worktree"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "+Subproject commit "+g.child2)
		assert.NotContains(t, out, "(no changes)")
	})

	// a real on-disk checkout with .gitmodules: the moved pointer renders the
	// checkout's commit, where go-git's status path agrees with git
	t.Run("worktree_names_moved_checkout", func(t *testing.T) {
		g := newCheckoutRepo(t)
		g.checkout(t, g.first)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()}, `{"to":"worktree"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "index "+shortHash(plumbing.NewHash(g.second))+".."+
			shortHash(plumbing.NewHash(g.first))+" 160000")
		assert.Contains(t, out, "-Subproject commit "+g.second)
		assert.Contains(t, out, "+Subproject commit "+g.first)
	})

	// .gitmodules gone from the work tree: go-git's status turns blind, the
	// checkout-resolved pointer still renders the move and the contents never
	// count as untracked files
	t.Run("worktree_undeclared_checkout", func(t *testing.T) {
		g := newCheckoutRepo(t)
		g.checkout(t, g.first)
		require.NoError(t, os.Remove(filepath.Join(g.dir, ".gitmodules")))
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()}, `{"to":"worktree"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "+Subproject commit "+g.first)
		assert.Contains(t, out, "deleted file mode 100644") // the .gitmodules deletion
		assert.NotContains(t, out, "untracked file")
	})

	// dirty contents move no pointer: named only by the committed commit,
	// matching the pointer-only comparison (git would suffix -dirty)
	t.Run("worktree_dirty_contents_silent", func(t *testing.T) {
		g := newCheckoutRepo(t)
		require.NoError(t, os0Write(filepath.Join(g.dir, "sub"), "lib.txt", "dirty\n"))
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()}, `{"to":"worktree"}`)
		assert.False(t, isErr)
		assert.NotContains(t, out, "Subproject")
		assert.Contains(t, out, "(no changes)")
	})

	// git rm --cached with the checkout still present: git reads the staged
	// deletion even though the directory remains, and so must the tool
	t.Run("worktree_staged_deletion_renders", func(t *testing.T) {
		g := newCheckoutRepo(t)
		gitRun(t, g.dir, "rm", "-q", "--cached", "sub")
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()}, `{"to":"worktree"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "diff --git a/sub b/sub")
		assert.Contains(t, out, "deleted file mode 160000")
		assert.Contains(t, out, "-Subproject commit "+g.second)

		out, isErr = runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"to":"worktree","submodule":"log"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "Submodule sub "+
			shortHash(plumbing.NewHash(g.second))+"...0000000 (submodule deleted)")
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

// TestGitDiffSubmoduleBlocksBounded runs in the sequential phase with the
// narrowed limit: Subproject hunks are ordinary blocks, so many submodule
// changes must spill and truncate like any other content.
func TestGitDiffSubmoduleBlocksBounded(t *testing.T) {
	applyTestLimits(t, Limits{Git: Limit{Lines: 3}})

	dir, from, to := newSubmoduleSweepRepo(t, 6)
	out, isErr := runGitTool(t, &gitDiffTool{policy: PathPolicy{Cwd: dir}},
		`{"from":"`+from+`","to":"`+to+`"}`)
	assert.False(t, isErr)
	assert.Contains(t, out, "diff --git a/sub0 b/sub0") // one hunk fills the bound
	assert.NotContains(t, out, "a/sub1 b/sub1")
	assert.Contains(t, out, "5 more changed paths not shown")
	assert.Contains(t, out, "truncated:")
	assert.Contains(t, out, "full output in @") // the spill holds every hunk
}

// TestGitDiffSubmoduleStyles covers git's --submodule rendering forms and
// their fallbacks, resolved through a real checkout.
func TestGitDiffSubmoduleStyles(t *testing.T) {
	t.Parallel()

	t.Run("bad_value_rejected", func(t *testing.T) {
		g := newSubmoduleRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()}, `{"submodule":"bogus"}`)
		assert.True(t, isErr)
		assert.Contains(t, out, "submodule must be short, log or diff")
	})

	// no checkout to resolve child commits: git's uninitialized wording
	t.Run("log_without_checkout_falls_back", func(t *testing.T) {
		g := newSubmoduleRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.base+`","to":"`+g.bump+`","submodule":"log"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "Submodule sub "+
			shortHash(plumbing.NewHash(g.child1))+"..."+shortHash(plumbing.NewHash(g.child2))+
			" (commits not present)")
	})

	t.Run("log_add_new_submodule", func(t *testing.T) {
		g := newCheckoutRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.pre+`","to":"`+g.base+`","submodule":"log"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "Submodule sub 0000000..."+
			shortHash(plumbing.NewHash(g.second))+" (new submodule)")
	})

	// moving back to the child's first commit is a rewind, listed with "<"
	t.Run("log_rewind_subjects", func(t *testing.T) {
		g := newCheckoutRepo(t)
		g.checkout(t, g.first)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"to":"worktree","submodule":"log"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "Submodule sub "+
			shortHash(plumbing.NewHash(g.second))+".."+shortHash(plumbing.NewHash(g.first))+
			" (rewind):")
		assert.Contains(t, out, "  < child two")
		assert.NotContains(t, out, "  > ")
	})

	// a new child commit staged into the index is a forward move, listed ">"
	t.Run("log_forward_subjects", func(t *testing.T) {
		g := newCheckoutRepo(t)
		sub := filepath.Join(g.dir, "sub")
		gitRun(t, sub, "-c", "user.email=t@t", "-c", "user.name=t",
			"commit", "-qm", "child three", "--allow-empty")
		gitRun(t, g.dir, "add", "sub")
		cr, err := git.PlainOpen(sub)
		require.NoError(t, err)
		ref, err := cr.Head()
		require.NoError(t, err)

		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"to":"worktree","submodule":"log"}`)
		assert.False(t, isErr)
		// a fast-forward still prints git's two dots, no marker
		assert.Contains(t, out, "Submodule sub "+shortHash(plumbing.NewHash(g.second))+
			".."+shortHash(ref.Hash())+":")
		assert.Contains(t, out, "  > child three")
	})

	t.Run("diff_form_renders_child_patch", func(t *testing.T) {
		g := newCheckoutRepo(t)
		g.checkout(t, g.first)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"to":"worktree","submodule":"diff"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "Submodule sub "+
			shortHash(plumbing.NewHash(g.second))+".."+shortHash(plumbing.NewHash(g.first))+
			" (rewind):")
		assert.Contains(t, out, "diff --git a/sub/lib.txt b/sub/lib.txt")
		assert.Contains(t, out, "-two")
		assert.Contains(t, out, "+one")
	})

	t.Run("diff_add_whole_child", func(t *testing.T) {
		g := newCheckoutRepo(t)
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.pre+`","to":"`+g.base+`","submodule":"diff"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "(new submodule)")
		assert.Contains(t, out, "diff --git a/sub/lib.txt b/sub/lib.txt")
		assert.Contains(t, out, "+two")
	})
}

// TestGitDiffSubmoduleWalkCap narrows the pointer-walk bound, so it runs in
// the sequential phase rather than parallel with the style readers: a capped
// walk must claim no relation it did not finish exploring, and the listing
// must say it stopped.
func TestGitDiffSubmoduleWalkCap(t *testing.T) {
	orig := gitSubmoduleWalkMax
	t.Cleanup(func() { gitSubmoduleWalkMax = orig })
	gitSubmoduleWalkMax = 2

	g := newCheckoutRepo(t)
	g.checkout(t, g.first) // second..first meets after three visits
	out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
		`{"to":"worktree","submodule":"log"}`)
	assert.False(t, isErr)
	assert.Contains(t, out, "Submodule sub "+
		shortHash(plumbing.NewHash(g.second))+"..."+shortHash(plumbing.NewHash(g.first))+":")
	assert.Contains(t, out, "more commits not walked")
	assert.NotContains(t, out, "(rewind)")
}
