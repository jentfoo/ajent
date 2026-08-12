package session

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCompactionResumeRebuildsReducedContext writes a compaction with a reduce
// plan, appends more turns, reopens from disk, and checks the rebuilt context is
// exactly the summary plus the reduced kept tail.
func TestCompactionResumeRebuildsReducedContext(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := Create(p, SessionData{Version: sessionVersion})
	require.NoError(t, err)

	appendMsg := func(m llm.Message) Entry {
		e, aerr := w.Append(TypeMessage, MessageData{Message: m})
		require.NoError(t, aerr)
		return e
	}

	// turn one is summarized away.
	appendMsg(llm.Text(llm.RoleUser, "first ask"))
	appendMsg(llm.Text(llm.RoleAssistant, "first reply"))

	// turn two is kept; its tool result is stubbed by the reduce plan.
	firstKept := appendMsg(llm.Text(llm.RoleUser, "second ask"))
	appendMsg(llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{
		llm.ToolCallBlock{ID: "c1", Name: "bash", Input: json.RawMessage(`{}`)},
	}})
	appendMsg(llm.Message{Role: llm.RoleUser, Content: llm.BlockList{
		llm.ToolResultBlock{CallID: "c1",
			Content: llm.BlockList{llm.TextBlock{Text: "a very large tool output"}}},
	}})

	// a compaction records the cut and the reduction plan.
	red := &Reduce{Stubs: []Stub{{CallID: "c1", Text: "[bash failed: output dropped]"}}}
	_, err = w.Append(TypeCompaction, CompactionData{
		Summary:          "did the first thing",
		FirstKeptEntryID: firstKept.ID,
		Before:           9000,
		After:            1200,
		Reduce:           red,
	})
	require.NoError(t, err)

	// more turns land after the compaction.
	appendMsg(llm.Text(llm.RoleUser, "third ask"))
	appendMsg(llm.Text(llm.RoleAssistant, "final answer"))
	require.NoError(t, w.Sync())
	require.NoError(t, w.Close())

	// reopen from disk and rebuild.
	entries, _, rerr := Read(p)
	require.NoError(t, rerr)
	branch := Branch(entries, Head(entries))
	resolve := func(key string) (llm.Model, error) { return llm.Model{ID: "test"}, nil }
	st, warns := State(branch, resolve)
	assert.Empty(t, warns)

	// summary + second ask + tool call + stubbed result + third ask + final answer
	require.Len(t, st.Messages, 6)
	assert.Equal(t, llm.RoleUser, st.Messages[0].Role) // summary reaches every provider
	assert.Contains(t, userText(st.Messages[0]), "did the first thing")
	assert.Equal(t, "second ask", userText(st.Messages[1]))
	assert.Equal(t, "[bash failed: output dropped]", toolResultTextOf(st.Messages[3]))
	assert.Equal(t, "third ask", userText(st.Messages[4]))
	assert.Equal(t, "final answer", userText(st.Messages[5]))
	assert.True(t, wellFormed(st.Messages))
}

// TestCompactionSummaryOnlyResumeRebuildsCheckpoint writes a summary-only
// compaction (no kept entry), appends more turns, reopens from disk, and checks
// the rebuilt context is exactly the summary plus the later turns.
func TestCompactionSummaryOnlyResumeRebuildsCheckpoint(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := Create(p, SessionData{Version: sessionVersion})
	require.NoError(t, err)

	appendMsg := func(m llm.Message) {
		_, aerr := w.Append(TypeMessage, MessageData{Message: m})
		require.NoError(t, aerr)
	}

	appendMsg(llm.Text(llm.RoleUser, "only ask"))
	appendMsg(llm.Text(llm.RoleAssistant, "only reply"))

	// a summary-only compaction folds everything before it.
	_, err = w.Append(TypeCompaction, CompactionData{
		Summary: "the whole exchange, condensed",
		Before:  500,
		After:   120,
	})
	require.NoError(t, err)

	appendMsg(llm.Text(llm.RoleUser, "next ask"))
	appendMsg(llm.Text(llm.RoleAssistant, "next reply"))
	require.NoError(t, w.Sync())
	require.NoError(t, w.Close())

	entries, _, rerr := Read(p)
	require.NoError(t, rerr)
	branch := Branch(entries, Head(entries))
	resolve := func(key string) (llm.Model, error) { return llm.Model{ID: "test"}, nil }
	st, warns := State(branch, resolve)
	assert.Empty(t, warns)

	require.Len(t, st.Messages, 3) // summary + next ask + next reply
	assert.Contains(t, userText(st.Messages[0]), "the whole exchange, condensed")
	assert.Equal(t, "next ask", userText(st.Messages[1]))
	assert.Equal(t, "next reply", userText(st.Messages[2]))
}

// toolResultTextOf returns the first text block of a message's tool result.
func toolResultTextOf(m llm.Message) string {
	for _, b := range m.Content {
		if tr, ok := b.(llm.ToolResultBlock); ok {
			for _, cb := range tr.Content {
				if tb, ok := cb.(llm.TextBlock); ok {
					return tb.Text
				}
			}
		}
	}
	return ""
}
