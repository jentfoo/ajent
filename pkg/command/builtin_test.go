package command

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// commandNames returns the registered names in order, derived from List.
func commandNames(r *Registry) []string {
	cmds := r.List()
	names := make([]string, len(cmds))
	for i, c := range cmds {
		names[i] = c.Name
	}
	return names
}

func TestRegisterBuiltinsInstallsAll(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	want := []string{"help", "model", "reasoning", "usage", "copy", "compact", "tools", "session", "mcp", "reload", "agents", "settings", "update", "exit"}
	assert.Equal(t, want, commandNames(r))
}

func TestHelp(t *testing.T) {
	t.Parallel()

	setup := func(t *testing.T) (*fakeConsole, Command) {
		t.Helper()

		c := newFakeConsole(t)
		r := NewRegistry()
		c.commands = r
		RegisterBuiltins(r, c)
		cmd, ok := r.Get("help")
		require.True(t, ok)
		return c, cmd
	}

	t.Run("prints_command_list", func(t *testing.T) {
		c, cmd := setup(t)
		require.NoError(t, cmd.Handler(t.Context(), "", c))

		require.Len(t, c.prints, 1)
		assert.Contains(t, c.prints[0], "# Commands")
		assert.Contains(t, c.prints[0], "/model")
		assert.Contains(t, c.prints[0], "/exit")
	})

	t.Run("bare_prints_reference", func(t *testing.T) {
		c, cmd := setup(t)
		require.NoError(t, cmd.Handler(t.Context(), "  ", c)) // whitespace-only stays bare

		require.Len(t, c.prints, 1)
		for _, want := range []string{"# Commands", "!!cmd", "Ctrl+K", "Ctrl+U", "Ctrl+Y", "Ctrl+J", "/help <question>"} {
			assert.Contains(t, c.prints[0], want)
		}
		assert.Empty(t, c.helpAsks) // never the Help seam
	})

	t.Run("question_delegates_to_help", func(t *testing.T) {
		c, cmd := setup(t)
		q := "how do I mark commands as read-only"
		require.NoError(t, cmd.Handler(t.Context(), q, c))

		assert.Equal(t, []string{q}, c.helpAsks) // the question rides verbatim
		assert.Empty(t, c.prints)                // prints nothing itself
	})
}

func TestExitSignalsConsoleExit(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	cmd, _ := r.Get("exit")
	require.NoError(t, cmd.Handler(t.Context(), "", c))
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
