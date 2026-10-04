package subagent

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jentfoo/ajent/pkg/agent"
)

// fullSource is the parent-like registry view: every tool, some read-only.
func fullSource() *fakeSource {
	return &fakeSource{
		tools: []agent.Tool{
			&fakeTool{name: "read"},
			&fakeTool{name: "grep"},
			&fakeTool{name: "find"}, // registered disabled in the parent, must still reach a child
			&fakeTool{name: "ls"},
			&fakeTool{name: "git_log"}, // repo-gated git reader
			&fakeTool{name: "git_diff"},
			&fakeTool{name: "bash"},  // enabled in the parent but never read-only
			&fakeTool{name: "write"}, // write tool, not marked read-only
			roTool("mcp_search"),     // MCP tool marked read-only
			roTool("agent_start"),    // must be barred structurally even if reported read-only
			roTool("ask_user"),       // read-only but interactive: barred like agent_*
		},
		readOnly: map[string]bool{"mcp_search": true, "agent_start": true, "ask_user": true},
	}
}

func TestChildTools(t *testing.T) {
	t.Parallel()

	src := fullSource()
	assert.ElementsMatch(t, []string{"read", "grep", "find", "ls", "git_log", "git_diff", "mcp_search"},
		toolNames(childTools(src, true)))
	for _, tl := range childTools(src, true) {
		name := tl.Name()
		assert.NotContains(t, []string{"bash", "write", "edit"}, name)
		assert.False(t, slices.Contains([]string{"agent_start", "agent_poll", "agent_list", "ask_user"}, name))
	}

	src = &fakeSource{tools: []agent.Tool{&fakeTool{name: "bash"}, roTool("read")}}
	// read is builtin-read-only so it survives, bash does not
	assert.Equal(t, []string{"read"}, toolNames(childTools(src, true)))
}

func TestChildToolsGitGate(t *testing.T) {
	t.Parallel()

	src := fullSource()
	got := toolNames(childTools(src, false))
	assert.ElementsMatch(t, []string{"read", "grep", "find", "ls", "mcp_search"}, got)
	for _, name := range gitToolNames {
		assert.NotContains(t, got, name)
	}
}

func TestIsChildBarred(t *testing.T) {
	t.Parallel()

	t.Run("barred_names", func(t *testing.T) {
		for _, name := range []string{"ask_user",
			"dev_implement", "dev_review", "dev_revise", "dev_complete"} {
			assert.True(t, isChildBarred(name))
		}
	})

	t.Run("admitted_names", func(t *testing.T) {
		assert.False(t, isChildBarred("read"))
		assert.False(t, isChildBarred("agent_start")) // barred by the agent_ prefix instead
	})

	// the parent marks dev_* read-only, so only this bar keeps them from children
	t.Run("child_tools_filter", func(t *testing.T) {
		src := &fakeSource{
			tools: []agent.Tool{&fakeTool{name: "read"},
				roTool("dev_implement"), roTool("dev_review"), roTool("dev_revise"), roTool("dev_complete")},
			readOnly: map[string]bool{"dev_implement": true, "dev_review": true,
				"dev_revise": true, "dev_complete": true},
		}
		assert.Equal(t, []string{"read"}, toolNames(childTools(src, true)))
	})
}

func TestIsGitTool(t *testing.T) {
	t.Parallel()

	for _, name := range gitToolNames {
		assert.True(t, isGitTool(name))
	}
	assert.False(t, isGitTool("read"))
	assert.False(t, isGitTool("git_blame")) // only the four shipped readers
}

func TestToolSetView(t *testing.T) {
	t.Parallel()

	set := newToolSet(childTools(fullSource(), true))
	assert.Equal(t, []string{"read", "grep", "find", "ls", "git_log", "git_diff", "mcp_search"}, set.Names())

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
