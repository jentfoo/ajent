package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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

// TestConnectInitTimeout is serial: it tightens the package-wide initTimeout.
func TestConnectInitTimeout(t *testing.T) {
	prev := initTimeout
	initTimeout = 250 * time.Millisecond
	t.Cleanup(func() { initTimeout = prev })

	start := time.Now()
	c, err := Connect(t.Context(), "fake", stdioConfig(t, "-hang-init"))
	require.Error(t, err) // the bound fires, the caller's context has no deadline
	assert.Nil(t, c)
	require.ErrorContains(t, err, "initialize")
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestRawIDProgressTokenCorrelation(t *testing.T) {
	t.Parallel()

	c, err := Connect(t.Context(), "fakehttp", ServerConfig{URL: startHTTP(t)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.True(t, mcp.IsModernProtocol(c.negotiated)) // era stamping is active here

	// typed calls ride mcp-go's own ids from one, raw ids must stay disjoint from
	// them however the two interleave
	res, err := c.Call(t.Context(), "tool_00", json.RawMessage(`{}`), nil)
	require.NoError(t, err)
	assert.False(t, res.IsError)

	const workers = 8
	const each = 50
	type stamp struct {
		id     int64
		params json.RawMessage
	}
	stamps := make([]stamp, 0, workers*each)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range each {
				id := c.rawSeq.Add(1)
				params, _ := c.applyEra(string(mcp.MethodToolsList), map[string]any{"cursor": "x"}, id)
				mu.Lock()
				stamps = append(stamps, stamp{id: id, params: mustJSON(params)})
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	require.Len(t, stamps, workers*each)
	ids := make(map[int64]struct{}, len(stamps))
	tokens := make(map[int64]struct{}, len(stamps))
	for _, s := range stamps {
		var p struct {
			Meta struct {
				Token int64 `json:"progressToken"`
			} `json:"_meta"`
		}
		require.NoError(t, json.Unmarshal(s.params, &p))
		assert.Greater(t, s.id, int64(rawSeqBase)) // never collides with a typed id
		assert.Equal(t, s.id, p.Meta.Token, "progress token must equal its request id")
		ids[s.id] = struct{}{}
		tokens[p.Meta.Token] = struct{}{}
	}
	assert.Len(t, ids, workers*each)    // one unique id per request
	assert.Len(t, tokens, workers*each) // and one unique default token
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
	params, header := c.applyEra(string(mcp.MethodToolsList), map[string]any{"cursor": "x"}, c.rawSeq.Add(1))
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

// TestNegotiatesAdvertisedOlderVersion covers a server that answers
// server/discover but serves an older revision: ajent must negotiate to the
// newest advertised version instead of sending era-stamped requests the server
// rejects (the "protocol version 2026-07-28 is not supported" failure).
func TestNegotiatesAdvertisedOlderVersion(t *testing.T) {
	t.Parallel()

	var modernLists atomic.Int32 // tools/list requests stamped 2026-07-28
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.Method == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		respond := func(result string) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":` + result + `}`))
		}
		switch req.Method {
		case "server/discover":
			respond(`{"supportedVersions":["2025-11-25","2025-06-18"],"capabilities":{},"serverInfo":{"name":"old","version":"1.0"}}`)
		case "initialize":
			respond(`{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"old","version":"1.0"}}`)
		case "tools/list":
			if strings.HasPrefix(r.Header.Get("Mcp-Protocol-Version"), "2026") {
				modernLists.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) +
					`,"error":{"code":-32001,"message":"protocol version \"2026-07-28\" is not supported by this server"}}`))
				return
			}
			respond(`{"tools":[{"name":"old_tool","description":"d","inputSchema":{"type":"object"}}]}`)
		default: // notifications and anything else
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(ts.Close)

	c, err := Connect(t.Context(), "aperture", ServerConfig{URL: ts.URL})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	assert.Equal(t, "2025-11-25", c.negotiated) // the advertised set wins over our newest

	defs, err := c.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, defs, 1)
	assert.Equal(t, "old_tool", defs[0].Name)
	assert.Zero(t, modernLists.Load()) // nothing rode the rejected era
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

func TestParseTool(t *testing.T) {
	t.Parallel()

	readOnly := FlexStrings{"gen_*"}
	tests := []struct {
		name    string
		raw     string
		ok      bool
		schema  string // expected InputSchema, empty asserts rejection
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
	assert.NotContains(t, notices[0], "mcp fake") // warnings stay bare, the sink prefixes
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
