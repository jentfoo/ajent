package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runGitTool executes one git tool call and returns the model text.
func runGitTool(t *testing.T, tl agent.Tool, input string) (string, bool) {
	t.Helper()
	res, err := tl.Execute(t.Context(), callWith([]byte(input)), nil)
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
	h := c.TreeHash.String()
	require.NoError(t, os.Remove(filepath.Join(g.dir, ".git", "objects", h[:2], h[2:])))

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

func TestGitToolsBareRepo(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, err := git.PlainInit(dir, true)
	require.NoError(t, err)

	out, isErr := runGitTool(t, &gitStatusTool{policy: PathPolicy{Cwd: dir}}, `{}`)
	assert.True(t, isErr)
	assert.Contains(t, out, "bare repository")
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
}

func TestGitStatusLines(t *testing.T) {
	t.Parallel()

	st := git.Status{
		"z.txt": {Staging: git.Modified, Worktree: git.Unmodified},
		"a.txt": {Staging: git.Unmodified, Worktree: git.Modified},
		"n.txt": {Staging: git.Untracked, Worktree: git.Untracked},
		"c.txt": {Staging: git.Unmodified, Worktree: git.Unmodified}, // Preload artifact
		"r.txt": {Staging: git.Renamed, Worktree: git.Unmodified, Extra: "old.txt"},
		"d.txt": {Staging: git.Untracked, Worktree: git.Untracked}, // under tracked dir/ — no collapse
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

func TestGitPathScope(t *testing.T) {
	t.Parallel()

	f := gitPathScope("pkg/api")
	assert.True(t, f("pkg/api/server.go"))
	assert.True(t, f("pkg/api"))
	assert.False(t, f("pkg/api2/x.go"))          // prefix must end at a segment
	assert.True(t, gitPathScope("")("anything")) // empty scope matches all
}

func TestFinishDiff(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "(no changes)", finishDiff("", nil))
	assert.Equal(t, "body", finishDiff("body\n", nil))
	assert.Equal(t, "body\nnote", finishDiff("body", []string{"note"}))
}
