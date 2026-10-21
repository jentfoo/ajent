package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitRun executes one git command in dir, skipping the test when the git
// binary is unavailable. Used where go-git has no API, like worktree creation.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	c := exec.CommandContext(t.Context(), "git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	require.NoError(t, err, string(out))
}

// runGitTool executes one git tool call and returns the model text.
func runGitTool(t *testing.T, tl agent.Tool, input string) (string, bool) {
	t.Helper()
	res, err := tl.Execute(t.Context(), callWith([]byte(input)), agent.NewOutput(agent.NopSink{}, "c"))
	require.NoError(t, err)
	return textOf(res), res.IsError
}

func TestGitToolsIgnoreFilters(t *testing.T) {
	g := newGitRepo(t)
	g.write(t, ".gitattributes", "*.txt diff=evil filter=evil\n")
	g.stage(t) // staged, so the worktree diff renders it as an addition

	t.Run("status", func(t *testing.T) {
		t.Setenv("PATH", "")
		out, isErr := runGitTool(t, &gitStatusTool{policy: g.policy()}, `{}`)
		assert.False(t, isErr)
		assert.Contains(t, out, ".gitattributes")
	})

	t.Run("worktree_diff", func(t *testing.T) {
		t.Setenv("PATH", "")
		out, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()}, `{"to":"worktree"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, ".gitattributes")
	})

	_, statErr := os.Stat(g.dir + "/sideEffect.txt")
	assert.True(t, os.IsNotExist(statErr)) // the filter command never ran
}

func TestGitToolsOutsideRepo(t *testing.T) {
	t.Parallel()

	_, policy := newSearchEnv(t)
	out, isErr := runGitTool(t, &gitStatusTool{policy: policy}, `{}`)
	assert.True(t, isErr)
	assert.Contains(t, out, "not a git repository")
}

func TestGitToolsCorruptTreeErrors(t *testing.T) {
	t.Parallel()

	g := newGitRepo(t)
	r, err := git.PlainOpen(g.dir)
	require.NoError(t, err)
	c, err := r.CommitObject(plumbing.NewHash(g.b))
	require.NoError(t, err)
	removeLoose(t, g.dir, c.TreeHash.String())

	t.Run("show", func(t *testing.T) {
		out, isErr := runGitTool(t, &gitShowTool{policy: g.policy()}, `{"ref":"`+g.b+`"}`)
		assert.True(t, isErr)
		assert.NotContains(t, out, "+one\n+two") // never renders the tree as empty
	})

	t.Run("diff_range", func(t *testing.T) {
		_, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()},
			`{"from":"`+g.a+`","to":"`+g.b+`"}`)
		assert.True(t, isErr)
	})

	t.Run("diff_worktree", func(t *testing.T) {
		_, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()}, `{"to":"worktree"}`)
		assert.True(t, isErr)
	})
}

// TestGitStatusPartialClone runs git_status against a real blob-filtered
// clone: trees are present and blobs fetch on demand, so enumerating tracked
// paths must read names only. The sparse checkout keeps dir/nested.txt's blob
// remote, the shape git_status would hard-error on while materialising blobs.
func TestGitStatusPartialClone(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}

	g, _ := nestedGitRepo(t)
	gitRun(t, g.dir, "config", "uploadpack.allowFilter", "true")
	work := filepath.Join(t.TempDir(), "work")
	gitRun(t, t.TempDir(), "clone", "-q", "--filter=blob:none", "--no-checkout",
		"file://"+g.dir, work)
	gitRun(t, work, "sparse-checkout", "init", "--cone")
	gitRun(t, work, "checkout", "-q", "master")
	// go-git cannot read the worktree-config extension sparse checkout writes.
	// Its settings already applied to the checkout, nothing needs them anymore
	_ = exec.CommandContext(t.Context(), "git", "-C", work,
		"config", "--unset", "extensions.worktreeConfig").Run()
	_ = os.Remove(filepath.Join(work, ".git", "config.worktree"))

	wr, err := git.PlainOpen(work)
	require.NoError(t, err)
	_, err = wr.BlobObject(headTreeEntry(t, wr, "dir/nested.txt"))
	require.ErrorIs(t, err, plumbing.ErrObjectNotFound) // the blob stayed remote

	out, isErr := runGitTool(t, &gitStatusTool{policy: PathPolicy{Cwd: work}}, `{}`)
	assert.False(t, isErr)
	assert.Contains(t, out, "(clean)")
}

func TestHeadTreePaths(t *testing.T) {
	t.Parallel()

	_, r := nestedGitRepo(t)

	files, gitlinks, err := headTreePaths(r)
	require.NoError(t, err)
	assert.Equal(t, []string{"a.txt", "bin.dat", "dir/nested.txt"}, files)
	assert.Empty(t, gitlinks)

	// the tree walker must find gitlinks Files() skips, so the tracked set
	// covers submodule paths too
	t.Run("gitlinks_listed", func(t *testing.T) {
		s := newSubmoduleRepo(t)
		sr, err := git.PlainOpen(s.dir)
		require.NoError(t, err)
		files, gitlinks, err := headTreePaths(sr)
		require.NoError(t, err)
		assert.Equal(t, []string{"a.txt"}, files)
		assert.Equal(t, []string{"sub"}, gitlinks)
	})

	// every breakage case damages its own repo, so no case depends on another's
	t.Run("missing_blob_still_listed", func(t *testing.T) {
		g, r := nestedGitRepo(t)
		removeLoose(t, g.dir, headTreeEntry(t, r, "dir/nested.txt").String())

		// names come from trees: a missing blob narrows nothing, the way git
		// answers from trees on a blob-filtered clone
		files, _, err := headTreePaths(r)
		require.NoError(t, err)
		assert.Equal(t, []string{"a.txt", "bin.dat", "dir/nested.txt"}, files)
	})

	t.Run("missing_subtree_errors", func(t *testing.T) {
		g, r := nestedGitRepo(t)
		removeLoose(t, g.dir, headTreeEntry(t, r, "dir").String())

		_, _, err := headTreePaths(r)
		assert.ErrorIs(t, err, plumbing.ErrObjectNotFound)
	})

	t.Run("missing_head_tree_errors", func(t *testing.T) {
		g, r := nestedGitRepo(t)
		c, err := gitCommit(r, "")
		require.NoError(t, err)
		removeLoose(t, g.dir, c.TreeHash.String())

		_, _, err = headTreePaths(r)
		assert.ErrorIs(t, err, plumbing.ErrObjectNotFound)
	})
}

// TestWalkTreePaths exercises the names-only entry walk directly, including
// the shapes the repository fixtures cannot hold.
func TestWalkTreePaths(t *testing.T) {
	t.Parallel()

	// craftTree stores one tree in the fixture's object store and returns it
	craftTree := func(t *testing.T, r *git.Repository, entries []object.TreeEntry) *object.Tree {
		t.Helper()
		crafted := &object.Tree{Entries: entries}
		obj := &plumbing.MemoryObject{}
		require.NoError(t, crafted.Encode(obj))
		h, err := r.Storer.SetEncodedObject(obj)
		require.NoError(t, err)
		tree, err := object.GetTree(r.Storer, h)
		require.NoError(t, err)
		return tree
	}
	collect := func(s storer.EncodedObjectStorer, t *object.Tree) (names []string, err error) {
		err = walkTreePaths(s, t, func(path string, _ object.TreeEntry) error {
			names = append(names, path)
			return nil
		})
		return names, err
	}

	t.Run("raw_names_and_no_blob_reads", func(t *testing.T) {
		g := newGitRepo(t)
		r, err := git.PlainOpen(g.dir)
		require.NoError(t, err)
		// a name go-git's walkers reject but git accepts, over a hash naming
		// no object at all: enumeration must read names only
		tree := craftTree(t, r, []object.TreeEntry{{
			Name: "bad\x01.txt", Mode: filemode.Regular, Hash: plumbing.ZeroHash,
		}})

		names, err := collect(r.Storer, tree)
		require.NoError(t, err)
		assert.Equal(t, []string{"bad\x01.txt"}, names)
	})

	t.Run("nested_paths_carry_prefix", func(t *testing.T) {
		g := newGitRepo(t)
		r, err := git.PlainOpen(g.dir)
		require.NoError(t, err)
		leaf := craftTree(t, r, []object.TreeEntry{{
			Name: "nested.txt", Mode: filemode.Regular, Hash: plumbing.ZeroHash,
		}})
		root := craftTree(t, r, []object.TreeEntry{{
			Name: "dir", Mode: filemode.Dir, Hash: leaf.Hash,
		}})

		names, err := collect(r.Storer, root)
		require.NoError(t, err)
		assert.Equal(t, []string{"dir/nested.txt"}, names)
	})

	t.Run("missing_subtree_errors", func(t *testing.T) {
		g := newGitRepo(t)
		r, err := git.PlainOpen(g.dir)
		require.NoError(t, err)
		tree := craftTree(t, r, []object.TreeEntry{{
			Name: "gone", Mode: filemode.Dir, Hash: plumbing.ZeroHash,
		}})

		_, err = collect(r.Storer, tree)
		assert.ErrorIs(t, err, plumbing.ErrObjectNotFound)
	})

	t.Run("runaway_nesting_capped", func(t *testing.T) {
		// a storer answering every hash with one self-nesting tree, hostile
		// store data no content-addressed repository can really hold
		obj := &plumbing.MemoryObject{}
		require.NoError(t, (&object.Tree{Entries: []object.TreeEntry{{
			Name: "loop", Mode: filemode.Dir, Hash: plumbing.ZeroHash,
		}}}).Encode(obj))
		s := loopStorer{obj: obj}
		tree, err := object.GetTree(s, plumbing.ZeroHash)
		require.NoError(t, err)

		_, err = collect(s, tree)
		assert.ErrorIs(t, err, object.ErrMaxTreeDepth)
	})
}

// loopStorer answers every object request with one fixed self-nesting tree.
type loopStorer struct {
	storer.EncodedObjectStorer
	obj plumbing.EncodedObject
}

func (s loopStorer) EncodedObject(plumbing.ObjectType, plumbing.Hash) (plumbing.EncodedObject, error) {
	return s.obj, nil
}

// TestGitStatusCorruptObjects covers git_status against a damaged object
// store: structural loss is loud, a missing blob narrows nothing.
func TestGitStatusCorruptObjects(t *testing.T) {
	t.Parallel()

	t.Run("missing_head_tree_errors", func(t *testing.T) {
		g, r := nestedGitRepo(t)
		c, err := gitCommit(r, "")
		require.NoError(t, err)
		removeLoose(t, g.dir, c.TreeHash.String())

		out, isErr := runGitTool(t, &gitStatusTool{policy: g.policy()}, `{}`)
		assert.True(t, isErr)
		assert.NotContains(t, out, "(clean)")
	})

	t.Run("missing_subtree_errors_loud", func(t *testing.T) {
		g, r := nestedGitRepo(t)
		removeLoose(t, g.dir, headTreeEntry(t, r, "dir").String())
		g.write(t, "dir/new.txt", "u\n")

		// a truncated tracked set would collapse dir/ into one ?? row
		out, isErr := runGitTool(t, &gitStatusTool{policy: g.policy()}, `{}`)
		assert.True(t, isErr)
		assert.NotContains(t, strings.Split(out, "\n"), "?? dir/")
	})

	t.Run("missing_blob_keeps_tracked_set", func(t *testing.T) {
		g, r := nestedGitRepo(t)
		removeLoose(t, g.dir, headTreeEntry(t, r, "dir/nested.txt").String())
		g.write(t, "dir/new.txt", "u\n")

		out, isErr := runGitTool(t, &gitStatusTool{policy: g.policy()}, `{}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "?? dir/new.txt")
		assert.NotContains(t, strings.Split(out, "\n"), "?? dir/")
	})
}

// TestGitDiffWorktreeMissingBlob covers the content side: rendering a changed
// path needs its base blob, and a missing one is a loud error, never a
// phantom new-file block.
func TestGitDiffWorktreeMissingBlob(t *testing.T) {
	t.Parallel()

	g, r := nestedGitRepo(t)
	removeLoose(t, g.dir, headTreeEntry(t, r, "dir/nested.txt").String())
	g.write(t, "dir/nested.txt", "changed\n")

	_, isErr := runGitTool(t, &gitDiffTool{policy: g.policy()}, `{"to":"worktree"}`)
	assert.True(t, isErr)
}

func TestGitToolsBareRepo(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, err := git.PlainInit(dir, true)
	require.NoError(t, err)

	out, isErr := runGitTool(t, &gitStatusTool{policy: PathPolicy{Cwd: dir}}, `{}`)
	assert.True(t, isErr)
	assert.Contains(t, out, "bare repository")
}

// TestWorktreeCommonDir exercises the commondir resolution alone.
func TestWorktreeCommonDir(t *testing.T) {
	t.Parallel()

	t.Run("absent_file", func(t *testing.T) {
		assert.Empty(t, worktreeCommonDir(t.TempDir()))
	})

	t.Run("empty_file", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os0Write(dir, "commondir", "\n"))
		assert.Empty(t, worktreeCommonDir(dir))
	})

	// git writes the common dir relative to the metadata dir it lives in
	t.Run("relative_target", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os0Write(dir, "commondir", "../..\n"))
		assert.Equal(t, filepath.Join(dir, "..", ".."), worktreeCommonDir(dir))
	})

	t.Run("absolute_target", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os0Write(dir, "commondir", "/repos/main/.git\n"))
		assert.Equal(t, "/repos/main/.git", worktreeCommonDir(dir))
	})
}

// TestGitToolsLinkedWorktree runs the readers against a real `git worktree add`
// checkout: its .git is a pointer to a metadata dir that shares objects and
// refs with the main repository through commondir.
func TestGitToolsLinkedWorktree(t *testing.T) {
	t.Parallel()

	g := newGitRepo(t)
	link := filepath.Join(t.TempDir(), "linked")
	gitRun(t, g.dir, "worktree", "add", link, "side") // checks out the side branch there

	// HEAD, index and refs resolve through the metadata dir without crossing
	// into the main checkout's state
	t.Run("status_reports_worktree_head", func(t *testing.T) {
		out, isErr := runGitTool(t, &gitStatusTool{policy: PathPolicy{Cwd: link}}, `{}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "on branch side")
		assert.Contains(t, out, "(clean)")
	})

	t.Run("main_repo_unaffected", func(t *testing.T) {
		out, isErr := runGitTool(t, &gitStatusTool{policy: g.policy()}, `{}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "on branch master")
	})

	t.Run("log_walks_shared_history", func(t *testing.T) {
		out, isErr := runGitTool(t, &gitLogTool{policy: PathPolicy{Cwd: link}}, `{}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "side edit of a.txt") // the worktree branch's commit
		assert.Contains(t, out, "add files")          // an object only reachable via commondir
	})

	t.Run("show_renders_head_commit", func(t *testing.T) {
		out, isErr := runGitTool(t, &gitShowTool{policy: PathPolicy{Cwd: link}}, `{}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "side edit of a.txt")
		assert.Contains(t, out, "+TWO")
	})

	t.Run("diff_worktree_edits", func(t *testing.T) {
		require.NoError(t, os0Write(link, "a.txt", "one\nTWO edited\nthree\n"))
		out, isErr := runGitTool(t, &gitDiffTool{policy: PathPolicy{Cwd: link}}, `{"to":"worktree"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "+TWO edited") // the edit on the linked work tree
	})

	// git normally writes a relative commondir; the absolute form resolves too
	t.Run("absolute_commondir_resolves", func(t *testing.T) {
		meta, _ := findDotGit(link)
		require.NotEmpty(t, meta)
		require.NoError(t, os0Write(meta, "commondir", filepath.Join(g.dir, ".git")+"\n"))

		out, isErr := runGitTool(t, &gitStatusTool{policy: PathPolicy{Cwd: link}}, `{}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "on branch side")
	})
}

func TestWriteGitFileDiff(t *testing.T) {
	t.Parallel()

	reg := filemode.Regular
	exec := filemode.Executable
	ha := plumbing.NewHash("0123456789abcdef0123456789abcdef01234567")
	hb := plumbing.NewHash("fedcba9876543210fedcba9876543210fedcba98")

	t.Run("mode_only_change", func(t *testing.T) {
		var b strings.Builder
		writeGitFileDiff(&b,
			&gitSide{path: "run.sh", hash: ha, mode: reg, data: "x"},
			&gitSide{path: "run.sh", hash: ha, mode: exec, data: "x"})
		out := b.String()
		assert.Contains(t, out, "old mode 100644")
		assert.Contains(t, out, "new mode 100755")
		assert.NotContains(t, out, "@@") // no content change, no hunks
	})

	t.Run("rename_pair", func(t *testing.T) {
		var b strings.Builder
		writeGitFileDiff(&b,
			&gitSide{path: "old.txt", hash: ha, mode: reg, data: "one\n"},
			&gitSide{path: "new.txt", hash: hb, mode: reg, data: "two\n"})
		out := b.String()
		assert.Contains(t, out, "diff --git a/old.txt b/new.txt")
		assert.Contains(t, out, "rename from old.txt")
		assert.Contains(t, out, "rename to new.txt")
		assert.Contains(t, out, "-one")
		assert.Contains(t, out, "+two")
	})

	t.Run("rename_with_mode_change", func(t *testing.T) {
		var b strings.Builder
		writeGitFileDiff(&b,
			&gitSide{path: "old.sh", hash: ha, mode: reg, data: "echo hi\n"},
			&gitSide{path: "new.sh", hash: ha, mode: exec, data: "echo hi\n"})
		out := b.String()
		// git's header order: mode pair before the rename pair
		assert.Less(t, strings.Index(out, "old mode 100644"), strings.Index(out, "rename from old.sh"))
		assert.Less(t, strings.Index(out, "new mode 100755"), strings.Index(out, "rename to new.sh"))
		assert.NotContains(t, out, "new file mode")
		assert.NotContains(t, out, "deleted file mode")
		assert.NotContains(t, out, "@@") // identical content, no hunks
	})

	t.Run("creation_and_deletion", func(t *testing.T) {
		var b strings.Builder
		writeGitFileDiff(&b, nil, &gitSide{path: "new.txt", hash: hb, mode: reg, data: "a\n"})
		writeGitFileDiff(&b, &gitSide{path: "gone.txt", hash: ha, mode: reg, data: "b\n"}, nil)
		out := b.String()
		assert.Contains(t, out, "new file mode 100644")
		assert.Contains(t, out, "deleted file mode 100644")
		assert.Regexp(t, `index 0000000\.\.[0-9a-f]{7}`, out)
		assert.Regexp(t, `index [0-9a-f]{7}\.\.0000000`, out)
	})

	t.Run("identical_sides_write_nothing", func(t *testing.T) {
		var b strings.Builder
		writeGitFileDiff(&b,
			&gitSide{path: "same.txt", hash: ha, mode: reg, data: "x"},
			&gitSide{path: "same.txt", hash: ha, mode: reg, data: "x"})
		assert.Empty(t, b.String())
	})

	t.Run("worktree_side_has_no_index_line", func(t *testing.T) {
		var b strings.Builder
		writeGitFileDiff(&b,
			&gitSide{path: "a.txt", hash: ha, mode: reg, data: "one\n"},
			&gitSide{path: "a.txt", mode: reg, data: "two\n"}) // hash zero: work tree
		assert.NotContains(t, b.String(), "index ")
	})

	t.Run("oversized_contents_withheld", func(t *testing.T) {
		var b strings.Builder
		writeGitFileDiff(&b,
			&gitSide{path: "big.txt", hash: ha, mode: reg, size: 3 << 20, data: "one\n", cut: true},
			&gitSide{path: "big.txt", hash: hb, mode: reg, size: 3 << 20, data: "two\n"})
		out := b.String()
		assert.Contains(t, out, "diff --git a/big.txt b/big.txt")
		assert.Contains(t, out, "too large to show (3mb)")
		assert.NotContains(t, out, "@@") // never hunks over truncated content
	})
}

func TestDetectGitRenames(t *testing.T) {
	t.Parallel()

	entry := func(name string, mode filemode.FileMode, hash string) object.ChangeEntry {
		return object.ChangeEntry{Name: name,
			TreeEntry: object.TreeEntry{Name: name, Mode: mode, Hash: plumbing.NewHash(hash)}}
	}
	h := "0123456789abcdef0123456789abcdef01234567"
	deleted := &object.Change{From: entry("old.sh", filemode.Regular, h)}
	added := &object.Change{To: entry("new.sh", filemode.Executable, h)}

	t.Run("pairs_across_mode_change", func(t *testing.T) {
		changes, err := detectGitRenames(object.Changes{deleted, added})
		require.NoError(t, err)
		require.Len(t, changes, 1)
		assert.Equal(t, "old.sh", changes[0].From.Name)
		assert.Equal(t, "new.sh", changes[0].To.Name)
		assert.Equal(t, filemode.Regular, changes[0].From.TreeEntry.Mode)
		assert.Equal(t, filemode.Executable, changes[0].To.TreeEntry.Mode)
	})

	t.Run("input_entries_untouched", func(t *testing.T) {
		_, err := detectGitRenames(object.Changes{deleted, added})
		require.NoError(t, err)
		assert.Equal(t, filemode.Regular, deleted.From.TreeEntry.Mode)
		assert.Equal(t, filemode.Executable, added.To.TreeEntry.Mode)
	})
}

func TestGitStatusLines(t *testing.T) {
	t.Parallel()

	st := git.Status{
		"z.txt": {Staging: git.Modified, Worktree: git.Unmodified},
		"a.txt": {Staging: git.Unmodified, Worktree: git.Modified},
		"n.txt": {Staging: git.Untracked, Worktree: git.Untracked},
		"c.txt": {Staging: git.Unmodified, Worktree: git.Unmodified}, // Preload artifact
		"r.txt": {Staging: git.Renamed, Worktree: git.Unmodified, Extra: "old.txt"},
		"d.txt": {Staging: git.Untracked, Worktree: git.Untracked}, // under tracked dir/, no collapse
	}
	lines, staged, unstaged, untracked := gitStatusLines(st, []string{"dir/file.bin"})
	assert.Equal(t, 2, staged)
	assert.Equal(t, 1, unstaged)
	assert.Equal(t, 2, untracked)
	assert.Equal(t, []string{
		" M a.txt",
		"?? d.txt",
		"?? n.txt",
		"R  old.txt -> r.txt",
		"M  z.txt",
	}, lines) // sorted by path, unmodified entry skipped

	t.Run("untracked_dir_collapses", func(t *testing.T) {
		st := git.Status{
			"new/x.txt": {Staging: git.Untracked, Worktree: git.Untracked},
			"new/y.txt": {Staging: git.Untracked, Worktree: git.Untracked},
			"top.txt":   {Staging: git.Untracked, Worktree: git.Untracked},
		}
		lines, _, _, untracked := gitStatusLines(st, []string{"other.bin"})
		assert.Equal(t, 2, untracked)
		assert.Equal(t, []string{"?? new/", "?? top.txt"}, lines)
	})
}

func TestCommitSubject(t *testing.T) {
	t.Parallel()

	c := &object.Commit{Message: "subject\n\nbody\n"}
	assert.Equal(t, "subject", commitSubject(c))
}

// TestGitStatusSubmoduleCheckout covers the shapes a real on-disk checkout
// takes, where go-git's status is only submodule-aware while .gitmodules sits
// in the work tree.
func TestGitStatusSubmoduleCheckout(t *testing.T) {
	t.Parallel()

	t.Run("clean_after_commit", func(t *testing.T) {
		g := newCheckoutRepo(t)
		out, isErr := runGitTool(t, &gitStatusTool{policy: g.policy()}, `{}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "(clean)")
	})

	t.Run("moved_checkout_reports_modified", func(t *testing.T) {
		g := newCheckoutRepo(t)
		g.checkout(t, g.first)
		out, isErr := runGitTool(t, &gitStatusTool{policy: g.policy()}, `{}`)
		assert.False(t, isErr)
		assert.Contains(t, out, " M sub")
		assert.Contains(t, out, "1 unstaged")
	})

	// contents dirt alone moves no pointer, so nothing reports; git would show
	// a lowercase "m" here, a content state the pointer-only engine cannot see
	t.Run("dirty_contents_stay_quiet", func(t *testing.T) {
		g := newCheckoutRepo(t)
		require.NoError(t, os0Write(filepath.Join(g.dir, "sub"), "lib.txt", "dirty\n"))
		out, isErr := runGitTool(t, &gitStatusTool{policy: g.policy()}, `{}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "(clean)")
	})

	// the undeclared shape go-git gets wrong: a checkout without .gitmodules
	// must not read as deleted with its contents loose untracked files
	t.Run("undeclared_checkout_not_untracked", func(t *testing.T) {
		g := newCheckoutRepo(t)
		require.NoError(t, os.Remove(filepath.Join(g.dir, ".gitmodules")))
		out, isErr := runGitTool(t, &gitStatusTool{policy: g.policy()}, `{}`)
		assert.False(t, isErr)
		assert.Contains(t, out, " D .gitmodules")
		assert.NotContains(t, out, " D sub")
		assert.NotContains(t, out, "?? sub")
		assert.Contains(t, out, "1 unstaged") // only the .gitmodules deletion counts
	})

	t.Run("undeclared_moved_still_modified", func(t *testing.T) {
		g := newCheckoutRepo(t)
		g.checkout(t, g.first)
		require.NoError(t, os.Remove(filepath.Join(g.dir, ".gitmodules")))
		out, isErr := runGitTool(t, &gitStatusTool{policy: g.policy()}, `{}`)
		assert.False(t, isErr)
		assert.Contains(t, out, " M sub")
		assert.Contains(t, out, " D .gitmodules")
		assert.NotContains(t, out, "?? sub")
	})
}

func TestGitCheckoutHash(t *testing.T) {
	t.Parallel()

	t.Run("resolves_checked_out_commit", func(t *testing.T) {
		g := newCheckoutRepo(t)
		assert.Equal(t, plumbing.NewHash(g.second), gitCheckoutHash(g.dir, "sub"))
	})

	t.Run("plain_directory_is_zero", func(t *testing.T) {
		g := newSubmoduleRepo(t)
		require.NoError(t, os.MkdirAll(filepath.Join(g.dir, "sub"), 0o755))
		assert.True(t, gitCheckoutHash(g.dir, "sub").IsZero())
	})

	t.Run("missing_path_is_zero", func(t *testing.T) {
		g := newSubmoduleRepo(t)
		assert.True(t, gitCheckoutHash(g.dir, "sub").IsZero())
	})
}

func TestGitPathScope(t *testing.T) {
	t.Parallel()

	f := gitPathScope("pkg/api")
	assert.True(t, f("pkg/api/server.go"))
	assert.True(t, f("pkg/api"))
	assert.False(t, f("pkg/api2/x.go"))          // prefix must end at a segment
	assert.True(t, gitPathScope("")("anything")) // empty scope matches all
}

func TestNormalizeGitPathspec(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"  ", ""},
		{".", ""},
		{"./", ""},
		{"dir/", "dir"},             // trailing separator selects the directory
		{".//dir/sub//", "dir/sub"}, // duplicate separators collapse
		{"./dir/sub/", "dir/sub"},   // leading dot-slash drops
		{"a/./b", "a/b"},            // interior dot segments resolve
		{"pkg/api/../api/git.go", "pkg/api/git.go"},
		{"/abs.txt", "abs.txt"}, // absolute spelling means repo-relative
		{"..", ".."},            // escaping the repo never matches
		{"../sibling.txt", "../sibling.txt"},
	} {
		assert.Equal(t, tc.want, normalizeGitPathspec(tc.in), tc.in)
	}
}

func TestGitDiffOut(t *testing.T) {
	t.Run("empty_scope_reads_as_no_changes", func(t *testing.T) {
		d := newGitDiffOut("", ToolGitDiff)
		defer func() { _ = d.close() }()
		assert.Equal(t, "(no changes)", d.Text(""))
	})

	t.Run("metas_follow_blocks", func(t *testing.T) {
		d := newGitDiffOut("", ToolGitDiff)
		t.Cleanup(func() { _ = d.close() })
		d.Meta("1 untracked file not shown")
		d.Block(nil, &gitSide{path: "new.txt", mode: filemode.Regular, data: "x\n"})
		text := d.Text("")
		assert.Contains(t, text, "diff --git a/new.txt b/new.txt")
		assert.Contains(t, text, "+x")
		assert.Contains(t, text, "1 untracked file not shown")
	})

	t.Run("truncation_spills_and_counts_skipped", func(t *testing.T) {
		applyTestLimits(t, Limits{Git: Limit{Lines: 2}})

		d := newGitDiffOut("", ToolGitDiff)
		defer func() { _ = d.close() }()
		for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
			if d.Full() { // the loop's own stop: nothing past the bound is read
				d.skipped++
				continue
			}
			d.Block(nil, &gitSide{path: name, mode: filemode.Regular, data: strings.Repeat(name+"\n", 3)})
		}
		text := d.Text("page it")
		assert.Contains(t, text, "truncated:")
		assert.Contains(t, text, "2 more changed paths not shown")
		assert.Contains(t, text, "; page it")
		require.NotEmpty(t, d.spill.path)
		spilled, err := os.ReadFile(d.spill.path)
		require.NoError(t, err)
		assert.Contains(t, string(spilled), "diff --git a/a.txt b/a.txt") // full stream survives
	})
}
