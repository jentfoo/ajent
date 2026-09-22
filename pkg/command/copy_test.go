package command

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopyCommand(t *testing.T) {
	t.Parallel()

	run := func(t *testing.T, c *fakeConsole) {
		t.Helper()
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)
		cmd, ok := r.Get("copy")
		require.True(t, ok)
		require.NoError(t, cmd.Handler(t.Context(), "", c))
	}

	t.Run("copies_last_assistant_turn", func(t *testing.T) {
		c := newFakeConsole(t)
		c.state.Messages = []llm.Message{
			llm.Text(llm.RoleUser, "hi"),
			llm.Text(llm.RoleAssistant, "first"),
			llm.Text(llm.RoleUser, "more"),
			llm.Text(llm.RoleAssistant, "second"),
		}

		run(t, c)

		assert.Equal(t, []string{"second"}, c.copiesSeen())
		assert.True(t, c.noticeContains("copied 6 chars"))
	})

	t.Run("no_messages_notices", func(t *testing.T) {
		c := newFakeConsole(t)

		run(t, c)

		assert.Empty(t, c.copiesSeen())
		assert.True(t, c.noticeContains("No agent messages to copy"))
	})

	t.Run("empty_response_notices", func(t *testing.T) {
		c := newFakeConsole(t)
		c.state.Messages = []llm.Message{llm.Text(llm.RoleUser, "hi")}

		run(t, c)

		assert.Empty(t, c.copiesSeen())
		assert.True(t, c.noticeContains("No agent messages to copy"))
	})

	t.Run("write_failure_notices", func(t *testing.T) {
		c := newFakeConsole(t)
		c.state.Messages = []llm.Message{llm.Text(llm.RoleAssistant, "answer")}
		c.copyErr = errors.New("no clipboard writer on PATH")

		run(t, c)

		assert.Equal(t, []string{"answer"}, c.copiesSeen())
		assert.True(t, c.noticeContains("no clipboard writer on PATH"))
	})

	t.Run("tool_turn_copies_verbatim_json", func(t *testing.T) {
		c := newFakeConsole(t)
		c.state.Messages = []llm.Message{
			llm.Text(llm.RoleUser, "hi"),
			{Role: llm.RoleAssistant, Content: llm.BlockList{
				llm.TextBlock{Text: "reading"},
				llm.ToolCallBlock{ID: "t1", Name: "read", Input: json.RawMessage(`{"path":"a.go"}`)},
			}},
			{Role: llm.RoleUser, Content: llm.BlockList{
				llm.ToolResultBlock{CallID: "t1", Content: llm.BlockList{llm.TextBlock{Text: "body"}}},
			}},
		}

		run(t, c)

		want := "reading\n" +
			`{"name":"read","input":{"path":"a.go"}}` + "\n" +
			`{"callId":"t1","content":[{"text":"body"}]}`
		assert.Equal(t, []string{want}, c.copiesSeen())
	})
}
