package session

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopyTexts(t *testing.T) {
	t.Parallel()

	entries := []Entry{
		sessionOnly("root"),
		pickMsg("u1", "root", llm.Text(llm.RoleUser, "hello world")),
		pickAssistText("a2", "u1", "hi there"),
		pickToolCall("t3", "a2"),
		pickToolResultMsg("r4", "t3"),
		pickMsg("u5", "r4", llm.Text(llm.RoleUser, "and now?")),
		pickMsg("a6", "u5", llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{
			llm.TextBlock{Text: "thinking aloud"},
			llm.ToolCallBlock{ID: "c9", Name: "grep",
				Input: json.RawMessage(`{"pattern":"x"}`)},
		}}),
		pickMsg("r7", "a6", llm.Message{Role: llm.RoleUser, Content: llm.BlockList{
			llm.ToolResultBlock{CallID: "c9", IsError: true,
				Content: llm.BlockList{llm.TextBlock{Text: "no match"}}},
		}}),
	}

	t.Run("user_prompt_full_text", func(t *testing.T) {
		got := CopyTexts(entries, []string{"u1", "u5"})
		assert.Equal(t, []string{"hello world", "and now?"}, got)
	})

	t.Run("assistant_text_verbatim", func(t *testing.T) {
		got := CopyTexts(entries, []string{"a2"})
		assert.Equal(t, []string{"hi there"}, got)
	})

	t.Run("tool_row_pairs_call_and_result", func(t *testing.T) {
		got := CopyTexts(entries, []string{"t3"})
		require.Len(t, got, 1)
		want := `{"name":"bash","input":{"command":"read main.go"}}` + "\n" +
			`{"callId":"c1","content":[{"text":"line one\nline two"}]}`
		assert.Equal(t, want, got[0])
	})

	t.Run("result_row_finds_its_call", func(t *testing.T) {
		got := CopyTexts(entries, []string{"r4"})
		require.Len(t, got, 1)
		want := `{"name":"bash","input":{"command":"read main.go"}}` + "\n" +
			`{"callId":"c1","content":[{"text":"line one\nline two"}]}`
		assert.Equal(t, got[0], CopyTexts(entries, []string{"t3"})[0]) // same payload both sides of the pair
		assert.Equal(t, want, got[0])
	})

	t.Run("assistant_turn_pairs_across_child", func(t *testing.T) {
		got := CopyTexts(entries, []string{"a6"})
		require.Len(t, got, 1)
		want := "thinking aloud\n" +
			`{"name":"grep","input":{"pattern":"x"}}` + "\n" +
			`{"callId":"c9","content":[{"text":"no match"}],"isError":true}`
		assert.Equal(t, want, got[0])
	})

	t.Run("compaction_copies_summary", func(t *testing.T) {
		withSummary := append(slices.Clone(entries), pickCompaction("c8", "r7"))
		got := CopyTexts(withSummary, []string{"c8"})
		assert.Equal(t, []string{"s"}, got)
	})

	t.Run("unknown_and_session_ids_empty", func(t *testing.T) {
		assert.Equal(t, []string{"", ""}, CopyTexts(entries, []string{"root", "nope"}))
	})

	t.Run("order_follows_ids", func(t *testing.T) {
		got := CopyTexts(entries, []string{"a2", "u1"})
		assert.Equal(t, []string{"hi there", "hello world"}, got)
	})
}

func TestCopyTextsForkChain(t *testing.T) {
	t.Parallel()

	// an abandoned fork: the copied row still pairs with its own chain's results
	entries := []Entry{
		sessionOnly("root"),
		pickMsg("u1", "root", llm.Text(llm.RoleUser, "q")),
		pickToolCall("t2", "u1"),
		pickToolResultMsg("r3", "t2"),
		pickToolCall("t4", "u1"),
	}

	got := CopyTexts(entries, []string{"t4"})
	require.Len(t, got, 1)
	// t4's first child r3 belongs to t2's chain? No: r3's parent is t2, so t4 has
	// no children and copies as a bare call
	assert.JSONEq(t, `{"name":"bash","input":{"command":"read main.go"}}`, got[0])
}

func TestCopyTextsMultiResultChain(t *testing.T) {
	t.Parallel()

	// two calls answered by one following message, like a parallel dispatch step
	callA := llm.ToolCallBlock{ID: "ca", Name: "read", Input: json.RawMessage(`{"path":"a"}`)}
	callB := llm.ToolCallBlock{ID: "cb", Name: "grep", Input: json.RawMessage(`{}`)}
	entries := []Entry{
		sessionOnly("root"),
		pickMsg("u1", "root", llm.Text(llm.RoleUser, "go")),
		pickMsg("a2", "u1", llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{callA, callB}}),
		pickMsg("r3", "a2", llm.Message{Role: llm.RoleUser, Content: llm.BlockList{
			llm.ToolResultBlock{CallID: "ca", Content: llm.BlockList{llm.TextBlock{Text: "A"}}},
			llm.ToolResultBlock{CallID: "cb", Content: llm.BlockList{llm.TextBlock{Text: "B"}}},
		}}),
	}

	got := CopyTexts(entries, []string{"a2"})
	require.Len(t, got, 1)
	want := `{"name":"read","input":{"path":"a"}}` + "\n" +
		`{"callId":"ca","content":[{"text":"A"}]}` + "\n" +
		`{"name":"grep","input":{}}` + "\n" +
		`{"callId":"cb","content":[{"text":"B"}]}`
	assert.Equal(t, want, got[0])
}

func TestCopyTextsSplitResultChain(t *testing.T) {
	t.Parallel()

	// two parallel calls answered by separate result messages, one per dispatch step
	callA := llm.ToolCallBlock{ID: "ca", Name: "read", Input: json.RawMessage(`{"path":"a"}`)}
	callB := llm.ToolCallBlock{ID: "cb", Name: "grep", Input: json.RawMessage(`{}`)}
	entries := []Entry{
		sessionOnly("root"),
		pickMsg("u1", "root", llm.Text(llm.RoleUser, "go")),
		pickMsg("a2", "u1", llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{callA, callB}}),
		pickMsg("r3", "a2", llm.Message{Role: llm.RoleUser, Content: llm.BlockList{
			llm.ToolResultBlock{CallID: "ca", Content: llm.BlockList{llm.TextBlock{Text: "A"}}},
		}}),
		pickMsg("r4", "r3", llm.Message{Role: llm.RoleUser, Content: llm.BlockList{
			llm.ToolResultBlock{CallID: "cb", Content: llm.BlockList{llm.TextBlock{Text: "B"}}},
		}}),
	}

	got := CopyTexts(entries, []string{"a2"})
	require.Len(t, got, 1)
	want := `{"name":"read","input":{"path":"a"}}` + "\n" +
		`{"callId":"ca","content":[{"text":"A"}]}` + "\n" +
		`{"name":"grep","input":{}}` + "\n" +
		`{"callId":"cb","content":[{"text":"B"}]}`
	assert.Equal(t, want, got[0])
}

func TestTurnMessages(t *testing.T) {
	t.Parallel()

	// the result answers t2's only call: the later prompt stays out of the window
	t.Run("stops_when_answered", func(t *testing.T) {
		entries := []Entry{
			sessionOnly("root"),
			pickMsg("u1", "root", llm.Text(llm.RoleUser, "go")),
			pickToolCall("t2", "u1"),
			pickToolResultMsg("r3", "t2"),
			pickMsg("u4", "r3", llm.Text(llm.RoleUser, "later prompt")),
		}

		msgs := turnMessagesOf(t, entries, "t2")
		require.Len(t, msgs, 2)
		assert.Equal(t, llm.RoleAssistant, msgs[0].Role)
		assert.Equal(t, llm.RoleUser, msgs[1].Role)
	})

	// a call that stays unanswered keeps the walk going until the next assistant turn
	t.Run("walks_until_next_assistant", func(t *testing.T) {
		entries := []Entry{
			sessionOnly("root"),
			pickMsg("u1", "root", llm.Text(llm.RoleUser, "go")),
			pickMsg("t2", "u1", llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{
				llm.ToolCallBlock{ID: "c9", Name: "grep"},
			}}),
			pickMsg("r3", "t2", llm.Message{Role: llm.RoleUser, Content: llm.BlockList{
				llm.ToolResultBlock{CallID: "other"},
			}}),
			pickMsg("u4", "r3", llm.Text(llm.RoleUser, "mid prompt")),
			pickAssistText("a5", "u4", "next turn"),
		}

		msgs := turnMessagesOf(t, entries, "t2")
		require.Len(t, msgs, 3) // the call, the mismatched result, then the prompt before a5
	})

	// no calls: the window is just the assistant message itself
	t.Run("text_only_turn", func(t *testing.T) {
		entries := []Entry{
			sessionOnly("root"),
			pickMsg("u1", "root", llm.Text(llm.RoleUser, "hi")),
			pickAssistText("a2", "u1", "hello"),
		}

		msgs := turnMessagesOf(t, entries, "a2")
		require.Len(t, msgs, 1)
	})
}

func TestCallsAnswered(t *testing.T) {
	t.Parallel()

	callA := llm.ToolCallBlock{ID: "ca", Name: "read"}
	callB := llm.ToolCallBlock{ID: "cb", Name: "grep"}
	resultFor := func(id string) llm.Message {
		return llm.Message{Role: llm.RoleUser, Content: llm.BlockList{
			llm.ToolResultBlock{CallID: id},
		}}
	}

	t.Run("no_calls_is_answered", func(t *testing.T) {
		assert.True(t, callsAnswered(llm.Text(llm.RoleAssistant, "hi"), nil))
	})

	t.Run("result_child_answers_call", func(t *testing.T) {
		call := llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{callA}}
		assert.True(t, callsAnswered(call, []llm.Message{resultFor("ca")}))
	})

	t.Run("wrong_call_id_unanswered", func(t *testing.T) {
		call := llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{callA}}
		assert.False(t, callsAnswered(call, []llm.Message{resultFor("cb")}))
	})

	t.Run("partial_results_unanswered", func(t *testing.T) {
		call := llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{callA, callB}}
		assert.False(t, callsAnswered(call, []llm.Message{resultFor("ca")}))
	})

	t.Run("spans_multiple_children", func(t *testing.T) {
		call := llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{callA, callB}}
		assert.True(t, callsAnswered(call, []llm.Message{resultFor("ca"), resultFor("cb")}))
	})

	t.Run("text_children_never_answer", func(t *testing.T) {
		call := llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{callA}}
		assert.False(t, callsAnswered(call, []llm.Message{llm.Text(llm.RoleUser, "a plain prompt")}))
	})
}

// turnMessagesOf decodes entry id and renders its turn window.
func turnMessagesOf(t *testing.T, entries []Entry, id string) []llm.Message {
	t.Helper()

	ti := newTreeIndex(entries)
	e, ok := ti.entry(id)
	require.True(t, ok)
	var md MessageData
	require.NoError(t, e.Decode(&md))
	return turnMessages(ti, e, md.Message)
}
