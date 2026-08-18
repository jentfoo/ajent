package compact

import (
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stage 1: structural reductions ----------------------------------------------

func TestStructural(t *testing.T) {
	t.Parallel()

	t.Run("failed_old_stubbed", func(t *testing.T) {
		branch := []session.Entry{
			userText("u1", "q1"), // turn 1
			callMsg("a1", "c1", "bash", ""),
			resultMsg("r1", "c1", "boom: no such file\nmore detail", true),
			userText("u2", "q2"), // turn 2
			userText("u3", "q3"), // turn 3
			userText("u4", "q4"), // turn 4
		}
		stubs, drops, stats := structural(branch, "/tmp")
		require.Len(t, stubs, 1)
		assert.Equal(t, "c1", stubs[0].CallID)
		assert.Contains(t, stubs[0].Text, "bash failed")
		assert.Contains(t, stubs[0].Text, "boom: no such file") // first line preserved
		assert.Empty(t, drops)
		assert.Equal(t, session.Stats{Failed: 1}, stats)
	})

	t.Run("failed_recent_kept", func(t *testing.T) {
		branch := []session.Entry{
			userText("u1", "q1"),
			callMsg("a1", "c1", "bash", ""),
			resultMsg("r1", "c1", "boom", true), // current turn: still live context
		}
		stubs, drops, stats := structural(branch, "/tmp")
		assert.Empty(t, stubs)
		assert.Empty(t, drops)
		assert.Equal(t, session.Stats{}, stats)
	})

	t.Run("superseded_read", func(t *testing.T) {
		branch := []session.Entry{
			userText("u1", "q1"),
			callMsg("a1", "c1", "read", "main.go"),
			resultMsg("r1", "c1", "old contents", false),
			userText("u2", "q2"),
			callMsg("a2", "c2", "read", "main.go"), // later read of the same path
			resultMsg("r2", "c2", "new contents", false),
		}
		stubs, _, stats := structural(branch, "/tmp")
		require.Len(t, stubs, 1)
		assert.Equal(t, "c1", stubs[0].CallID) // the older read is stubbed
		assert.Contains(t, stubs[0].Text, "superseded")
		assert.Equal(t, session.Stats{Superseded: 1}, stats)
	})

	t.Run("edit_before_write_stubbed", func(t *testing.T) {
		branch := []session.Entry{
			userText("u1", "q1"),
			callMsg("a1", "c1", "edit", "main.go"),
			resultMsg("r1", "c1", "applied diff", false),
			userText("u2", "q2"),
			callMsg("a2", "c2", "write", "main.go"), // later wholesale rewrite
			resultMsg("r2", "c2", "wrote file", false),
		}
		stubs, _, stats := structural(branch, "/tmp")
		require.Len(t, stubs, 1)
		assert.Equal(t, "c1", stubs[0].CallID)
		assert.Contains(t, stubs[0].Text, "superseded")
		assert.Equal(t, session.Stats{Superseded: 1}, stats)
	})

	t.Run("edit_after_write_kept", func(t *testing.T) {
		branch := []session.Entry{
			userText("u1", "q1"),
			callMsg("a1", "c1", "write", "main.go"),
			resultMsg("r1", "c1", "wrote file", false),
			userText("u2", "q2"),
			callMsg("a2", "c2", "edit", "main.go"), // builds on the rewrite: kept
			resultMsg("r2", "c2", "applied diff on top", false),
		}
		stubs, drops, stats := structural(branch, "/tmp")
		assert.Empty(t, stubs) // no later write supersedes this edit
		assert.Empty(t, drops)
		assert.Equal(t, session.Stats{}, stats)
	})

	t.Run("edit_between_writes_stubbed", func(t *testing.T) {
		branch := []session.Entry{
			userText("u1", "q1"),
			callMsg("a1", "c1", "edit", "main.go"),
			resultMsg("r1", "c1", "first diff", false),
			userText("u2", "q2"),
			callMsg("a2", "c2", "write", "main.go"),
			resultMsg("r2", "c2", "wrote v1", false),
			userText("u3", "q3"),
			callMsg("a3", "c3", "edit", "main.go"), // before the final write: stubbed
			resultMsg("r3", "c3", "second diff", false),
			userText("u4", "q4"),
			callMsg("a4", "c4", "write", "main.go"), // last rewrite supersedes both edits
			resultMsg("r4", "c4", "wrote v2", false),
		}
		stubs, _, stats := structural(branch, "/tmp")
		require.Len(t, stubs, 2)
		assert.Equal(t, "c1", stubs[0].CallID)
		assert.Equal(t, "c3", stubs[1].CallID)
		assert.Equal(t, session.Stats{Superseded: 2}, stats)
	})

	t.Run("aborted_assistant_dropped", func(t *testing.T) {
		branch := []session.Entry{
			userText("u1", "q1"),
			assistText("a1", ""), // aborted: no tool calls, no text
			userText("u2", "q2"),
		}
		_, drops, stats := structural(branch, "/tmp")
		assert.Equal(t, []string{"a1"}, drops)
		assert.Equal(t, session.Stats{Aborted: 1}, stats)
	})

	t.Run("tool_calling_assistant_kept", func(t *testing.T) {
		branch := []session.Entry{
			userText("u1", "q1"),
			callMsg("a1", "c1", "bash", ""), // carries a tool call: never dropped
			resultMsg("r1", "c1", "ok", false),
		}
		stubs, drops, stats := structural(branch, "/tmp")
		assert.Empty(t, stubs)
		assert.Empty(t, drops)
		assert.Equal(t, session.Stats{}, stats)
	})
}

// stage 2: output elision -------------------------------------------------------

func TestTruncate(t *testing.T) {
	t.Parallel()

	t.Run("elides_old_large_output", func(t *testing.T) {
		big := strings.Repeat("x", 16<<10)
		branch := []session.Entry{
			userText("u1", "q1"),
			callMsg("a1", "c1", "bash", ""),
			resultMsg("r1", "c1", big, false), // four turns back: oldest tier
			userText("u2", "q2"),
			userText("u3", "q3"),
			userText("u4", "q4"),
			userText("u5", "q5"),
		}
		out := truncate(branch, "/tmp", &session.Reduce{})
		require.Len(t, out, 1)
		assert.Equal(t, "c1", out[0].CallID)
		assert.Positive(t, out[0].Limit)
		assert.Less(t, out[0].Limit, len(big)) // elided below the original
	})

	t.Run("keeps_current_turn", func(t *testing.T) {
		big := strings.Repeat("x", 16<<10)
		branch := []session.Entry{
			userText("u1", "q1"),
			callMsg("a1", "c1", "bash", ""),
			resultMsg("r1", "c1", big, false), // current turn: left alone
		}
		out := truncate(branch, "/tmp", &session.Reduce{})
		assert.Empty(t, out)
	})

	t.Run("elides_recent_output_to_8k", func(t *testing.T) {
		big := strings.Repeat("x", 64<<10)
		branch := []session.Entry{
			userText("u1", "q1"),
			callMsg("a1", "c1", "bash", ""),
			resultMsg("r1", "c1", big, false), // one turn back: recent tier
			userText("u2", "q2"),
		}
		out := truncate(branch, "/tmp", &session.Reduce{})
		require.Len(t, out, 1)
		assert.Equal(t, 8<<10, out[0].Limit) // recent keep: 8 kB
	})

	t.Run("elides_older_output_to_1k", func(t *testing.T) {
		big := strings.Repeat("x", 64<<10)
		branch := []session.Entry{
			userText("u1", "q1"),
			callMsg("a1", "c1", "bash", ""),
			resultMsg("r1", "c1", big, false), // two turns back: older tier
			userText("u2", "q2"),
			userText("u3", "q3"),
		}
		out := truncate(branch, "/tmp", &session.Reduce{})
		require.Len(t, out, 1)
		assert.Equal(t, 1024, out[0].Limit) // older: 1 kB
	})

	t.Run("duplicate_outputs_stubbed", func(t *testing.T) {
		same := strings.Repeat("line\n", 300)
		branch := []session.Entry{
			userText("u1", "q1"),
			callMsg("a1", "c1", "bash", ""),
			resultMsg("r1", "c1", same, false),
			userText("u2", "q2"),
			callMsg("a2", "c2", "bash", ""),
			resultMsg("r2", "c2", same, false), // identical output: stubbed
		}
		out := truncate(branch, "/tmp", &session.Reduce{})
		require.Len(t, out, 1)
		assert.Equal(t, "c2", out[0].CallID)
		assert.Contains(t, out[0].Text, "duplicate")
	})

	t.Run("distinct_targets_not_collapsed", func(t *testing.T) {
		same := strings.Repeat("line\n", 300)
		branch := []session.Entry{
			userText("u1", "q1"),
			callMsg("a1", "c1", "read", "one.txt"),
			resultMsg("r1", "c1", same, false),
			userText("u2", "q2"),
			callMsg("a2", "c2", "read", "two.txt"), // different file
			resultMsg("r2", "c2", same, false),     // identical bytes but not a dup
		}
		out := truncate(branch, "/tmp", &session.Reduce{})
		assert.Empty(t, out) // both are distinct targets; neither collapsed
	})
}
