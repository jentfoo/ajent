package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitLog(t *testing.T) {
	t.Parallel()

	t.Run("lists_newest_first_with_metadata", func(t *testing.T) {
		t.Parallel()
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{}`)
		assert.False(t, isErr)
		assert.Regexp(t, `[0-9a-f]{7} 2026-09-01 12:02 merge side`, out)
		assert.Contains(t, out, g.m[:7])
		assert.Contains(t, out, "side edit of a.txt")
	})

	t.Run("file_filter_scopes_to_path", func(t *testing.T) {
		t.Parallel()
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"file":"a.txt"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "side edit of a.txt")
		assert.Contains(t, out, "add files")
	})

	t.Run("follows_rename_to_older_history", func(t *testing.T) {
		t.Parallel()
		g := newLinearRepo(t)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"file":"moved.txt"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "edit moved")
		assert.Contains(t, out, "rename origin")
		assert.Contains(t, out, "add origin") // pre-rename history found
		assert.Contains(t, out, "renames followed: origin.txt -> moved.txt")
	})

	t.Run("old_name_still_finds_creation", func(t *testing.T) {
		t.Parallel()
		g := newLinearRepo(t)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"file":"origin.txt"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "add origin")
		assert.NotContains(t, out, "edit moved")
	})

	// pairing follows content alone (tools-design.md), so a rename that also
	// changed mode keeps one change and the walk continues under the old name
	t.Run("follows_rename_through_chmod", func(t *testing.T) {
		t.Parallel()
		g, _ := renameChmodRepo(t, 0o644, "echo hi\n", 0o755)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"file":"new.sh"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "rename script")
		assert.Contains(t, out, "add script") // pre-rename history reached
		assert.Contains(t, out, "renames followed: old.sh -> new.sh")
	})

	// edited blobs pair by similarity instead of the exact-hash match above,
	// with modes normalised first so a chmod keeps one change
	t.Run("follows_edited_rename_through_chmod", func(t *testing.T) {
		t.Parallel()
		g, _ := renameChmodRepo(t, 0o644, "echo hi\nmore\n", 0o755)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"file":"new.sh"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "add script")
		assert.Contains(t, out, "renames followed: old.sh -> new.sh")
	})

	t.Run("old_name_passes_rename_chmod", func(t *testing.T) {
		t.Parallel()
		g, _ := renameChmodRepo(t, 0o644, "echo hi\n", 0o755)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"file":"old.sh"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "rename script") // the rename-away no longer ends history
		assert.Contains(t, out, "add script")
	})

	// both readers see one rename at the same commit, so log may never claim
	// creation where show describes a rename
	t.Run("readers_agree_on_rename_chmod", func(t *testing.T) {
		t.Parallel()
		g, h := renameChmodRepo(t, 0o644, "echo hi\nmore\n", 0o755)
		showOut, isErr := runGitTool(t, &gitShowTool{policy: g.policy()}, `{"ref":"`+h+`"}`)
		assert.False(t, isErr)
		assert.Contains(t, showOut, "rename from old.sh")

		logOut, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"file":"new.sh"}`)
		assert.False(t, isErr)
		assert.NotContains(t, logOut, "no commits touch")
		assert.Contains(t, logOut, "add script")
	})

	t.Run("follow_limit_reports_more", func(t *testing.T) {
		t.Parallel()
		g := newLinearRepo(t)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"file":"moved.txt","limit":1}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "edit moved")
		assert.Contains(t, out, "1 commit shown")
		assert.NotContains(t, out, "renames followed") // the rename commit was cut
	})

	t.Run("step_cap_counts_shown_commits", func(t *testing.T) {
		t.Parallel()
		g := newDeepRepo(t, maxLogCommits+10)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"file":"target.txt"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "edit target again")
		assert.Contains(t, out, "edit target")
		assert.Contains(t, out, "... 2 commits shown") // the cap cut before the limit did
	})

	t.Run("step_cap_without_match_stays_partial", func(t *testing.T) {
		t.Parallel()
		g := newDeepRepo(t, maxLogCommits+10)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"file":"ghost.txt","limit":5}`)
		assert.False(t, isErr)
		assert.NotContains(t, out, "no commits touch") // a capped walk may not claim it
		assert.Contains(t, out, "... 0 commits shown")
	})

	t.Run("cap_at_history_end_is_complete", func(t *testing.T) {
		t.Parallel()
		g := newDeepRepo(t, maxLogCommits-4) // the cut lands exactly at history's end
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"file":"ghost.txt"}`)
		assert.False(t, isErr)
		// a capped walk that exhausted history may claim the empty scope after all
		assert.Equal(t, "(no commits touch ghost.txt)", strings.TrimSpace(out))

		out, isErr = runGitTool(t, &gitLogTool{policy: g.policy()}, `{"file":"target.txt"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "add target") // the birth was reached before the cap
		assert.NotContains(t, out, "shown")   // nothing was cut
	})

	t.Run("empty_scope_says_so", func(t *testing.T) {
		t.Parallel()
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"file":"ghost.txt"}`)
		assert.False(t, isErr)
		assert.Equal(t, "(no commits touch ghost.txt)", strings.TrimSpace(out))
	})

	t.Run("limit_bounds_output", func(t *testing.T) {
		t.Parallel()
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"limit":1}`)
		assert.False(t, isErr)
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		require.Len(t, lines, 2) // the commit line plus the more-history note
		assert.Contains(t, lines[0], "merge side")
		assert.Contains(t, lines[1], "1 commit shown")
	})

	t.Run("limit_at_history_end_has_no_note", func(t *testing.T) {
		t.Parallel()
		g := newGitRepo(t) // exactly three commits
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"limit":3}`)
		assert.False(t, isErr)
		assert.NotContains(t, out, "shown") // nothing was cut
	})

	t.Run("unborn_head_errors_cleanly", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		_, err := git.PlainInit(dir, false)
		require.NoError(t, err)

		out, isErr := runGitTool(t, &gitLogTool{policy: PathPolicy{Cwd: dir}}, `{}`)
		assert.True(t, isErr)
		assert.Contains(t, out, "no commits yet")
	})

	t.Run("trailing_separator_file_scope", func(t *testing.T) {
		t.Parallel()
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"file":"./a.txt/"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "side edit of a.txt") // never a false negative
	})

	t.Run("directory_scope_lists_nested_history", func(t *testing.T) {
		t.Parallel()
		g := newDirRepo(t)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"file":"pkg/"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "edit pkg file")
		assert.Contains(t, out, "delete pkg file")
		assert.Contains(t, out, "add pkg") // a deletion never ends a directory walk
		assert.NotContains(t, out, "edit root")
	})

	t.Run("dot_slash_scope_matches_too", func(t *testing.T) {
		t.Parallel()
		g := newDirRepo(t)
		out, isErr := runGitTool(t, &gitLogTool{policy: g.policy()}, `{"file":"./pkg/"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "edit pkg file")
	})
}

// newDeepRepo builds a history of noise+4 commits: two edits of target.txt on
// top, its birth far below them, and identical-tree filler in between. Sizing
// the filler past maxLogCommits forces a step-cap exit; at exactly maxLogCommits
// total commits the walk instead exhausts history there. The filler only needs
// to sit in the ancestry, so commits are written straight through the object
// storer: no worktree round-trip per commit keeps a thousand-commit fixture cheap.
func newDeepRepo(t *testing.T, noise int) *gitRepo {
	t.Helper()

	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)

	store := func(o plumbing.EncodedObject) plumbing.Hash {
		t.Helper()
		h, err := r.Storer.SetEncodedObject(o)
		require.NoError(t, err)
		return h
	}
	blob := func(content string) plumbing.Hash {
		mo := &plumbing.MemoryObject{}
		mo.SetType(plumbing.BlobObject)
		_, err := mo.Write([]byte(content))
		require.NoError(t, err)
		return store(mo)
	}
	tree := func(target plumbing.Hash, withTarget bool) plumbing.Hash {
		entries := []object.TreeEntry{
			{Name: "base.txt", Mode: filemode.Regular, Hash: blob("one\n")},
		}
		if withTarget {
			entries = append(entries, object.TreeEntry{Name: "target.txt", Mode: filemode.Regular, Hash: target})
		}
		tr := &object.Tree{Entries: entries}
		mo := &plumbing.MemoryObject{}
		require.NoError(t, tr.Encode(mo))
		return store(mo)
	}
	commit := func(parent, treeHash plumbing.Hash, msg string, when time.Time) plumbing.Hash {
		c := &object.Commit{Author: *gitAuthor(when), Committer: *gitAuthor(when),
			Message: msg, TreeHash: treeHash}
		if parent != plumbing.ZeroHash {
			c.ParentHashes = []plumbing.Hash{parent}
		}
		mo := &plumbing.MemoryObject{}
		require.NoError(t, c.Encode(mo))
		return store(mo)
	}

	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	targetV2 := blob("one\ntwo\n")
	targetV3 := blob("one\ntwo\nthree\n")
	birthTree := tree(plumbing.ZeroHash, false) // target.txt not yet added
	noiseTree := tree(blob("one\n"), true)      // identical trees: filler touches nothing

	h := commit(plumbing.ZeroHash, birthTree, "add base", base)
	h = commit(h, noiseTree, "add target", base.Add(time.Minute))
	for i := range noise {
		h = commit(h, noiseTree, fmt.Sprintf("edit base %d", i), base.Add(time.Duration(2+i)*time.Minute))
	}
	h = commit(h, tree(targetV2, true), "edit target", base.Add((maxLogCommits+12)*time.Minute))
	h = commit(h, tree(targetV3, true), "edit target again", base.Add((maxLogCommits+13)*time.Minute))
	require.NoError(t, r.Storer.SetReference(plumbing.NewHashReference(plumbing.Master, h)))
	return &gitRepo{dir: dir}
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

// newDirRepo builds master -> c1 adds pkg/a.txt and root.txt, c2 edits
// pkg/a.txt, c3 deletes pkg/a.txt, c4 edits root.txt: the smallest directory
// scope shape, where one file's deletion must not end the walk.
func newDirRepo(t *testing.T) *gitRepo {
	t.Helper()

	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	commit := func(msg string, when time.Time) string {
		t.Helper()

		require.NoError(t, w.AddWithOptions(&git.AddOptions{All: true})) // stage deletions too
		h, err := w.Commit(msg, &git.CommitOptions{Author: gitAuthor(when)})
		require.NoError(t, err)
		return h.String()
	}
	require.NoError(t, os0Write(dir, "pkg/a.txt", "one\n"))
	require.NoError(t, os0Write(dir, "root.txt", "r1\n"))
	commit("add pkg", base)
	require.NoError(t, os0Write(dir, "pkg/a.txt", "one\ntwo\n"))
	commit("edit pkg file", base.Add(time.Minute))
	require.NoError(t, os.Remove(filepath.Join(dir, "pkg", "a.txt")))
	commit("delete pkg file", base.Add(2*time.Minute))
	require.NoError(t, os0Write(dir, "root.txt", "r2\n"))
	commit("edit root", base.Add(3*time.Minute))
	return &gitRepo{dir: dir}
}
