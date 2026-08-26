package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
)

func TestBridge(t *testing.T) {
	t.Parallel()

	t.Run("call_maps_text_content", func(t *testing.T) {
		c, err := Connect(t.Context(), "fake", stdioConfig(t))
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		defs, err := c.Tools(t.Context())
		require.NoError(t, err)
		b := Bridge("fake", defs[0], c, BridgeOptions{})

		res, err := b.Execute(t.Context(), agent.ToolCall{ID: "1", Name: b.Name()}, nil)
		require.NoError(t, err)
		assert.False(t, res.IsError)
		assert.Contains(t, textOf(res.Content), "tool_00: ok")
	})

	t.Run("namespaces_name", func(t *testing.T) {
		c, err := Connect(t.Context(), "fake", stdioConfig(t))
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		defs, err := c.Tools(t.Context())
		require.NoError(t, err)

		b := Bridge("fake", defs[0], c, BridgeOptions{})
		assert.Equal(t, "fake__tool_00", b.Name()) // server__tool is what the model sees
		assert.Equal(t, "tool_00", b.Label(agent.ToolCall{}))
	})

	t.Run("mode_parallel_when_read_only", func(t *testing.T) {
		c, err := Connect(t.Context(), "fake", stdioConfig(t))
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		defs, err := c.Tools(t.Context())
		require.NoError(t, err)

		ro := Bridge("fake", defs[0], c, BridgeOptions{ReadOnly: true})
		assert.Equal(t, agent.ModeParallel, ro.Mode()) // read-only runs parallel with other reads
		rw := Bridge("fake", defs[1], c, BridgeOptions{ReadOnly: false})
		assert.Equal(t, agent.ModeSerial, rw.Mode())
	})

	t.Run("timeout_returns_error_result", func(t *testing.T) {
		c, err := Connect(t.Context(), "fake", stdioConfig(t))
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		defs, err := c.Tools(t.Context())
		require.NoError(t, err)
		b := Bridge("fake", defs[0], c, BridgeOptions{Timeout: 1}) // 1ns, instantly exceeded

		res, err := b.Execute(t.Context(), agent.ToolCall{ID: "x"}, nil)
		require.NoError(t, err) // transport failure is a result, not a Go error
		assert.True(t, res.IsError)
	})
}
