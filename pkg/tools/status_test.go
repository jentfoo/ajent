package tools

import (
	"fmt"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitStatus(t *testing.T) {
	t.Parallel()

	t.Run("clean_tree_reports_branch", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitStatusTool{policy: g.policy()}, `{}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "on branch master")
		assert.Contains(t, out, "(clean)")
	})

	t.Run("counts_and_short_format", func(t *testing.T) {
		g := newGitRepo(t)
		g.write(t, "a.txt", "changed\n")         // unstaged modify…
		g.write(t, "staged.txt", "new staged\n") // …both then staged
		g.stage(t)
		g.write(t, "a.txt", "changed again\n") // unstaged edit on top of the staged one
		g.write(t, "untracked.txt", "wanderer\n")
		out, isErr := runGitTool(t, &gitStatusTool{policy: g.policy()}, `{}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "2 staged")
		assert.Contains(t, out, "1 unstaged")
		assert.Contains(t, out, "1 untracked")
		assert.Contains(t, out, "MM a.txt")
		assert.Contains(t, out, "A  staged.txt")
		assert.Contains(t, out, "?? untracked.txt")
	})

	t.Run("limit_truncates_with_spill", func(t *testing.T) {
		g := newGitRepo(t)
		for i := 0; i < 150; i++ { // root level: no directory collapse, one line each
			g.write(t, fmt.Sprintf("f%03d.txt", i), "x")
		}
		out, isErr := runGitTool(t,
			&gitStatusTool{policy: g.policy(), sessionID: "git-status-test"}, `{}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "150 untracked")
		assert.Contains(t, out, "truncated")
	})

	t.Run("untracked_dir_collapses", func(t *testing.T) {
		g := newGitRepo(t)
		g.write(t, "newdir/deep/x.txt", "x") // whole directory untracked
		g.write(t, "newdir/y.txt", "y")
		g.write(t, "solo.txt", "s")
		out, isErr := runGitTool(t, &gitStatusTool{policy: g.policy()}, `{}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "?? newdir/")
		assert.Contains(t, out, "?? solo.txt")
		assert.NotContains(t, out, "x.txt")
		assert.Contains(t, out, "2 untracked") // collapsed entries count once
	})

	t.Run("mixed_dir_stays_expanded", func(t *testing.T) {
		g := newGitRepo(t)
		g.write(t, "mix/known.txt", "tracked")
		g.stage(t) // commit it so the directory holds a tracked file
		r, err := git.PlainOpen(g.dir)
		require.NoError(t, err)
		w, err := r.Worktree()
		require.NoError(t, err)
		_, err = w.Commit("track mix", &git.CommitOptions{Author: gitAuthor(time.Now())})
		require.NoError(t, err)
		g.write(t, "mix/new.txt", "untracked")
		out, isErr := runGitTool(t, &gitStatusTool{policy: g.policy()}, `{}`)
		assert.False(t, isErr)
		assert.Equal(t, "on branch master\n1 untracked\n?? mix/new.txt", strings.TrimSpace(out))
	})
}
