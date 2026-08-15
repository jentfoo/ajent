package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
)

// TestBridgeCallMapsTextContent verifies a text result round-trips to the model.
func TestBridgeCallMapsTextContent(t *testing.T) {
	t.Parallel()

	c, err := Connect(t.Context(), "fake", stdioConfig(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	defs, _ := c.Tools(t.Context())
	b := Bridge("fake", defs[0], c, BridgeOptions{})

	res, err := b.Execute(context.Background(), agent.ToolCall{ID: "1", Name: b.Name()}, nil)
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, textOf(res.Content), "tool_00: ok")
}

// TestBridgeReadOnlyModeParallel verifies read-only tools run parallel.
func TestBridgeReadOnlyModeParallel(t *testing.T) {
	t.Parallel()

	c, _ := Connect(t.Context(), "fake", stdioConfig(t))
	t.Cleanup(func() { _ = c.Close() })
	defs, _ := c.Tools(t.Context())
	ro := Bridge("fake", defs[0], c, BridgeOptions{ReadOnly: true})
	assert.Equal(t, agent.ModeParallel, ro.Mode())
	rw := Bridge("fake", defs[1], c, BridgeOptions{ReadOnly: false})
	assert.Equal(t, agent.ModeSerial, rw.Mode())
}

// TestBridgeTimeoutReturnsErrorResult verifies an over-long call surfaces as a
// result rather than aborting the turn.
func TestBridgeTimeoutReturnsErrorResult(t *testing.T) {
	c, err := Connect(t.Context(), "fake", stdioConfig(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	defs, _ := c.Tools(t.Context())
	b := Bridge("fake", defs[0], c, BridgeOptions{Timeout: 1}) // 1ns, instantly exceeded

	res, err := b.Execute(context.Background(), agent.ToolCall{ID: "x"}, nil)
	require.NoError(t, err) // transport failure is a result, not a Go error
	assert.True(t, res.IsError)
}
