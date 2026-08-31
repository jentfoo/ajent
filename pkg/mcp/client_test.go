package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConnectStdio(t *testing.T) {
	t.Parallel()

	t.Run("lists_tools", func(t *testing.T) {
		c, err := Connect(t.Context(), "fake", stdioConfig(t))
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })

		defs, err := c.Tools(t.Context())
		require.NoError(t, err)
		assert.Len(t, defs, 3)
		assert.Equal(t, "tool_00", defs[0].Name)
	})

	t.Run("calls_tool", func(t *testing.T) {
		c, err := Connect(t.Context(), "fake", stdioConfig(t))
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })

		res, err := c.Call(t.Context(), "tool_01", json.RawMessage(`{}`), nil)
		require.NoError(t, err)
		assert.False(t, res.IsError)
		assert.Len(t, res.Content, 1)
		assert.Equal(t, "tool_01: ok", res.Content[0])
	})
}

// TestRawSeqBase verifies Connect seeds the raw-seam id counter into a space
// mcp-go's own request ids never reach, so neither can steal the other's response.
func TestRawSeqBase(t *testing.T) {
	t.Parallel()

	c, err := Connect(t.Context(), "fake", stdioConfig(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	assert.GreaterOrEqual(t, c.rawSeq.Load(), int64(rawSeqBase))
	_, err = c.Tools(t.Context())
	require.NoError(t, err)
	assert.Greater(t, c.rawSeq.Load(), int64(rawSeqBase)) // list calls advance it, still disjoint
}

// TestHandle verifies handlers accumulate across calls rather than replacing.
func TestHandle(t *testing.T) {
	t.Parallel()

	c, err := Connect(t.Context(), "fake", stdioConfig(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	h := func(context.Context, json.RawMessage) (any, error) { return nil, nil }
	c.Handle("first/method", h)
	c.Handle("second/method", h)

	c.mu.Lock()
	defer c.mu.Unlock()
	assert.Contains(t, c.handlers, "first/method")
	assert.Contains(t, c.handlers, "second/method")
	assert.Contains(t, c.handlers, string(mcp.MethodPing)) // always re-implemented
}

// TestCallTimeoutCancelsSlowTool verifies a slow call is cancelled by context.
func TestCallTimeoutCancelsSlowTool(t *testing.T) {
	t.Parallel()

	c, err := Connect(t.Context(), "fake", stdioConfig(t, "-slow"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // already cancelled: the call must not hang
	_, err = c.Call(ctx, "tool_00", json.RawMessage(`{}`), nil)
	assert.Error(t, err) // transport or context failure surfaces as an error
}

func TestToolsPreservesSchemaFidelity(t *testing.T) {
	t.Parallel()

	c, err := Connect(t.Context(), "fake", stdioConfig(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	defs, err := c.Tools(t.Context())
	require.NoError(t, err)

	var schema map[string]any
	err = json.Unmarshal(defs[0].InputSchema, &schema)
	require.NoError(t, err)
	assert.Equal(t, "object", schema["type"])
}

func TestConnectHTTPListsTools(t *testing.T) {
	t.Parallel()

	url := startHTTP(t)
	c, err := Connect(t.Context(), "fakehttp", ServerConfig{URL: url})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	defs, err := c.Tools(t.Context())
	require.NoError(t, err)
	assert.Len(t, defs, 3)
}

// TestHTTPAgainstHTTPServer exercises the Streamable HTTP transport against an
// httptest-backed server via our own in-process server handler.
func TestHTTPAgainstHTTPServer(t *testing.T) {
	t.Parallel()

	srv := mcpserver.NewMCPServer("inproc", "1.0")
	srv.AddTool(mcp.NewToolWithRawSchema("echo", "an echo tool", json.RawMessage(`{"type":"object"}`)),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{mcp.NewTextContent("hi")}}, nil
		})
	h := mcpserver.NewStreamableHTTPServer(srv)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	c, err := Connect(t.Context(), "inproc", ServerConfig{URL: ts.URL + "/mcp"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	defs, err := c.Tools(t.Context())
	require.NoError(t, err)
	assert.Len(t, defs, 1)

	res, err := c.Call(t.Context(), "echo", json.RawMessage(`{}`), nil)
	require.NoError(t, err)
	assert.Equal(t, "hi", res.Content[0])
}

// TestPing verifies the health check round-trips.
func TestPing(t *testing.T) {
	t.Parallel()

	c, err := Connect(t.Context(), "fake", stdioConfig(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	assert.NoError(t, c.Ping(t.Context()))
}

// TestRequestRawSeam sends a custom method through the MCP raw-request seam.
func TestRequestRawSeam(t *testing.T) {
	t.Parallel()

	c, err := Connect(t.Context(), "fake", stdioConfig(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	resp, err := c.Request(t.Context(), "ping", nil)
	require.NoError(t, err)
	assert.NotEmpty(t, resp)
}

func TestClientDiscovers(t *testing.T) {
	t.Parallel()

	// resources/list is fetched through the raw seam and mapped onto our own shape.
	t.Run("resources", func(t *testing.T) {
		c, err := Connect(t.Context(), "fake", stdioConfig(t))
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })

		rs, err := c.Resources(t.Context())
		require.NoError(t, err)
		require.Len(t, rs, 1)
		assert.Equal(t, "fake://doc", rs[0].URI)
		assert.Equal(t, "the doc", rs[0].Name)
	})

	t.Run("prompts", func(t *testing.T) {
		c, err := Connect(t.Context(), "fake", stdioConfig(t))
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })

		ps, err := c.Prompts(t.Context())
		require.NoError(t, err)
		require.Len(t, ps, 1)
		assert.Equal(t, "summarize", ps[0].Name)
		assert.Len(t, ps[0].Arguments, 1)
	})
}

// TestNotificationHandlerDoesNotDeadlock verifies OnNotification dispatches handlers off mcp-go's reader goroutine so a handler that does blocking I/O (like re-discovering after tools/list_changed) cannot stall the stdio transport.
func TestNotificationHandlerDoesNotDeadlock(t *testing.T) {
	t.Parallel()

	c, err := Connect(t.Context(), "fake", stdioConfig(t, "-notify-list-changed"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	var rediscovered atomic.Bool
	// mimic the manager: handle list_changed by synchronously re-listing tools.
	c.OnNotification(func(n mcp.JSONRPCNotification) {
		if n.Method != "notifications/tools/list_changed" {
			return
		}
		defs, err := c.Tools(t.Context())
		if err == nil && len(defs) > 0 { // any non-empty result proves the reader stayed live
			rediscovered.Store(true)
		}
	})

	// trigger_listchanged makes the server respond AND emit list_changed in one burst.
	res, err := c.Call(t.Context(), "trigger_listchanged", json.RawMessage(`{}`), nil)
	require.NoError(t, err)
	require.False(t, res.IsError)

	// Rediscovery inside the handler proves it runs off mcp-go's reader goroutine,
	// not blocking subsequent I/O.
	require.Eventually(t, func() bool { return rediscovered.Load() }, 5*time.Second, 20*time.Millisecond,
		"notification-handler re-discovery deadlocked the stdio transport")

	// a follow-up request must still work after notification handling
	res2, err := c.Call(t.Context(), "tool_00", json.RawMessage(`{}`), nil)
	require.NoError(t, err)
	require.False(t, res2.IsError)
}
