package session

import (
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// toolResultEntry builds a user message entry holding one tool result for callID.
func toolResultEntry(id, parent, callID, text string, isError bool) Entry {
	m := llm.Message{Role: llm.RoleUser, Content: llm.BlockList{
		llm.ToolResultBlock{CallID: callID, IsError: isError,
			Content: llm.BlockList{llm.TextBlock{Text: text}}},
	}}
	return pickMsg(id, parent, m)
}

// toolResultText extracts the first text block of a message's tool result.
func toolResultText(m llm.Message) string {
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

func TestContextMessages(t *testing.T) {
	t.Parallel()

	// a summary replaces everything before FirstKeptEntryID and is emitted as a
	// user message so it reaches providers that skip the system role.
	t.Run("summary_is_user_message", func(t *testing.T) {
		branch := []Entry{
			pickMsg("m1", "", llm.Text(llm.RoleUser, "dropped")),
			{ID: "comp", Type: TypeCompaction,
				Data: mustJSON(CompactionData{Summary: "the goal", FirstKeptEntryID: "m2"})},
			pickMsg("m2", "comp", llm.Text(llm.RoleUser, "kept")),
		}
		msgs, warns := ContextMessages(branch, CompactionData{
			Summary: "the goal", FirstKeptEntryID: "m2",
		}, nil)
		assert.Empty(t, warns)
		require.Len(t, msgs, 2)                     // summary + kept tail
		assert.Equal(t, llm.RoleUser, msgs[0].Role) // reaches providers that skip system role
		assert.Contains(t, textOf(msgs[0]), "<summary>")
		assert.Contains(t, textOf(msgs[0]), "the goal")
		assert.Equal(t, "kept", textOf(msgs[1]))
	})

	t.Run("reductions_only_no_cut", func(t *testing.T) {
		// an empty FirstKeptEntryID applies reductions without truncating and warns
		// about nothing.
		branch := []Entry{
			pickMsg("m1", "", llm.Text(llm.RoleUser, "one")),
			toolResultEntry("m2", "m1", "c1", "old failed output", true),
			pickMsg("m3", "m2", llm.Text(llm.RoleUser, "two")),
		}
		cd := CompactionData{Reduce: &Reduce{
			Stubs: []Stub{{CallID: "c1", Text: "[bash failed: output dropped]"}},
		}}
		msgs, warns := ContextMessages(branch, cd, nil)
		assert.Empty(t, warns)
		require.Len(t, msgs, 3) // nothing truncated
		assert.Equal(t, "[bash failed: output dropped]", toolResultText(msgs[1]))
	})

	t.Run("drops_listed_entries", func(t *testing.T) {
		branch := []Entry{
			pickMsg("m1", "", llm.Text(llm.RoleUser, "keep")),
			pickAssistText("m2", "m1", ""), // aborted: dropped by id
			pickMsg("m3", "m2", llm.Text(llm.RoleUser, "also keep")),
		}
		cd := CompactionData{Reduce: &Reduce{Drop: []string{"m2"}}}
		msgs, warns := ContextMessages(branch, cd, nil)
		assert.Empty(t, warns)
		require.Len(t, msgs, 2)
		assert.Equal(t, "keep", textOf(msgs[0]))
		assert.Equal(t, "also keep", textOf(msgs[1]))
	})

	t.Run("strips_thinking", func(t *testing.T) {
		assist := llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{
			llm.ThinkingBlock{Text: "hidden reasoning"},
			llm.TextBlock{Text: "visible"},
		}}
		branch := []Entry{pickMsg("m1", "", assist)}
		cd := CompactionData{Reduce: &Reduce{StripThinking: true}}

		msgs, warns := ContextMessages(branch, cd, nil)
		assert.Empty(t, warns)
		require.Len(t, msgs, 1)
		require.Len(t, msgs[0].Content, 1) // thinking stripped, text kept
		assert.Equal(t, "visible", textOf(msgs[0]))
	})

	t.Run("drops_assistant_emptied_by_strip_thinking", func(t *testing.T) {
		// an assistant message holding only thinking becomes empty when stage 3 strips it;
		// providers reject zero-content assistant messages, so the entry is dropped.
		assist := llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{
			llm.ThinkingBlock{Text: "only hidden reasoning"},
		}}
		branch := []Entry{pickMsg("m1", "", assist)}
		cd := CompactionData{Reduce: &Reduce{StripThinking: true}}

		msgs, warns := ContextMessages(branch, cd, nil)
		assert.Empty(t, warns)
		require.Empty(t, msgs) // nothing left worth emitting
	})

	t.Run("keeps_pre_existing_empty_assistant_without_stripping", func(t *testing.T) {
		// a genuinely empty assistant (e.g. from an under-specified test provider) is not
		// dropped when we never stripped thinking, so resume keeps the recorded context.
		assist := llm.Message{Role: llm.RoleAssistant, Content: nil}
		branch := []Entry{pickMsg("m1", "", assist)}

		msgs, warns := ContextMessages(branch, CompactionData{}, nil)
		assert.Empty(t, warns)
		require.Len(t, msgs, 1) // left as recorded when no stage-3 strip ran
	})

	t.Run("summary_only_compaction", func(t *testing.T) {
		branch := []Entry{
			pickMsg("m1", "", llm.Text(llm.RoleUser, "old ask")),
			pickMsg("m2", "m1", llm.Text(llm.RoleAssistant, "old answer")),
			{ID: "comp", Type: TypeCompaction,
				Data: mustJSON(CompactionData{Summary: "everything folded"})},
			pickMsg("m3", "comp", llm.Text(llm.RoleUser, "new ask")),
		}
		msgs, warns := ContextMessages(branch, CompactionData{Summary: "everything folded"}, nil)
		assert.Empty(t, warns)
		require.Len(t, msgs, 2) // summary + post-compaction messages only
		assert.Contains(t, textOf(msgs[0]), "everything folded")
		assert.Equal(t, "new ask", textOf(msgs[1]))
	})

	t.Run("summary_without_entry_keeps_everything", func(t *testing.T) {
		// a summary-only plan measured before its entry exists is no cut at all
		branch := []Entry{pickMsg("m1", "", llm.Text(llm.RoleUser, "only"))}
		msgs, warns := ContextMessages(branch, CompactionData{Summary: "s"}, nil)
		assert.Empty(t, warns)
		require.Len(t, msgs, 1)
	})

	t.Run("missing_first_kept_warns", func(t *testing.T) {
		branch := []Entry{pickMsg("m1", "", llm.Text(llm.RoleUser, "only"))}
		msgs, warns := ContextMessages(branch, CompactionData{FirstKeptEntryID: "ghost"}, nil)
		assert.NotEmpty(t, warns)
		assert.Empty(t, msgs)
	})

	t.Run("elides_by_limit", func(t *testing.T) {
		long := make([]byte, 4096)
		for i := range long {
			long[i] = 'a'
		}
		branch := []Entry{toolResultEntry("m1", "", "c1", string(long), false)}
		cd := CompactionData{Reduce: &Reduce{Stubs: []Stub{{CallID: "c1", Limit: 512}}}}

		msgs, warns := ContextMessages(branch, cd, nil)
		assert.Empty(t, warns)
		require.Len(t, msgs, 1)
		out := toolResultText(msgs[0])
		assert.Less(t, len(out), len(long)) // elided below the original size
		assert.Contains(t, out, "[truncated]")
	})

	t.Run("stamps_origin_across_model_change", func(t *testing.T) {
		branch := []Entry{
			{ID: "s", Type: TypeSession, Data: mustJSON(SessionData{Version: 1, Model: "p/a"})},
			pickMsg("m1", "s", llm.Text(llm.RoleAssistant, "from a")),
			{ID: "mc", Type: TypeModelChange, ParentID: "m1",
				Data: mustJSON(ModelData{Model: "p/b"})},
			pickMsg("m2", "mc", llm.Text(llm.RoleAssistant, "from b")),
		}
		resolve := func(key string) (llm.Model, error) {
			if key == "p/a" {
				return llm.Model{Provider: "p", ID: "a", Caps: llm.Capabilities{Dialect: llm.DialectAnthropic}}, nil
			}
			return llm.Model{Provider: "p", ID: "b", Caps: llm.Capabilities{Dialect: llm.DialectOpenAIResponses}}, nil
		}

		msgs, warns := ContextMessages(branch, CompactionData{}, resolve)
		assert.Empty(t, warns)
		require.Len(t, msgs, 2)

		if assert.NotNil(t, msgs[0].Origin) {
			assert.Equal(t, "a", msgs[0].Origin.Model)
			assert.Equal(t, llm.DialectAnthropic, msgs[0].Origin.Dialect)
		}
		if assert.NotNil(t, msgs[1].Origin) {
			assert.Equal(t, "b", msgs[1].Origin.Model)
			assert.Equal(t, llm.DialectOpenAIResponses, msgs[1].Origin.Dialect)
		}
	})

	t.Run("stamps_origin_across_compaction_cut", func(t *testing.T) {
		// the session entry that records the model lives before FirstKeptEntryID, so
		// it must still seed the producing model for assistant messages in the kept tail.
		branch := []Entry{
			{ID: "s", Type: TypeSession, Data: mustJSON(SessionData{Version: 1, Model: "p/a"})},
			pickMsg("m1", "s", llm.Text(llm.RoleAssistant, "cut away")),
			{ID: "comp", Type: TypeCompaction,
				Data: mustJSON(CompactionData{Summary: "the goal", FirstKeptEntryID: "m2"})},
			pickMsg("m2", "comp", llm.Text(llm.RoleAssistant, "kept")),
		}
		resolve := func(key string) (llm.Model, error) {
			return llm.Model{Provider: "p", ID: "a", Caps: llm.Capabilities{Dialect: llm.DialectAnthropic}}, nil
		}

		msgs, warns := ContextMessages(branch, CompactionData{
			Summary: "the goal", FirstKeptEntryID: "m2",
		}, resolve)
		assert.Empty(t, warns)
		require.Len(t, msgs, 2) // summary + kept assistant
		if assert.NotNil(t, msgs[1].Origin) {
			assert.Equal(t, "a", msgs[1].Origin.Model)
		}
	})

	t.Run("nil_resolver_leaves_unstamped", func(t *testing.T) {
		branch := []Entry{
			{ID: "s", Type: TypeSession, Data: mustJSON(SessionData{Version: 1, Model: "p/a"})},
			pickMsg("m1", "s", llm.Text(llm.RoleAssistant, "from a")),
		}
		msgs, warns := ContextMessages(branch, CompactionData{}, nil)
		assert.Empty(t, warns)
		require.Len(t, msgs, 1)
		assert.Nil(t, msgs[0].Origin)
	})
}
