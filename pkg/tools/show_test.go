package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGitShow(t *testing.T) {
	t.Parallel()

	t.Run("submodule_pointer_bump_named", func(t *testing.T) {
		g := newSubmoduleRepo(t)
		out, isErr := runGitTool(t, &gitShowTool{policy: g.policy()}, `{"ref":"`+g.bump+`"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "submodule sub changed")
		assert.NotContains(t, out, "(no changes)")
		assert.NotContains(t, out, "diff --git") // no phantom block for a gitlink
	})

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

	t.Run("rename_chmod_keeps_mode_lines", func(t *testing.T) {
		g, h := renameChmodRepo(t, 0o644, "echo hi\n", 0o755)
		out, isErr := runGitTool(t, &gitShowTool{policy: g.policy()}, `{"ref":"`+h+`"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "commit "+h)
		assert.Contains(t, out, "rename from old.sh")
		assert.Contains(t, out, "new mode 100755")
		assert.NotContains(t, out, "new file mode")
	})

	t.Run("unknown_revision_errors", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitShowTool{policy: g.policy()}, `{"ref":"nope"}`)
		assert.True(t, isErr)
		assert.Contains(t, out, "unknown revision")
	})

	t.Run("file_scope_normalizes_separators", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitShowTool{policy: g.policy()},
			`{"ref":"`+g.b+`","file":"./a.txt/"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "+TWO") // never a false negative
	})

	t.Run("unmatched_scope_keeps_metadata", func(t *testing.T) {
		g := newGitRepo(t)
		out, isErr := runGitTool(t, &gitShowTool{policy: g.policy()},
			`{"ref":"`+g.b+`","file":"missing.txt"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "commit "+g.b) // the header is the answer's anchor
		assert.Contains(t, out, "(no changes)")
	})

	t.Run("header_full_never_claims_no_changes", func(t *testing.T) {
		g := newGitRepo(t)

		testLimitsGate.Lock()
		t.Cleanup(testLimitsGate.Unlock) // unlock runs after the restore below
		orig := GitResultLimit()
		t.Cleanup(func() { ApplyLimits(Limits{Git: orig}) })
		ApplyLimits(Limits{Git: Limit{Lines: 3}}) // the header alone fills the line bound

		// commit a adds two paths, both counted as skipped once full
		out, isErr := runGitTool(t, &gitShowTool{policy: g.policy()}, `{"ref":"`+g.a+`"}`)
		assert.False(t, isErr)
		assert.Contains(t, out, "commit "+g.a) // the kept header prefix survives
		assert.Contains(t, out, "2 more changed paths not shown")
		assert.Contains(t, out, "truncated:")
		assert.NotContains(t, out, "(no changes)")
	})
}
