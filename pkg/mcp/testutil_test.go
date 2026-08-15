package mcp

import (
	"strings"

	"github.com/jentfoo/ajent/pkg/llm"
)

// textOf flattens a BlockList into its text, for assertions.
func textOf(blocks llm.BlockList) string {
	var b strings.Builder
	for _, blk := range blocks {
		if t, ok := blk.(llm.TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}
