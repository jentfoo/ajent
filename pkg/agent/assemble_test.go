package agent

import (
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// systemText returns the text of a message's first block, for asserting order.
func systemText(m llm.Message) string {
	if b, ok := m.Content[0].(llm.TextBlock); ok {
		return b.Text
	}
	return ""
}

func TestAssemble(t *testing.T) {
	t.Parallel()

	s := &State{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: llm.BlockList{llm.TextBlock{Text: "hi"}}},
	}}

	t.Run("identity_without_transform", func(t *testing.T) {
		out := assemble(s, nil)
		assert.Equal(t, s.Messages, out)
	})
	t.Run("transform_applied", func(t *testing.T) {
		prependSystem := func(ms []llm.Message) []llm.Message {
			return append([]llm.Message{{Role: llm.RoleSystem, Content: llm.BlockList{llm.TextBlock{Text: "sys"}}}}, ms...)
		}
		out := assemble(s, []Transform{prependSystem})
		assert.Len(t, out, 2)
		assert.Equal(t, llm.RoleSystem, out[0].Role)
	})
	t.Run("chain_applies_in_order", func(t *testing.T) {
		first := func(ms []llm.Message) []llm.Message {
			return append([]llm.Message{{Role: llm.RoleSystem, Content: llm.BlockList{llm.TextBlock{Text: "a"}}}}, ms...)
		}
		second := func(ms []llm.Message) []llm.Message {
			return append([]llm.Message{{Role: llm.RoleSystem, Content: llm.BlockList{llm.TextBlock{Text: "b"}}}}, ms...)
		}
		out := assemble(s, []Transform{nil, first, nil, second})
		require.Len(t, out, 3) // b then a prepended in order; the original stays last
		assert.Equal(t, "b", systemText(out[0]))
		assert.Equal(t, "a", systemText(out[1]))
	})
	t.Run("state_untouched_by_transform", func(t *testing.T) {
		before := len(s.Messages)
		assemble(s, []Transform{func(ms []llm.Message) []llm.Message { return nil }})
		assert.Len(t, s.Messages, before)
	})
}
