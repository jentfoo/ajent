package command

import (
	"context"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegisterBuiltinsInstallsAll(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	want := []string{"help", "model", "reasoning", "usage", "compact", "tools", "exit"}
	assert.Equal(t, want, r.Names())
}

func TestHelpPrintsCommandList(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	cmd, ok := r.Get("help")
	require.True(t, ok)
	require.NoError(t, cmd.Handler(context.Background(), "", c))

	require.Len(t, c.prints, 1)
	assert.Contains(t, c.prints[0], "# Commands")
	assert.Contains(t, c.prints[0], "/model")
	assert.Contains(t, c.prints[0], "/exit")
}

func TestExitSignalsConsoleExit(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	cmd, _ := r.Get("exit")
	require.NoError(t, cmd.Handler(context.Background(), "", c))
	assert.True(t, c.exited)
}

func TestModelResolveByName(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	cmd, _ := r.Get("model")
	require.NoError(t, cmd.Handler(context.Background(), "beta", c))
	assert.Equal(t, "beta", c.setModel.ID)
}

func TestModelUnknownNameNotifies(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	cmd, _ := r.Get("model")
	require.NoError(t, cmd.Handler(context.Background(), "nope", c))
	assert.Equal(t, llm.Model{}, c.setModel)
	assert.True(t, c.noticeContains("no model matches nope"))
}

func TestModelPickerCancelled(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)
	c.picks = []fakePick{{result: 1}} // pick index 1 = beta

	cmd, _ := r.Get("model")
	require.NoError(t, cmd.Handler(context.Background(), "", c))
	// default active is alpha (first), pick index 1 selects beta
	assert.Equal(t, "beta", c.setModel.ID)
}

func TestReasoningSetsLevel(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	cmd, _ := r.Get("reasoning")
	require.NoError(t, cmd.Handler(context.Background(), "high", c))
	assert.Equal(t, llm.LevelHigh, c.state.Reasoning.Level)
}

func TestReasoningUnknownLevelNotifies(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	cmd, _ := r.Get("reasoning")
	require.NoError(t, cmd.Handler(context.Background(), "bogus", c))
	assert.NotEqual(t, llm.LevelHigh, c.state.Reasoning.Level)
	assert.True(t, c.noticeContains("unknown reasoning level"))
}

func TestToolsFreeSelectBeforeStarted(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	// register a couple of tools; ls is disabled by default
	c.tools.Register(&fakeToolAdapter{name: "read"}, true)
	c.tools.Register(&fakeToolAdapter{name: "ls"}, false)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	// free select: pick read and ls (indexes 0,1) → both enabled
	c.multiPicks = []fakeMultiPick{{result: []int{0, 1}}}
	cmd, _ := r.Get("tools")
	require.NoError(t, cmd.Handler(context.Background(), "", c))
	assert.Equal(t, []string{"read", "ls"}, c.tools.Names())
	assert.Equal(t, 1, c.toolsChanged)
}

func TestToolsWidenOnlyAfterStarted(t *testing.T) {
	t.Parallel()

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
	require.NoError(t, cmd.Handler(context.Background(), "", c))
	assert.True(t, enabledHas(c.tools, "read"))
	assert.True(t, enabledHas(c.tools, "ls"))
	assert.False(t, enabledHas(c.tools, "find"))
}

func TestToolsWidenOnlyNothingDisabled(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	c.tools.Register(&fakeToolAdapter{name: "read"}, true)
	c.started = true

	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)
	cmd, _ := r.Get("tools")
	require.NoError(t, cmd.Handler(context.Background(), "", c))
	assert.True(t, c.noticeContains("all tools already enabled"))
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

// enabledHas reports whether name is currently enabled in reg.
func enabledHas(reg *tools.Registry, name string) bool {
	for _, n := range reg.Names() {
		if n == name {
			return true
		}
	}
	return false
}

func TestUsagePrintsSessionLedger(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	// give the fake state a ledger with one reported turn so /usage has data.
	st := &agent.State{Model: llm.Model{ID: "alpha", Provider: "test"},
		Reasoning: llm.ReasoningConfig{}}
	tok := tokens.New(st.Model)
	const key = "test/alpha"
	tok.Response(key, llm.Usage{Input: 1000, Output: 200}, 900)
	st.Tokens = tok
	c.state = st

	cmd, ok := r.Get("usage")
	require.True(t, ok)
	require.NoError(t, cmd.Handler(context.Background(), "", c))

	require.Len(t, c.prints, 1)
	assert.Contains(t, c.prints[0], "# Usage")
	assert.Contains(t, c.prints[0], "input")
	assert.Contains(t, c.prints[0], "output")
}

func TestUsageWithNoLedgerNotifies(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	c.state.Tokens = nil // no accounting configured

	cmd, ok := r.Get("usage")
	require.True(t, ok)
	require.NoError(t, cmd.Handler(context.Background(), "", c))

	assert.NotEmpty(t, c.notices) // warns rather than panics
}
