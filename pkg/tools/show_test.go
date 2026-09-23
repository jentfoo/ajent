package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGitShow(t *testing.T) {
	t.Parallel()

	t.Run("metadata_plus_patch", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitShowTool{policy: g.policy()},
			`{"ref":"`+g.b+`"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "commit "+g.b)
		assert.Contains(t, out, "Author: Ajent Test <test@ajent.local>")
		assert.Contains(t, out, "side edit of a.txt")
		assert.Contains(t, out, "-two")
		assert.Contains(t, out, "+TWO")
	})

	t.Run("default_ref_is_head", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitShowTool{policy: g.policy()}, `{}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "merge side")
	})

	t.Run("annotated_tag_resolves", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitShowTool{policy: g.policy()}, `{"ref":"v1"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "side edit of a.txt")
	})

	t.Run("merge_names_first_parent", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitShowTool{policy: g.policy()},
			`{"ref":"`+g.m+`"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "merge commit; showing the diff against first parent "+g.a[:7])
	})

	t.Run("unknown_revision_errors", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitShowTool{policy: g.policy()}, `{"ref":"nope"}`)
		assert.True(t, isErr)
		assert.Contains(t, out, "unknown revision")
	})
}
