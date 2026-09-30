package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInjectPair(t *testing.T) {
	t.Parallel()

	call := func() ToolCall {
		return ToolCall{ID: "ref-main.go", Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)}
	}

	t.Run("call_and_result_pair", func(t *testing.T) {
		sink := &recordingSink{}
		msgs, res := InjectPair(t.Context(), &stubTool{name: "read", result: "body"}, sink, call(), "read main.go")
		require.Len(t, msgs, 2)

		assert.Equal(t, llm.RoleAssistant, msgs[0].Role)
		cb, ok := msgs[0].Content[0].(llm.ToolCallBlock)
		require.True(t, ok)
		assert.Equal(t, "ref-main.go", cb.ID)
		assert.Equal(t, "read", cb.Name)

		assert.Equal(t, llm.RoleUser, msgs[1].Role)
		rb, ok := msgs[1].Content[0].(llm.ToolResultBlock)
		require.True(t, ok)
		assert.Equal(t, "ref-main.go", rb.CallID)
		assert.False(t, rb.IsError)

		assert.Equal(t, "body", textOf(res.Content))
		assert.Contains(t, sink.calls, "tool_start:read")
	})

	t.Run("error_becomes_result", func(t *testing.T) {
		msgs, res := InjectPair(t.Context(), &stubTool{name: "read", err: errors.New("boom")},
			&recordingSink{}, call(), "read main.go")
		require.Len(t, msgs, 2)
		assert.True(t, res.IsError)
		assert.Equal(t, "boom", textOf(res.Content))

		rb, ok := msgs[1].Content[0].(llm.ToolResultBlock)
		require.True(t, ok)
		assert.True(t, rb.IsError)
	})
}

// textOf joins the text blocks of a result's content.
func textOf(bl llm.BlockList) string {
	var b strings.Builder
	for _, blk := range bl {
		if tb, ok := blk.(llm.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}
