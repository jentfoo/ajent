package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-analyze/bulk"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/version"
)

// ToolDef is one tool a server exposes, in our own shape so mcp-go's wire types
// never escape this package.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"` // preserved byte for byte from the server
	ReadOnly    bool            `json:"readOnly,omitempty"`
}

// Client wraps one connected MCP server. It owns the transport lifecycle and the
// raw-request seam extensions ride on, plus progress routing to live outputs.
type Client struct {
	name string
	cfg  ServerConfig

	c    *mcpclient.Client
	cmd  *exec.Cmd // stdio child for process-group kill, nil for network servers
	tran string    // "stdio", "http" or "sse"

	onWarn func(string)

	negotiated string             // negotiated protocol version, ""-era values gate applyEra
	clientInfo mcp.Implementation // repeated in every modern request's _meta

	rawSeq atomic.Int64 // raw-request id counter, seeded to rawSeqBase

	mu       sync.Mutex
	nextID   int64 // progress token source, one per call
	output   map[int64]agent.Output
	handlers map[string]handlerFunc // incoming request methods, nil until the first Handle
}

// rawAttemptTimeout bounds one raw-seam request so a dropped or reset response cannot hang discovery.
const rawAttemptTimeout = 15 * time.Second

// probeTimeout bounds the server/discover version probe, so a server that
// predates the method and never answers an unknown request cannot stall the
// handshake. A var so tests can tighten it.
var probeTimeout = 5 * time.Second

// initTimeout bounds the initialize handshake and its transport start, so a
// server that accepts the connection but never answers surfaces as a connect
// error instead of hanging the dial and every waiter sharing it. A var so tests
// can tighten it.
var initTimeout = 45 * time.Second

// rawRetries resends a single request after transport-level failures, since mcp-go's stdio can drop a line under load.
const rawRetries = 2

// rawSeqBase offsets raw-seam ids into a space mcp-go's own counter (from 1, +1 per
// request) cannot reach, so the two never register under one transport response key.
const rawSeqBase = 1 << 40

// closeGrace bounds a polite transport shutdown before Close stops waiting on it.
const closeGrace = 500 * time.Millisecond

// Connect dials cfg's server (stdio or Streamable HTTP / SSE), negotiates the
// protocol and returns a ready client.
func Connect(ctx context.Context, name string, cfg ServerConfig) (*Client, error) {
	c := &Client{
		name:   name,
		cfg:    cfg,
		tran:   transportKind(cfg),
		output: make(map[int64]agent.Output),
	}

	var cl *mcpclient.Client
	switch c.tran {
	case TransportStdio:
		if err := c.spawnStdio(ctx); err != nil {
			return nil, err
		}
	default:
		hdr := maps.Clone(cfg.Headers) // already env-expanded by LoadConfig
		var err error
		if c.tran == "sse" {
			cl, err = mcpclient.NewSSEMCPClient(cfg.URL, transport.WithHeaders(hdr))
		} else {
			cl, err = mcpclient.NewStreamableHttpClient(cfg.URL, transport.WithHTTPHeaders(hdr))
		}
		if err != nil {
			return nil, fmt.Errorf("connect: %w", err)
		}
		c.c = cl
	}

	c.rawSeq.Store(rawSeqBase)
	if err := c.init(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// Transport reports the transport kind as a short label for /mcp.
func (c *Client) Transport() string { return c.tran }

// ServerName returns the configured server name.
func (c *Client) ServerName() string { return c.name }

// SetNotice installs a sink for non-fatal warnings during discovery and calls.
func (c *Client) SetNotice(f func(string)) { c.onWarn = f }

// warn reports through the notice callback, dropping it when none is set.
// Messages stay bare: the sink owns the server prefix, so it appears once.
func (c *Client) warn(msg string) {
	if c.onWarn != nil {
		c.onWarn(msg)
	}
}

// transportKind resolves a server's wire transport: the declared Transport field
// wins, otherwise it is inferred from whether a command (stdio) or url (network)
// is set.
func transportKind(cfg ServerConfig) string {
	switch cfg.Transport {
	case TransportStdio, TransportHTTP, TransportSSE:
		return cfg.Transport
	}
	if cfg.Command != "" { // no explicit transport: a command implies stdio
		return TransportStdio
	}
	return cfg.NetworkKind() // http or sse
}

// init negotiates the protocol version, naming ours and the server's on a mismatch.
func (c *Client) init(ctx context.Context) error {
	ictx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	if err := c.c.Start(ictx); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	c.clientInfo = mcp.Implementation{Name: "ajent", Version: version.Version}
	res, err := c.c.Initialize(ictx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{ClientInfo: c.clientInfo, ProtocolVersion: c.preferredVersion(ictx)},
	})
	if err != nil {
		var unsup mcp.UnsupportedProtocolVersionError
		if errors.As(err, &unsup) {
			return fmt.Errorf("protocol version mismatch (we speak %s, server wants %s)",
				mcp.LATEST_PROTOCOL_VERSION, unsup.Version)
		}
		return fmt.Errorf("initialize: %w", err)
	}
	c.negotiated = res.ProtocolVersion
	return nil
}

// preferredVersion asks a discover-capable server which protocol revisions it
// serves and returns the newest one we share, so a server advertising an older
// set negotiates there instead of failing its first request. The empty string
// leaves Initialize's own probe and handshake to decide.
func (c *Client) preferredVersion(ctx context.Context) string {
	if c.tran == TransportSSE { // legacy-only transport, nothing to discover
		return ""
	}
	supported, ok := c.discoverVersions(ctx)
	if !ok {
		// no discover answer: a pre-discover server, so go straight to the
		// handshake instead of paying Initialize's duplicate probe
		return mcp.LATEST_LEGACY_PROTOCOL_VERSION
	}
	v := mcp.NegotiateMutuallySupportedVersion(supported)
	if v == "" || v == mcp.LATEST_PROTOCOL_VERSION {
		return "" // default path already negotiates LATEST; disjoint sets fail legibly there
	}
	return v
}

// discoverVersions sends one raw server/discover, stamped modern, and returns
// the protocol versions the server advertises. ok is false when the server
// gave no usable answer: method unknown, transport failure, unparseable body.
func (c *Client) discoverVersions(ctx context.Context) ([]string, bool) {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	id := c.rawSeq.Add(1)
	params, header := c.applyEraVersion(mcp.LATEST_PROTOCOL_VERSION, string(mcp.MethodServerDiscover), nil, id)
	header.Set(mcp.HeaderProtocolVersion, mcp.LATEST_PROTOCOL_VERSION) // pre-init, no transport mirrors it yet
	resp, err := c.c.GetTransport().SendRequest(pctx, transport.JSONRPCRequest{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      mcp.NewRequestId(id),
		Method:  string(mcp.MethodServerDiscover),
		Params:  params,
		Header:  header,
	})
	if err != nil {
		return nil, false
	}
	if resp.Error != nil { // a version rejection may name the set the server does serve
		if b, merr := json.Marshal(resp.Error.Data); merr == nil {
			var data struct {
				Supported []string `json:"supported"`
			}
			if json.Unmarshal(b, &data) == nil && len(data.Supported) > 0 {
				return data.Supported, true
			}
		}
		return nil, false
	}
	var disc struct {
		SupportedVersions []string `json:"supportedVersions"`
	}
	if json.Unmarshal(resp.Result, &disc) != nil || len(disc.SupportedVersions) == 0 {
		return nil, false
	}
	return disc.SupportedVersions, true
}

// spawnStdio launches a stdio server in its own process group so Close can sweep
// grandchildren that outlive the child.
func (c *Client) spawnStdio(ctx context.Context) error {
	var cmd *exec.Cmd
	cl, err := mcpclient.NewStdioMCPClientWithOptions(
		c.cfg.Command,
		nil, // env is built by the command func below
		c.cfg.Args,
		transport.WithCommandFunc(func(_ context.Context, command string, _ []string, args []string) (*exec.Cmd, error) {
			cmd = exec.CommandContext(ctx, command, args...)
			if cmd.SysProcAttr == nil {
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			} else {
				cmd.SysProcAttr.Setpgid = true
			}
			cmd.Env = mcpEnv(c.cfg)
			return cmd, nil
		}),
	)
	if err != nil {
		return fmt.Errorf("spawn %q: %w", c.cfg.Command, err)
	}
	c.cmd = cmd // captured by the command func, used for process-group kill
	c.c = cl
	return nil
}

// mcpEnv builds a stdio child's environment from the parent plus config overrides.
func mcpEnv(cfg ServerConfig) []string {
	env := os.Environ()
	for k, v := range cfg.Env { // replace or append each override
		var found bool
		for i, kv := range env {
			if strings.HasPrefix(kv, k+"=") {
				env[i] = k + "=" + v
				found = true
				break
			}
		}
		if !found {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// Tools lists the server's tools by following pagination. inputSchema is fetched
// as raw JSON so a lossy typed decode cannot drop schema keywords.
func (c *Client) Tools(ctx context.Context) ([]ToolDef, error) {
	var out []ToolDef
	var cursor string
	for {
		resp, err := c.sendRaw(ctx, string(mcp.MethodToolsList), listToolParams(cursor))
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("tools/list: %s", resp.Error.Message)
		}
		var page struct {
			Tools      []json.RawMessage `json:"tools"`
			NextCursor string            `json:"nextCursor,omitempty"`
		}
		if err = json.Unmarshal(resp.Result, &page); err != nil {
			return nil, fmt.Errorf("tools/list decode: %w", err)
		}
		for _, raw := range page.Tools {
			def, ok, warn := parseTool(raw, c.name, c.cfg.ReadOnly)
			if !ok {
				c.warn(warn) // one bad tool skips without failing the whole list
				continue
			}
			out = append(out, def)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return uniqueTools(out, c.warn), nil
}

func listToolParams(cursor string) any {
	p := map[string]any{}
	if cursor != "" {
		p["cursor"] = cursor
	}
	return p
}

// uniqueTools drops later duplicates of an already-accepted name, so a server
// listing one tool twice cannot register two entries under one name.
func uniqueTools(defs []ToolDef, warn func(string)) []ToolDef {
	seen := make(map[string]struct{}, len(defs))
	return bulk.SliceFilter(func(d ToolDef) bool {
		if _, ok := seen[d.Name]; ok {
			warn(fmt.Sprintf("tool %q listed more than once; duplicates skipped", d.Name))
			return false
		}
		seen[d.Name] = struct{}{}
		return true
	}, defs)
}

// toolNameRe is the tightest tool-name pattern ajent's providers accept
// (the spec's charset is looser but dotted names 400 on OpenAI).
var toolNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// maxToolNameLen is the composed tool-name budget every provider enforces.
const maxToolNameLen = 64

// parseTool decodes one tools/list entry preserving inputSchema byte for byte.
// A tool whose name or schema could not survive into a provider request body,
// including once namespaced under server, is dropped with a warning for the
// caller to report.
func parseTool(raw json.RawMessage, server string, readOnly FlexStrings) (def ToolDef, ok bool, warn string) {
	var wire struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
		Annotations *struct {
			ReadOnlyHint *bool `json:"readOnlyHint,omitempty"`
		} `json:"annotations,omitempty"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return def, false, "a tools/list entry was undecodable"
	}
	if wire.Name == "" || len(wire.InputSchema) == 0 {
		return def, false, fmt.Sprintf("tool %q has no name or input schema; skipped", wire.Name)
	}
	// providers reject names outside this charset on every request, and the MCP spec
	// requires the same pattern, so a miss is the server's defect
	if !toolNameRe.MatchString(wire.Name) {
		return def, false, fmt.Sprintf("tool %q has a name outside [a-zA-Z0-9_-]{1,64}; skipped", wire.Name)
	}
	// the provider cap applies to the composed server__tool string the registry
	// ships, which only this side of the bridge can measure
	if n := len(server) + len(wire.Name) + 2; n > maxToolNameLen {
		return def, false, fmt.Sprintf("tool %q exceeds the %d character name limit once namespaced as %s__%s; skipped",
			wire.Name, maxToolNameLen, server, wire.Name)
	}
	if reason := schemaDefect(wire.InputSchema); reason != "" {
		return def, false, fmt.Sprintf("tool %q has an invalid input schema: %s; skipped", wire.Name, reason)
	}
	def = ToolDef{
		Name:        wire.Name,
		Description: wire.Description,
		InputSchema: slices.Clone(wire.InputSchema),
	}
	if wire.Annotations != nil && wire.Annotations.ReadOnlyHint != nil {
		def.ReadOnly = *wire.Annotations.ReadOnlyHint
	}
	for _, pat := range readOnly { // config globs mark additional tools read-only, "*" marks all
		if pathMatch(pat, def.Name) {
			def.ReadOnly = true
		}
	}
	return def, true, ""
}

// Call invokes a tool with raw arguments and maps the result content. Progress
// deltas stream to out.
func (c *Client) Call(ctx context.Context, name string, args json.RawMessage, out agent.Output) (Result, error) {
	c.mu.Lock()
	token := c.nextID
	c.nextID++
	c.output[token] = out
	c.mu.Unlock()

	var arguments any
	if len(args) > 0 && string(args) != jsonNull {
		arguments = args
	} else {
		arguments = map[string]any{}
	}

	res, err := c.c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      name,
			Arguments: arguments,
			Meta:      &mcp.Meta{ProgressToken: token},
		},
	})
	c.mu.Lock()
	delete(c.output, token)
	c.mu.Unlock()

	if err != nil {
		return Result{}, fmt.Errorf("call %q: %w", name, err)
	}
	return mapCallResult(res), nil
}

// OnNotification registers a handler for server notifications (progress,
// tools/list_changed). Handlers are invoked asynchronously: mcp-go delivers
// notifications on its transport's single reader goroutine, so doing blocking I/O
// inside a handler would deadlock stdio, since the reader is what returns those
// responses. Dispatching each notification to its own goroutine keeps that
// invariant at this boundary for every current and future handler.
func (c *Client) OnNotification(h func(mcp.JSONRPCNotification)) {
	if c.c == nil {
		return
	}
	c.c.OnNotification(func(n mcp.JSONRPCNotification) { go h(n) })
}

// stderr returns the stdio child's captured stderr reader, or nil for network
// servers. The manager feeds it to /mcp logs.
func (c *Client) stderr() io.Reader {
	r, ok := mcpclient.GetStderr(c.c)
	if !ok || r == nil {
		return nil
	}
	return r
}

// writeProgress streams one progress line to a call's live output, if any. It
// ignores tokens with no matching in-flight call.
func (c *Client) writeProgress(key int64, text string) {
	c.mu.Lock()
	out, ok := c.output[key]
	c.mu.Unlock()
	if ok && out != nil {
		_, _ = out.Write([]byte(text + "\n"))
	}
}

// Ping verifies the server is responsive. The ping RPC was removed in protocol
// version 2026-07-28, where liveness is a transport concern, so a modern
// connection is considered responsive without sending anything.
func (c *Client) Ping(ctx context.Context) error {
	if mcp.IsModernProtocol(c.negotiated) {
		return nil
	}
	if _, err := c.Request(ctx, string(mcp.MethodPing), nil); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	return nil
}

// Close shuts down the client and sweeps a stdio child's whole process group so
// grandchildren do not linger. The transport's own close is bounded by
// closeGrace: mcp-go waits seconds for a child to exit on its closed stdin and
// for a remote to acknowledge the session delete, neither of which is worth
// stalling a session teardown (or the app's exit) for.
func (c *Client) Close() error {
	var err error
	if c.c != nil {
		done := make(chan error, 1)
		go func() { done <- c.c.Close() }()
		timer := time.NewTimer(closeGrace)
		defer timer.Stop()
		select {
		case err = <-done:
		case <-timer.C: // wedged server: the group kill below unblocks its cmd.Wait
		}
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
	}
	return err
}

// sendRaw sends one raw-seam request, retrying transient transport failures so a
// dropped or reset stdio response cannot fail discovery outright.
func (c *Client) sendRaw(ctx context.Context, method string, params any) (*transport.JSONRPCResponse, error) {
	return c.sendRawAttempts(ctx, method, params, rawRetries)
}

// sendRawAttempts sends one request up to retries resends. Each attempt is bounded by
// rawAttemptTimeout, and a fresh ID per attempt keeps late responses from misrouting.
func (c *Client) sendRawAttempts(ctx context.Context, method string, params any, retries int) (*transport.JSONRPCResponse, error) {
	var lastErr error
	for range retries + 1 {
		if err := ctx.Err(); err != nil { // caller budget exhausted, stop early
			return nil, err
		}
		// one id per attempt, threaded into the request and its default progress
		// token together: the token must equal the id and stay unique under concurrency
		id := c.rawSeq.Add(1)
		params, header := c.applyEra(method, params, id)
		aCtx, cancel := context.WithTimeout(ctx, rawAttemptTimeout)
		resp, err := c.c.GetTransport().SendRequest(aCtx, transport.JSONRPCRequest{
			JSONRPC: mcp.JSONRPC_VERSION,
			ID:      mcp.NewRequestId(id),
			Method:  method,
			Params:  params,
			Header:  header,
		})
		cancel()
		if err == nil {
			return resp, nil // a JSON-RPC error response is not a transport failure, no retry
		}
		lastErr = err
	}
	return nil, lastErr
}

// applyEra stamps the negotiated era onto a request; see applyEraVersion.
func (c *Client) applyEra(method string, params any, id int64) (any, http.Header) {
	return c.applyEraVersion(c.negotiated, method, params, id)
}

// applyEraVersion returns params and headers carrying the metadata the given
// protocol version requires on every request. id seeds the default progress
// token, so it matches the request it rides on. Legacy versions are returned
// unchanged.
func (c *Client) applyEraVersion(version, method string, params any, id int64) (any, http.Header) {
	if !mcp.IsModernProtocol(version) {
		return params, nil
	}
	fields := map[string]json.RawMessage{}
	if params != nil {
		if b, err := json.Marshal(params); err == nil {
			_ = json.Unmarshal(b, &fields) // params are plain maps, failures below leave them unstamped
		}
	}
	meta := map[string]any{}
	if raw, ok := fields["_meta"]; ok {
		_ = json.Unmarshal(raw, &meta) // preserve a caller-supplied _meta, e.g. Call's progress token
	}
	meta[mcp.MetaKeyProtocolVersion] = version
	meta[mcp.MetaKeyClientInfo] = c.clientInfo
	meta[mcp.MetaKeyClientCapabilities] = mcp.ClientCapabilities{} // required on every modern request, we declare none
	if _, ok := meta["progressToken"]; !ok {
		meta["progressToken"] = id // ties notifications to the request that caused them
	}
	if b, err := json.Marshal(meta); err == nil {
		fields["_meta"] = b
		if len(fields) == 0 {
			params = map[string]any{"_meta": meta}
		} else {
			params = fields
		}
	}
	header := http.Header{}
	for k, v := range mcp.StandardHeaders(version, mcp.MCPMethod(method), mustJSON(params)) {
		header.Set(k, v)
	}
	return params, header
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// Request sends an arbitrary JSON-RPC request to the server, the raw seam
// extensions use for methods we do not type. Bounded per attempt so it never hangs.
func (c *Client) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	resp, err := c.sendRawAttempts(ctx, method, params, 0)
	if err != nil {
		return nil, fmt.Errorf("request %q: %w", method, err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("request %q: %s", method, resp.Error.Message)
	}
	return slices.Clone(resp.Result), nil
}

// Handle installs a handler for an incoming server-to-client request method, or
// removes it when h is nil. Handlers accumulate across calls. mcp-go's own
// handlers are replaced after Start, and ping is re-implemented.
func (c *Client) Handle(method string, h func(ctx context.Context, params json.RawMessage) (any, error)) {
	bidir, ok := c.c.GetTransport().(transport.BidirectionalInterface)
	if !ok {
		return
	}
	c.mu.Lock()
	first := c.handlers == nil
	if first {
		c.handlers = map[string]handlerFunc{string(mcp.MethodPing): pingHandler}
	}
	if h != nil {
		c.handlers[method] = wrapIncoming(h)
	} else {
		delete(c.handlers, method)
	}
	c.mu.Unlock()
	if !first { // the dispatcher below already reads the live map
		return
	}
	bidir.SetRequestHandler(func(ctx context.Context, req transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		c.mu.Lock()
		hf, ok := c.handlers[req.Method]
		c.mu.Unlock()
		if ok {
			return hf(ctx, req)
		}
		return nil, fmt.Errorf("unsupported request method: %s", req.Method)
	})
}

type handlerFunc func(context.Context, transport.JSONRPCRequest) (*transport.JSONRPCResponse, error)

func pingHandler(_ context.Context, _ transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	b, _ := json.Marshal(&mcp.EmptyResult{})
	return &transport.JSONRPCResponse{JSONRPC: mcp.JSONRPC_VERSION, Result: b}, nil
}

// wrapIncoming adapts a plain params-in/result-out handler to the transport's.
func wrapIncoming(h func(ctx context.Context, params json.RawMessage) (any, error)) handlerFunc {
	return func(ctx context.Context, req transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		var raw json.RawMessage
		if req.Params != nil {
			b, err := json.Marshal(req.Params)
			if err != nil {
				return nil, err
			}
			raw = b
		}
		result, err := h(ctx, raw)
		if err != nil {
			return transport.NewJSONRPCErrorResponse(req.ID, 0, err.Error(), nil), nil
		}
		b, _ := json.Marshal(result)
		return &transport.JSONRPCResponse{JSONRPC: mcp.JSONRPC_VERSION, ID: req.ID, Result: b}, nil
	}
}
