package command

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// fakeMCP is a canned MCPServers backing /mcp tests.
type fakeMCP struct {
	names []string
}

func (f *fakeMCP) ServerNames() []string                    { return f.names }
func (f *fakeMCP) LoadOnFirstMessage(context.Context)       {}
func (f *fakeMCP) Status(context.Context) []MCPServerStatus { return nil }
func (f *fakeMCP) Connect(context.Context, string) error    { return nil }
func (f *fakeMCP) Disconnect(string)                        {}
func (f *fakeMCP) Reload(context.Context) error             { return nil }
func (f *fakeMCP) Logs(string) []string                     { return nil }
func (f *fakeMCP) Groups() []MCPGroup                       { return nil }
func (f *fakeMCP) RefreshStatus()                           {}

// mcpConsole returns a fake console with canned server names.
func mcpConsole(tb testing.TB) *fakeConsole {
	tb.Helper()

	c := newFakeConsole(tb)
	c.mcp = &fakeMCP{names: []string{"github", "gitlab"}}
	return c
}

func TestMCPCompletion(t *testing.T) {
	t.Parallel()

	// verbs while naming one, then server names after connect/disconnect/logs
	t.Run("verbs_then_names", func(t *testing.T) {
		fn := mcpCompletion(mcpConsole(t))

		assert.Equal(t, []string{"connect", "disconnect", "logs", "reload"}, fn(""))
		assert.Equal(t, []string{"connect"}, fn("con"))
		assert.Equal(t, []string{"github", "gitlab"}, fn("connect "))
		assert.Equal(t, []string{"github", "gitlab"}, fn("disconnect "))
		assert.Equal(t, []string{"github", "gitlab"}, fn("logs "))
	})

	t.Run("filters_names_by_prefix", func(t *testing.T) {
		fn := mcpCompletion(mcpConsole(t))

		assert.Equal(t, []string{"github", "gitlab"}, fn("connect gi"))
		assert.Equal(t, []string{"github"}, fn("connect gith"))
		assert.Empty(t, fn("connect nope"))
	})

	// extra breaks before the name parse like dispatch, which trims them
	t.Run("extra_breaks_before_name", func(t *testing.T) {
		fn := mcpCompletion(mcpConsole(t))

		assert.Equal(t, []string{"github", "gitlab"}, fn("connect  "))
		assert.Equal(t, []string{"github", "gitlab"}, fn("connect  gi"))
	})

	// reload takes no name and an unknown verb offers nothing
	t.Run("unsupported_verbs_nil", func(t *testing.T) {
		fn := mcpCompletion(mcpConsole(t))

		assert.Nil(t, fn("reload "))
		assert.Nil(t, fn("bogus "))
	})

	t.Run("unavailable_nil", func(t *testing.T) {
		fn := mcpCompletion(newFakeConsole(t))
		assert.Nil(t, fn(""))
	})
}
