package tools

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
)

// preambleArgs is the params shape the decode/resolve helpers test against.
type preambleArgs struct {
	Path string `json:"path"`
}

func TestDecodeCall(t *testing.T) {
	t.Parallel()

	t.Run("decodes_arguments", func(t *testing.T) {
		p, res, ok := decodeCall[preambleArgs](callWith(json.RawMessage(`{"path":"a.txt"}`)))
		require.True(t, ok)
		assert.Equal(t, agent.ToolResult{}, res)
		assert.Equal(t, "a.txt", p.Path)
	})

	t.Run("malformed_args_is_error_result", func(t *testing.T) {
		p, res, ok := decodeCall[preambleArgs](callWith(json.RawMessage(`not json`)))
		assert.False(t, ok)
		assert.Zero(t, p)
		assert.True(t, res.IsError)
		assert.Equal(t, "bad args: arguments are not valid JSON", textOf(res))
	})
}

func TestResolveArg(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	policy := PathPolicy{Cwd: cwd}

	t.Run("resolves_against_policy_cwd", func(t *testing.T) {
		full, res, ok := resolveArg(policy, "a.txt")
		require.True(t, ok)
		assert.Equal(t, agent.ToolResult{}, res)
		assert.Equal(t, filepath.Join(cwd, "a.txt"), full)
	})

	t.Run("empty_defaults_to_session_cwd", func(t *testing.T) {
		full, _, ok := resolveArg(policy, "")
		require.True(t, ok)
		assert.Equal(t, cwd, full)
	})

	t.Run("absolute_passes_through", func(t *testing.T) {
		dir := filepath.Join(cwd, "sub")
		full, _, ok := resolveArg(policy, dir)
		require.True(t, ok)
		assert.Equal(t, dir, full)
	})
}
