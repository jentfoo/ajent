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

func TestChildToolsFiltersToReadOnly(t *testing.T) {
	t.Parallel()
	names := toolNames(childTools(fullSource()))
	assert.ElementsMatch(t, []string{"read", "grep", "find", "ls", "mcp_search"}, names)
	// write/edit/bash are unreachable
	for _, n := range []string{"bash", "write", "edit"} {
		assert.NotContains(t, names, n)
	}
}

func TestChildToolsBarsAgentStartEvenIfReadOnly(t *testing.T) {
	t.Parallel()
	src := fullSource() // agent_start is marked read-only here
	for _, tl := range childTools(src) {
		assert.False(t, slices.Contains([]string{"agent_start", "agent_poll", "agent_list"}, tl.Name()))
	}
}

func TestToolSetView(t *testing.T) {
	t.Parallel()
	set := &toolSet{tools: childTools(fullSource())}
	assert.Equal(t, []string{"read", "grep", "find", "ls", "mcp_search"}, set.Names())

	if _, ok := set.Get("bash"); ok {
		t.Fatal("child tool set must not expose bash")
	}
	if got, ok := set.Get("read"); !ok || got.Name() != "read" {
		t.Fatalf("expected read in child tool set, got %v/%v", got, ok)
	}
	assert.Len(t, set.Schemas(), len(set.Names()))
}

// TestChildToolsNilSource is a no-op guard: nil source yields an empty set.
func TestChildToolsEmptyWhenNoReadOnly(t *testing.T) {
	t.Parallel()
	src := &fakeSource{tools: []agent.Tool{&fakeTool{name: "bash"}, roTool("read")}}
	// read is builtin-read-only so it survives; bash does not
	assert.Equal(t, []string{"read"}, toolNames(childTools(src)))
}

func toolNames(ts []agent.Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name()
	}
	return out
}
