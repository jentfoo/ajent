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

	t.Run("superseded_read", func(t *testing.T) {
		branch := []session.Entry{
			callMsg("a1", "c1", "read", "/tmp/f.go"),
			resultMsg("r1", "c1", "old body", false),
			callMsg("a2", "c2", "read", "/tmp/f.go"),
			resultMsg("r2", "c2", "new body", false),
		}
		stubs := all(branch)
		require.Len(t, stubs, 1)
		assert.Equal(t, "c1", stubs[0].CallID)
		assert.Contains(t, stubs[0].Text, "superseded")
	})

	t.Run("edit_before_write_stubbed", func(t *testing.T) {
		branch := []session.Entry{
			callMsg("a1", "c1", "edit", "/tmp/f.go"),
			resultMsg("r1", "c1", "edited", false),
			callMsg("a2", "c2", "write", "/tmp/f.go"),
			resultMsg("r2", "c2", "written", false),
		}
		stubs := all(branch)
		require.Len(t, stubs, 1)
		assert.Equal(t, "c1", stubs[0].CallID)
		assert.Contains(t, stubs[0].Text, "superseded")
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
		assert.Equal(t, "c2", stubs[0].CallID, "the first copy is the keeper")
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
		assert.Empty(t, all(branch), "two files that share bytes are still two files")
	})

	t.Run("system_role_message_skipped", func(t *testing.T) {
		branch := []session.Entry{
			msg("s1", llm.Message{Role: llm.RoleSystem, Content: llm.BlockList{
				llm.ToolResultBlock{CallID: "c1", Content: llm.BlockList{llm.TextBlock{Text: "x"}}},
			}}),
		}
		assert.Empty(t, all(branch))
	})
}

// The band is what the agent continues from, so nothing in it may be replaced —
// but the rules must still *see* it, or a read superseded by a band read would
// look like the newest copy of that file.
func TestSpanStubsBandScope(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("file body ", 20)
	branch := []session.Entry{
		callMsg("a1", "c1", "read", "/tmp/f.go"),
		resultMsg("r1", "c1", body, false),
		callMsg("a2", "c2", "read", "/tmp/f.go"),
		resultMsg("r2", "c2", body, false),
	}

	t.Run("emits_nothing_inside_the_band", func(t *testing.T) {
		stubs := spanStubs(branch, 0, 2, "/tmp") // the band opens at the second step
		for _, s := range stubs {
			assert.NotEqual(t, "c2", s.CallID)
		}
	})

	t.Run("detects_across_the_band", func(t *testing.T) {
		stubs := spanStubs(branch, 0, 2, "/tmp")
		require.Len(t, stubs, 1)
		assert.Equal(t, "c1", stubs[0].CallID, "the band read still supersedes the span read")
		assert.Equal(t, supersededReadMarker, stubs[0].Text,
			"a marker must not claim a later read is shown above when it sits in the band")
	})

	t.Run("duplicate_keeper_survives_the_cut", func(t *testing.T) {
		// the keeper is inside the band, so the span copy must stay whole rather
		// than point at something the summariser cannot see
		dup := []session.Entry{
			callMsg("a1", "c1", "bash", ""),
			resultMsg("r1", "c1", body, false),
		}
		stubs := spanStubs(dup, 0, len(dup), "/tmp")
		assert.Empty(t, stubs)
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

	t.Run("long_first_line_clipped", func(t *testing.T) {
		tr := llm.ToolResultBlock{CallID: "c1", IsError: true,
			Content: llm.BlockList{llm.TextBlock{Text: strings.Repeat("e", 200)}}}
		assert.Less(t, len([]rune(failedStub("bash", tr))), 120)
	})
}

func TestSpanStubsScale(t *testing.T) {
	t.Parallel()

	// a long run of identical greps: everything after the first collapses
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
}
