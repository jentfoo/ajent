package command

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegisterBuiltinsInstallsAll(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	want := []string{"help", "model", "reasoning", "usage", "compact", "tools", "mcp", "agents", "settings", "update", "exit"}
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

func TestFilterPrefix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		names  []string
		prefix string
		want   []string
	}{
		{"empty_prefix_returns_all", []string{"Alpha", "beta"}, "", []string{"Alpha", "beta"}},
		{"case_insensitive_match", []string{"Read", "ls", "find"}, "re", []string{"Read"}},
		{"no_match_is_empty", []string{"read", "write"}, "zz", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, filterPrefix(c.names, c.prefix))
		})
	}
}
