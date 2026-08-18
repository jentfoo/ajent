package subagent

import (
	"slices"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/stretchr/testify/assert"
)

// fullSource is the parent-like registry view: every tool, some read-only.
func fullSource() *fakeSource {
	return &fakeSource{
		tools: []agent.Tool{
			&fakeTool{name: "read"},
			&fakeTool{name: "grep"},
			&fakeTool{name: "find"}, // registered disabled in the parent; must still reach a child
			&fakeTool{name: "ls"},
			&fakeTool{name: "bash"},  // enabled in the parent but never read-only
			&fakeTool{name: "write"}, // write tool, not marked read-only
			roTool("mcp_search"),     // MCP tool marked read-only
			roTool("agent_start"),    // must be barred structurally even if reported read-only
		},
		readOnly: map[string]bool{"mcp_search": true, "agent_start": true},
	}
}

func TestChildTools(t *testing.T) {
	t.Parallel()
	src := fullSource()
	assert.ElementsMatch(t, []string{"read", "grep", "find", "ls", "mcp_search"}, toolNames(childTools(src)))
	for _, tl := range childTools(src) {
		name := tl.Name()
		assert.NotContains(t, []string{"bash", "write", "edit"}, name)
		assert.False(t, slices.Contains([]string{"agent_start", "agent_poll", "agent_list"}, name))
	}

	src = &fakeSource{tools: []agent.Tool{&fakeTool{name: "bash"}, roTool("read")}}
	// read is builtin-read-only so it survives; bash does not
	assert.Equal(t, []string{"read"}, toolNames(childTools(src)))
}

func TestToolSetView(t *testing.T) {
	t.Parallel()
	set := &toolSet{tools: childTools(fullSource())}
	assert.Equal(t, []string{"read", "grep", "find", "ls", "mcp_search"}, set.Names())

	_, ok := set.Get("bash")
	assert.False(t, ok)
	got, ok := set.Get("read")
	if assert.True(t, ok) {
		assert.Equal(t, "read", got.Name())
	}
	assert.Len(t, set.Schemas(), len(set.Names()))
}

func toolNames(ts []agent.Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name()
	}
	return out
}
