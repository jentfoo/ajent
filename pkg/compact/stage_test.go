package compact

import (
	"strconv"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubTexts returns the replacement text of each stub, in order.
func stubTexts(stubs []session.Stub) []string {
	out := make([]string, len(stubs))
	for i, s := range stubs {
		out[i] = s.Text
	}
	return out
}

func TestSpanStubs(t *testing.T) {
	t.Parallel()

	// all cases emit over the whole branch unless they are testing the band
	all := func(branch []session.Entry) []session.Stub {
		return spanStubs(branch, 0, len(branch), "/tmp")
	}

	t.Run("failed_stubbed_regardless_of_age", func(t *testing.T) {
		branch := []session.Entry{
			userText("u1", "go"),
			callMsg("a1", "c1", "bash", ""),
			resultMsg("r1", "c1", "boom: no such file\nmore detail", true),
		}
		stubs := all(branch)
		require.Len(t, stubs, 1)
		assert.Contains(t, stubs[0].Text, "bash failed")
		assert.Contains(t, stubs[0].Text, "boom: no such file")
		assert.NotContains(t, stubs[0].Text, "more detail", "only the first line survives")
	})

	t.Run("newer_success_supersedes", func(t *testing.T) {
		for _, tc := range []struct{ first, next, marker string }{
			{"read", "read", supersededReadMarker},
			{"edit", "write", supersededEditMarker},
		} {
			branch := []session.Entry{
				callMsg("a1", "c1", tc.first, "/tmp/f.go"),
				resultMsg("r1", "c1", "old body", false),
				callMsg("a2", "c2", tc.next, "/tmp/f.go"),
				resultMsg("r2", "c2", "new body", false),
			}
			stubs := all(branch)
			require.Len(t, stubs, 1)
			assert.Equal(t, "c1", stubs[0].CallID)
			assert.Equal(t, tc.marker, stubs[0].Text)
		}
	})

	t.Run("edit_after_write_kept", func(t *testing.T) {
		branch := []session.Entry{
			callMsg("a1", "c1", "write", "/tmp/f.go"),
			resultMsg("r1", "c1", "written", false),
			callMsg("a2", "c2", "edit", "/tmp/f.go"),
			resultMsg("r2", "c2", "edited", false),
		}
		assert.Empty(t, all(branch))
	})

	t.Run("failed_read_keeps_earlier_copy", func(t *testing.T) {
		// a denied re-read proves nothing about file state, so it must not
		// demote the only successful read of that path
		branch := []session.Entry{
			callMsg("a1", "c1", "read", "/tmp/f.go"),
			resultMsg("r1", "c1", "good body", false),
			callMsg("a2", "c2", "read", "/tmp/f.go"),
			resultMsg("r2", "c2", "denied: path is protected", true),
		}
		stubs := all(branch)
		require.Len(t, stubs, 1)
		assert.Equal(t, "c2", stubs[0].CallID)
	})

	// a failed newest read used to mark every earlier read of the path at once
	t.Run("failed_last_read_keeps_all", func(t *testing.T) {
		branch := []session.Entry{
			callMsg("a1", "c1", "read", "/tmp/f.go"),
			resultMsg("r1", "c1", "first body", false),
			callMsg("a2", "c2", "read", "/tmp/f.go"),
			resultMsg("r2", "c2", "second body", false),
			callMsg("a3", "c3", "read", "/tmp/f.go"),
			resultMsg("r3", "c3", "denied", true),
		}
		stubs := all(branch)
		require.Len(t, stubs, 2)
		assert.Equal(t, "c1", stubs[0].CallID) // superseded by c2's success only
		assert.Equal(t, supersededReadMarker, stubs[0].Text)
		assert.Equal(t, "c3", stubs[1].CallID) // the failure alone carries a failed stub
	})

	t.Run("success_after_failure_supersedes", func(t *testing.T) {
		branch := []session.Entry{
			callMsg("a1", "c1", "read", "/tmp/f.go"),
			resultMsg("r1", "c1", "first body", false),
			callMsg("a2", "c2", "read", "/tmp/f.go"),
			resultMsg("r2", "c2", "boom", true),
			callMsg("a3", "c3", "read", "/tmp/f.go"),
			resultMsg("r3", "c3", "newest body", false),
		}
		stubs := all(branch)
		require.Len(t, stubs, 2)
		assert.Equal(t, "c1", stubs[0].CallID)
		assert.Equal(t, supersededReadMarker, stubs[0].Text)
	})

	t.Run("failed_write_keeps_edit", func(t *testing.T) {
		branch := []session.Entry{
			callMsg("a1", "c1", "edit", "/tmp/f.go"),
			resultMsg("r1", "c1", "edited", false),
			callMsg("a2", "c2", "write", "/tmp/f.go"),
			resultMsg("r2", "c2", "denied: path is protected", true),
		}
		stubs := all(branch)
		require.Len(t, stubs, 1)
		assert.Equal(t, "c2", stubs[0].CallID)
	})

	t.Run("later_successful_write_supersedes", func(t *testing.T) {
		branch := []session.Entry{
			callMsg("a1", "c1", "edit", "/tmp/f.go"),
			resultMsg("r1", "c1", "edited", false),
			callMsg("a2", "c2", "write", "/tmp/f.go"),
			resultMsg("r2", "c2", "boom", true),
			callMsg("a3", "c3", "write", "/tmp/f.go"),
			resultMsg("r3", "c3", "written", false),
		}
		stubs := all(branch)
		require.Len(t, stubs, 2)
		assert.Equal(t, "c1", stubs[0].CallID)
		assert.Equal(t, supersededEditMarker, stubs[0].Text)
	})

	t.Run("errored_duplicate_classifies_failed", func(t *testing.T) {
		body := "boom: no such file\nmore detail"
		branch := []session.Entry{
			callMsg("a1", "c1", "bash", ""),
			resultMsg("r1", "c1", body, true),
			callMsg("a2", "c2", "bash", ""),
			resultMsg("r2", "c2", body, true),
		}
		for _, s := range all(branch) {
			assert.Contains(t, s.Text, "failed")
		}
	})

	// unreachable on real paths, but absence of success must never count as success
	t.Run("unanswered_call_counts_absent", func(t *testing.T) {
		branch := []session.Entry{
			callMsg("a1", "c1", "read", "/tmp/f.go"),
			resultMsg("r1", "c1", "good body", false),
			callMsg("a2", "c2", "read", "/tmp/f.go"),
		}
		assert.Empty(t, all(branch))
	})

	t.Run("duplicate_output_stubbed", func(t *testing.T) {
		body := strings.Repeat("same bytes ", 20)
		branch := []session.Entry{
			callMsg("a1", "c1", "bash", ""),
			resultMsg("r1", "c1", body, false),
			callMsg("a2", "c2", "bash", ""),
			resultMsg("r2", "c2", body, false),
		}
		stubs := all(branch)
		require.Len(t, stubs, 1)
		assert.Equal(t, "c2", stubs[0].CallID)
		assert.Contains(t, stubs[0].Text, "identical")
	})

	t.Run("distinct_targets_not_collapsed", func(t *testing.T) {
		body := strings.Repeat("same bytes ", 20)
		branch := []session.Entry{
			callMsg("a1", "c1", "read", "/tmp/one.go"),
			resultMsg("r1", "c1", body, false),
			callMsg("a2", "c2", "read", "/tmp/two.go"),
			resultMsg("r2", "c2", body, false),
		}
		assert.Empty(t, all(branch))
	})

	t.Run("system_role_message_skipped", func(t *testing.T) {
		branch := []session.Entry{
			msg("s1", llm.Message{Role: llm.RoleSystem, Content: llm.BlockList{
				llm.ToolResultBlock{CallID: "c1", Content: llm.BlockList{llm.TextBlock{Text: "x"}}},
			}}),
		}
		assert.Empty(t, all(branch))
	})

	// band entries stay visible to the rules but are never replaced
	t.Run("band_not_emitted_but_detected", func(t *testing.T) {
		body := strings.Repeat("file body ", 20)
		branch := []session.Entry{
			callMsg("a1", "c1", "read", "/tmp/f.go"),
			resultMsg("r1", "c1", body, false),
			callMsg("a2", "c2", "read", "/tmp/f.go"),
			resultMsg("r2", "c2", body, false),
		}

		stubs := spanStubs(branch, 0, 2, "/tmp") // the band opens at the second step
		require.Len(t, stubs, 1)
		assert.Equal(t, "c1", stubs[0].CallID)
		assert.NotContains(t, []string{stubs[0].CallID}, "c2")
		assert.Equal(t, supersededReadMarker, stubs[0].Text)
	})

	// a long run of identical greps: everything after the first collapses
	t.Run("scale_dedupes_identical_runs", func(t *testing.T) {
		var branch []session.Entry
		body := strings.Repeat("match ", 50)
		for i := 1; i <= 8; i++ {
			s := strconv.Itoa(i)
			branch = append(branch,
				callMsg("a"+s, "c"+s, "grep", ""),
				resultMsg("r"+s, "c"+s, body, false))
		}
		stubs := spanStubs(branch, 0, len(branch), "/tmp")
		assert.Len(t, stubs, 7)
		for _, txt := range stubTexts(stubs) {
			assert.Equal(t, dupMarker, txt)
		}
	})
}

func TestFailedStub(t *testing.T) {
	t.Parallel()

	t.Run("keeps_the_first_line", func(t *testing.T) {
		tr := llm.ToolResultBlock{CallID: "c1", IsError: true,
			Content: llm.BlockList{llm.TextBlock{Text: "exit 1: undefined: Foo\nstack..."}}}
		assert.Equal(t, "[tool bash failed: exit 1: undefined: Foo]", failedStub("bash", tr))
	})

	t.Run("blank_output_names_the_tool", func(t *testing.T) {
		tr := llm.ToolResultBlock{CallID: "c1", IsError: true}
		assert.Equal(t, "[tool read failed: output dropped]", failedStub("read", tr))
	})

	t.Run("clip_respects_the_80_rune_cap", func(t *testing.T) {
		tr := llm.ToolResultBlock{CallID: "c1", IsError: true,
			Content: llm.BlockList{llm.TextBlock{Text: strings.Repeat("e", 200)}}}
		assert.Len(t, []rune(failedStub("bash", tr)), len("[tool bash failed: ")+80+len("]"))
	})
}
