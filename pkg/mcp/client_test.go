package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
)

func TestConnectStdioListsTools(t *testing.T) {
	t.Parallel()

	c, err := Connect(t.Context(), "fake", stdioConfig(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	defs, err := c.Tools(t.Context())
	require.NoError(t, err)
	assert.Len(t, defs, 3)
	assert.Equal(t, "tool_00", defs[0].Name)
}

func TestConnectStdioCallsTool(t *testing.T) {
	t.Parallel()

	c, err := Connect(t.Context(), "fake", stdioConfig(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	res, err := c.Call(t.Context(), "tool_01", json.RawMessage(`{}`), nil)
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Len(t, res.Content, 1)
	assert.Equal(t, "tool_01: ok", res.Content[0])
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

// TestRequestRawSeam sends a custom method through the raw-request seam phase 11
// relies on.
func TestRequestRawSeam(t *testing.T) {
	t.Parallel()

	c, err := Connect(t.Context(), "fake", stdioConfig(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	resp, err := c.Request(t.Context(), "ping", nil)
	require.NoError(t, err)
	assert.NotEmpty(t, resp)
}

// TestHandleIncomingAnswer installs an incoming-request handler and exercises it
// via the transport's own dispatch.
func TestBridgeNamespacesName(t *testing.T) {
	t.Parallel()

	c, err := Connect(t.Context(), "fake", stdioConfig(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	defs, _ := c.Tools(t.Context())
	b := Bridge("fake", defs[0], c, BridgeOptions{})
	assert.Equal(t, "fake__tool_00", b.Name())
	assert.Equal(t, agent.ModeSerial, b.Mode()) // not read-only: serial
}

// TestClientDiscoversResources verifies resources/list is fetched through the raw
// seam and mapped onto our own shape.
func TestClientDiscoversResources(t *testing.T) {
	t.Parallel()

	c, err := Connect(t.Context(), "fake", stdioConfig(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	rs, err := c.Resources(t.Context())
	require.NoError(t, err)
	require.Len(t, rs, 1)
	assert.Equal(t, "fake://doc", rs[0].URI)
	assert.Equal(t, "the doc", rs[0].Name)
}

// TestClientDiscoversPrompts verifies prompts/list is fetched and mapped.
func TestClientDiscoversPrompts(t *testing.T) {
	t.Parallel()

	c, err := Connect(t.Context(), "fake", stdioConfig(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	ps, err := c.Prompts(t.Context())
	require.NoError(t, err)
	require.Len(t, ps, 1)
	assert.Equal(t, "summarize", ps[0].Name)
	assert.Len(t, ps[0].Arguments, 1)
}
