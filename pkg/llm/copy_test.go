package llm

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCopyTurn(t *testing.T) {
	t.Parallel()

	assistant := func(blocks ...Block) Message {
		return Message{Role: RoleAssistant, Content: BlockList(blocks)}
	}
	results := func(blocks ...Block) Message {
		return Message{Role: RoleUser, Content: BlockList(blocks)}
	}
	call := ToolCallBlock{ID: "t1", Name: "read", Input: json.RawMessage(`{"path":"a.go"}`)}
	result := ToolResultBlock{CallID: "t1",
		Content: BlockList{TextBlock{Text: "contents"}}}

	t.Run("no_assistant_is_empty", func(t *testing.T) {
		msgs := []Message{Text(RoleUser, "hi")}
		assert.Empty(t, CopyTurn(msgs))
		assert.Empty(t, CopyTurn(nil))
	})

	t.Run("text_only", func(t *testing.T) {
		msgs := []Message{Text(RoleUser, "hi"), assistant(TextBlock{Text: "line one"}, TextBlock{Text: "line two"})}
		assert.Equal(t, "line one\nline two", CopyTurn(msgs))
	})

	t.Run("newest_assistant_wins", func(t *testing.T) {
		msgs := []Message{
			assistant(TextBlock{Text: "first"}),
			Text(RoleUser, "more"),
			assistant(TextBlock{Text: "second"}),
		}
		assert.Equal(t, "second", CopyTurn(msgs))
	})

	t.Run("call_pairs_with_following_result", func(t *testing.T) {
		msgs := []Message{
			Text(RoleUser, "hi"),
			assistant(TextBlock{Text: "done"}, call),
			results(result),
		}
		want := "done\n" +
			`{"name":"read","input":{"path":"a.go"}}` + "\n" +
			`{"callId":"t1","content":[{"text":"contents"}]}`
		assert.Equal(t, want, CopyTurn(msgs))
	})

	t.Run("multiple_calls_in_order", func(t *testing.T) {
		second := ToolCallBlock{ID: "t2", Name: "grep", Input: json.RawMessage(`{"pattern":"x"}`)}
		failed := ToolResultBlock{CallID: "t2", IsError: true,
			Content: BlockList{TextBlock{Text: "boom"}}}
		msgs := []Message{
			assistant(call, second),
			results(result, failed),
		}
		want := `{"name":"read","input":{"path":"a.go"}}` + "\n" +
			`{"callId":"t1","content":[{"text":"contents"}]}` + "\n" +
			`{"name":"grep","input":{"pattern":"x"}}` + "\n" +
			`{"callId":"t2","content":[{"text":"boom"}],"isError":true}`
		assert.Equal(t, want, CopyTurn(msgs))
	})

	t.Run("unanswered_call_copies_alone", func(t *testing.T) {
		msgs := []Message{assistant(call)}
		want := `{"name":"read","input":{"path":"a.go"}}`
		assert.Equal(t, want, CopyTurn(msgs))
	})

	t.Run("thinking_is_skipped", func(t *testing.T) {
		msgs := []Message{assistant(ThinkingBlock{Text: "pondering"}, TextBlock{Text: "answer"})}
		assert.Equal(t, "answer", CopyTurn(msgs))
	})

	t.Run("results_stop_at_next_assistant", func(t *testing.T) {
		second := ToolCallBlock{ID: "t2", Name: "grep", Input: json.RawMessage(`{}`)}
		msgs := []Message{
			assistant(call),
			results(result),
			assistant(second),
		}
		want := `{"name":"read","input":{"path":"a.go"}}` + "\n" +
			`{"callId":"t1","content":[{"text":"contents"}]}`
		assert.Equal(t, want, CopyAssistant(msgs, 0))
	})
}

func TestCopyResults(t *testing.T) {
	t.Parallel()

	call := ToolCallBlock{ID: "t1", Name: "read", Input: json.RawMessage(`{"path":"a.go"}`)}
	result := ToolResultBlock{CallID: "t1",
		Content: BlockList{TextBlock{Text: "contents"}}}
	resultsMsg := Message{Role: RoleUser, Content: BlockList{result}}

	t.Run("pairs_with_call", func(t *testing.T) {
		msgs := []Message{
			Text(RoleUser, "hi"),
			{Role: RoleAssistant, Content: BlockList{call}},
			resultsMsg,
		}
		want := `{"name":"read","input":{"path":"a.go"}}` + "\n" +
			`{"callId":"t1","content":[{"text":"contents"}]}`
		assert.Equal(t, want, CopyResults(msgs, 2))
	})

	t.Run("call_found_across_turns", func(t *testing.T) {
		older := ToolCallBlock{ID: "t1", Name: "old", Input: json.RawMessage(`{}`)}
		msgs := []Message{
			{Role: RoleAssistant, Content: BlockList{older}},
			resultsMsg,
		}
		want := `{"name":"old","input":{}}` + "\n" +
			`{"callId":"t1","content":[{"text":"contents"}]}`
		assert.Equal(t, want, CopyResults(msgs, 1))
	})

	t.Run("unmatched_result_copies_alone", func(t *testing.T) {
		assert.JSONEq(t, `{"callId":"t1","content":[{"text":"contents"}]}`, CopyResults([]Message{resultsMsg}, 0))
	})

	t.Run("multiple_results_in_order", func(t *testing.T) {
		second := ToolCallBlock{ID: "t2", Name: "grep", Input: json.RawMessage(`{}`)}
		failed := ToolResultBlock{CallID: "t2", IsError: true,
			Content: BlockList{TextBlock{Text: "boom"}}}
		msgs := []Message{
			{Role: RoleAssistant, Content: BlockList{call, second}},
			{Role: RoleUser, Content: BlockList{result, failed}},
		}
		want := `{"name":"read","input":{"path":"a.go"}}` + "\n" +
			`{"callId":"t1","content":[{"text":"contents"}]}` + "\n" +
			`{"name":"grep","input":{}}` + "\n" +
			`{"callId":"t2","content":[{"text":"boom"}],"isError":true}`
		assert.Equal(t, want, CopyResults(msgs, 1))
	})

	t.Run("non_result_blocks_skipped", func(t *testing.T) {
		mixed := Message{Role: RoleUser, Content: BlockList{TextBlock{Text: "note"}, result}}
		assert.JSONEq(t, `{"callId":"t1","content":[{"text":"contents"}]}`, CopyResults([]Message{mixed}, 0))
	})
}
