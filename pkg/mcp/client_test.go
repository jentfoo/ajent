package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/llm"
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
		require.Len(t, res.Blocks, 1)
		assert.Equal(t, "tool_01: ok", res.Blocks[0].(llm.TextBlock).Text)
	})
}

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
	assert.Equal(t, mcp.LATEST_PROTOCOL_VERSION, c.negotiated) // modern server, modern era
	defs, err := c.Tools(t.Context())
	require.NoError(t, err)
	assert.Len(t, defs, 3)
}

func TestLegacyServerCompat(t *testing.T) {
	t.Parallel()

	url := startHTTP(t, "-legacy")
	c, err := Connect(t.Context(), "legacyhttp", ServerConfig{URL: url})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	assert.Equal(t, mcp.LATEST_LEGACY_PROTOCOL_VERSION, c.negotiated)

	// raw-seam requests stay byte-identical to the pre-1.0 wire: no _meta, no headers
	params, header := c.applyEra(string(mcp.MethodToolsList), map[string]any{"cursor": "x"})
	assert.Equal(t, map[string]any{"cursor": "x"}, params)
	assert.Nil(t, header)

	defs, err := c.Tools(t.Context())
	require.NoError(t, err)
	assert.Len(t, defs, 3)

	res, err := c.Call(t.Context(), "tool_01", json.RawMessage(`{}`), nil)
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Equal(t, "tool_01: ok", res.Blocks[0].(llm.TextBlock).Text)

	// legacy servers still answer the ping RPC for real
	require.NoError(t, c.Ping(t.Context()))
}

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
	assert.Equal(t, "hi", res.Blocks[0].(llm.TextBlock).Text)
}

func TestPing(t *testing.T) {
	t.Parallel()

	c, err := Connect(t.Context(), "fake", stdioConfig(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	assert.NoError(t, c.Ping(t.Context()))
}

func TestRequestRawSeam(t *testing.T) {
	t.Parallel()

	c, err := Connect(t.Context(), "fake", stdioConfig(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	resp, err := c.Request(t.Context(), string(mcp.MethodToolsList), nil)
	require.NoError(t, err)
	assert.NotEmpty(t, resp)
}

func TestClientDiscovers(t *testing.T) {
	t.Parallel()

	// resources/list is fetched through the raw seam and mapped onto our own shape
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

func TestParseTool(t *testing.T) {
	t.Parallel()

	readOnly := FlexStrings{"gen_*"}
	tests := []struct {
		name    string
		raw     string
		ok      bool
		schema  string // expected InputSchema; empty asserts rejection
		wantSub string // expected substring of the warning
	}{
		{
			name:   "valid_tool_passes",
			raw:    `{"name":"t1","description":"d","inputSchema":{"type":"object","properties":{}}}`,
			ok:     true,
			schema: `{"type":"object","properties":{}}`,
		},
		{
			name:   "schema_preserved_byte_for_byte",
			raw:    `{"name":"t1","inputSchema":{"type": "object", "properties":{"a": {"type":"string"}}}}`,
			ok:     true,
			schema: `{"type": "object", "properties":{"a": {"type":"string"}}}`,
		},
		{name: "missing_name", raw: `{"inputSchema":{"type":"object"}}`, wantSub: "has no name or input schema"},
		{name: "missing_schema", raw: `{"name":"t1"}`, wantSub: "has no name or input schema"},
		{name: "name_with_space", raw: `{"name":"my tool","inputSchema":{"type":"object"}}`, wantSub: "name outside [a-zA-Z0-9_-]{1,64}"},
		{name: "name_too_long", raw: `{"name":"` + strings.Repeat("x", 65) + `","inputSchema":{"type":"object"}}`, wantSub: "name outside [a-zA-Z0-9_-]{1,64}"},
		{ // bare name is legal but fake__<58 x's> exceeds the composed 64 budget
			name:    "namespaced_name_too_long",
			raw:     `{"name":"` + strings.Repeat("x", 59) + `","inputSchema":{"type":"object"}}`,
			wantSub: "exceeds the 64 character name limit once namespaced as fake__",
		},
		{name: "undecodable_entry", raw: `{"name":`, wantSub: "undecodable"},
		{name: "top_not_json", raw: `{"name":"t1","inputSchema":[]}`, wantSub: "not a JSON object"},
		{
			name:    "top_not_object",
			raw:     `{"name":"t1","inputSchema":{"type":"string"}}`,
			wantSub: `type must be "object"`,
		},
		{
			name:    "malformed_property_rejected",
			raw:     `{"name":"t1","inputSchema":{"type":"object","properties":{"rows":{"type":"array","items":"string"}}}}`,
			wantSub: "properties.rows.items is not an object",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def, ok, warn := parseTool(json.RawMessage(tt.raw), "fake", readOnly)
			assert.Equal(t, tt.ok, ok)
			assert.Contains(t, warn, tt.wantSub)
			if tt.schema != "" {
				assert.Equal(t, tt.schema, string(def.InputSchema)) // byte for byte
			}
		})
	}

	t.Run("read_only_globs_apply", func(t *testing.T) {
		def, ok, warn := parseTool(json.RawMessage(`{"name":"gen_echo","inputSchema":{"type":"object"}}`), "fake", readOnly)
		require.True(t, ok)
		assert.Empty(t, warn)
		assert.True(t, def.ReadOnly)
	})
}

func TestUniqueTools(t *testing.T) {
	t.Parallel()

	var warns []string
	defs := uniqueTools([]ToolDef{
		{Name: "a", InputSchema: jsonRawObject},
		{Name: "b", InputSchema: jsonRawObject},
		{Name: "a", InputSchema: jsonRawObject}, // listed twice by the server
	}, func(msg string) { warns = append(warns, msg) })

	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	assert.Equal(t, []string{"a", "b"}, names)
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0], `tool "a" listed more than once`)
}

func TestToolsDropsBadSchema(t *testing.T) {
	t.Parallel()

	c, err := Connect(t.Context(), "fake", stdioConfig(t, "-bad-schema"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	var notices []string // warn fires synchronously inside Tools, single goroutine
	c.SetNotice(func(msg string) { notices = append(notices, msg) })

	defs, err := c.Tools(t.Context())
	require.NoError(t, err)
	assert.Len(t, defs, 3) // the generated tools survive the bad one
	assert.False(t, slices.ContainsFunc(defs, func(d ToolDef) bool { return d.Name == "bad_schema" }))
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0], `tool "bad_schema" has an invalid input schema`)
	assert.Contains(t, notices[0], "properties.rows.items is not an object")
}

func TestNotificationHandlerDoesNotDeadlock(t *testing.T) {
	t.Parallel()

	c, err := Connect(t.Context(), "fake", stdioConfig(t, "-notify-list-changed"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	var rediscovered atomic.Bool
	// mimic the manager: handle list_changed by synchronously re-listing tools
	c.OnNotification(func(n mcp.JSONRPCNotification) {
		if n.Method != "notifications/tools/list_changed" {
			return
		}
		defs, err := c.Tools(t.Context())
		if err == nil && len(defs) > 0 { // any non-empty result proves the reader stayed live
			rediscovered.Store(true)
		}
	})

	// trigger_listchanged makes the server respond AND emit list_changed in one burst
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
