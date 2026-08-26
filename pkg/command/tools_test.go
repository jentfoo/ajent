package command

import (
	"context"
	"slices"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolsCommand(t *testing.T) {
	t.Parallel()

	// before the first prompt, a free multi-select can enable any tool.
	t.Run("free_select_before_started", func(t *testing.T) {
		c := newFakeConsole(t)
		// register a couple of tools; ls is disabled by default
		c.tools.Register(&fakeToolAdapter{name: "read"}, true)
		c.tools.Register(&fakeToolAdapter{name: "ls"}, false)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)

		// free select: pick read and ls (indexes 0,1) => both enabled
		c.multiPicks = []fakeMultiPick{{result: []int{0, 1}}}
		cmd, _ := r.Get("tools")
		require.NoError(t, cmd.Handler(t.Context(), "", c))
		assert.Equal(t, []string{"read", "ls"}, c.tools.Names())
		assert.Equal(t, 1, c.toolsChanged)
	})

	// after the first prompt only disabled tools are offered (widen-only).
	t.Run("widen_only_after_started", func(t *testing.T) {
		c := newFakeConsole(t)
		c.tools.Register(&fakeToolAdapter{name: "read"}, true)
		c.tools.Register(&fakeToolAdapter{name: "ls"}, false)
		c.tools.Register(&fakeToolAdapter{name: "find"}, false)
		c.started = true // after first prompt: widen only

		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)

		// only disabled tools offered; pick ls (index 0 of disabled slice)
		c.multiPicks = []fakeMultiPick{{result: []int{0}}}
		cmd, _ := r.Get("tools")
		require.NoError(t, cmd.Handler(t.Context(), "", c))
		assert.True(t, slices.Contains(c.tools.Names(), "read"))
		assert.True(t, slices.Contains(c.tools.Names(), "ls"))
		assert.False(t, slices.Contains(c.tools.Names(), "find"))
	})

	t.Run("widen_only_nothing_disabled", func(t *testing.T) {
		c := newFakeConsole(t)
		c.tools.Register(&fakeToolAdapter{name: "read"}, true)
		c.started = true

		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)
		cmd, _ := r.Get("tools")
		require.NoError(t, cmd.Handler(t.Context(), "", c))
		assert.True(t, c.noticeContains("all tools already enabled"))
	})
}

// fakeToolAdapter adapts a name into a minimal agent.Tool for the registry.
type fakeToolAdapter struct{ name string }

func (f *fakeToolAdapter) Name() string                { return f.name }
func (f *fakeToolAdapter) Label(agent.ToolCall) string { return f.name }
func (f *fakeToolAdapter) Description() string         { return "test" }
func (f *fakeToolAdapter) Schema() llm.ToolSchema      { return llm.ToolSchema{Name: f.name} }
func (f *fakeToolAdapter) Mode() agent.ExecutionMode   { return agent.ModeParallel }
func (f *fakeToolAdapter) Execute(context.Context, agent.ToolCall, agent.Output) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
}
