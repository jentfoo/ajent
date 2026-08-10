package agent

import (
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
)

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
		out := assemble(s, func(ms []llm.Message) []llm.Message {
			return append([]llm.Message{{Role: llm.RoleSystem, Content: llm.BlockList{llm.TextBlock{Text: "sys"}}}}, ms...)
		})
		assert.Len(t, out, 2)
		assert.Equal(t, llm.RoleSystem, out[0].Role)
	})
	t.Run("state_untouched_by_transform", func(t *testing.T) {
		before := len(s.Messages)
		assemble(s, func(ms []llm.Message) []llm.Message { return nil })
		assert.Len(t, s.Messages, before, "assembly never mutates State")
	})
}
