package command

import (
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelResolveByName(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	cmd, _ := r.Get("model")
	require.NoError(t, cmd.Handler(t.Context(), "beta", c))
	assert.Equal(t, "beta", c.setModel.ID)
}

func TestModelUnknownNameNotifies(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	cmd, _ := r.Get("model")
	require.NoError(t, cmd.Handler(t.Context(), "nope", c))
	assert.Equal(t, llm.Model{}, c.setModel)
	assert.True(t, c.noticeContains("no model matches nope"))
}

func TestModelPickerSelectsActiveOffset(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	// pick index 1; the default active (alpha at index 0) pre-selects that row.
	c.picks = []fakePick{{result: 1}}
	cmd, _ := r.Get("model")
	require.NoError(t, cmd.Handler(t.Context(), "", c))
	assert.Equal(t, "beta", c.setModel.ID)
}

func TestReasoningSetsLevel(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	cmd, _ := r.Get("reasoning")
	require.NoError(t, cmd.Handler(t.Context(), "high", c))
	assert.Equal(t, llm.LevelHigh, c.state.Reasoning.Level)
}

func TestReasoningUnknownLevelNotifies(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	cmd, _ := r.Get("reasoning")
	require.NoError(t, cmd.Handler(t.Context(), "bogus", c))
	assert.NotEqual(t, llm.LevelHigh, c.state.Reasoning.Level)
	assert.True(t, c.noticeContains("unknown reasoning level"))
}
