package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/jentfoo/ajent/pkg/agent"
)

// ToolDef is one tool a server exposes, in our own shape so mcp-go's wire types
// never escape this package.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"` // preserved byte for byte from the server
	ReadOnly    bool            `json:"readOnly,omitempty"`
}

// Resource is one resource a server exposes, in our own shape. Field names match
// the MCP wire keys so discovery decodes directly onto it.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// PromptDef is one prompt template a server offers, in our own shape. Arguments
// describe the variables prompts/get fills.
type PromptDef struct {
	Name        string      `json:"name"`
	Title       string      `json:"title,omitempty"`
	Description string      `json:"description,omitempty"`
	Arguments   []PromptArg `json:"arguments,omitempty"`
}

// PromptArg is one argument of a prompt template.
type PromptArg struct {
	Name        string `json:"name"`
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description,omitempty"`
}

// Client wraps one connected MCP server. It owns the transport lifecycle and the
// raw-request seam extensions ride on, plus progress routing to live outputs.
type Client struct {
	name string
	cfg  ServerConfig

	c    *mcpclient.Client
	cmd  *exec.Cmd // stdio child for process-group kill; nil for network servers
	tran string    // "stdio", "http" or "sse"

	onWarn func(string)

	negotiated string // negotiated protocol version

	rawSeq atomic.Int64 // raw-request id counter; must not collide with mcp-go's

	mu     sync.Mutex
	nextID int64 // progress token source, one per call
	output map[int64]agent.Output
}

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
		hdr := map[string]string{}
		for k, v := range cfg.Headers { // already env-expanded by LoadConfig
			hdr[k] = v
		}
		var err error
		if c.tran == "sse" {
			cl, err = mcpclient.NewSSEMCPClient(cfg.URL, transport.WithHeaders(hdr))
		} else {
			cl, err = mcpclient.NewStreamableHttpClient(cfg.URL,
				transport.WithHTTPHeaders(hdr))
		}
		if err != nil {
			return nil, fmt.Errorf("mcp %s: connect: %w", name, err)
		}
		c.c = cl
	}

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
func (c *Client) warn(msg string) {
	if c.onWarn != nil {
		c.onWarn("mcp " + c.name + ": " + msg)
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

// init negotiates protocol version, naming both versions on a mismatch.
func (c *Client) init(ctx context.Context) error {
	if err := c.c.Start(ctx); err != nil {
		return fmt.Errorf("mcp %s: start: %w", c.name, err)
	}
	res, err := c.c.Initialize(ctx, mcp.InitializeRequest{})
	if err != nil {
		var unsup mcp.UnsupportedProtocolVersionError
		if errors.As(err, &unsup) {
			return fmt.Errorf("mcp %s: protocol version mismatch (server wants %s)", c.name, unsup.Version)
		}
		return fmt.Errorf("mcp %s: initialize: %w", c.name, err)
	}
	c.negotiated = res.ProtocolVersion
	return nil
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
		return fmt.Errorf("mcp %s: spawn %q: %w", c.name, c.cfg.Command, err)
	}
	c.cmd = cmd // captured by the command func; used for process-group kill
	c.c = cl
	return nil
}

// mcpEnv builds a stdio child's environment from the parent plus config overrides.
func mcpEnv(cfg ServerConfig) []string {
	env := os.Environ()
	for k, v := range cfg.Env { // replace or append each override
		found := false
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
	cursor := ""
	for {
		resp, err := c.c.GetTransport().SendRequest(ctx, transport.JSONRPCRequest{
			JSONRPC: mcp.JSONRPC_VERSION,
			ID:      mcp.NewRequestId(c.rawSeq.Add(1)),
			Method:  string(mcp.MethodToolsList),
			Params:  listToolParams(cursor),
		})
		if err != nil {
			return nil, fmt.Errorf("mcp %s: tools/list: %w", c.name, err)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("mcp %s: tools/list: %s", c.name, resp.Error.Message)
		}
		var page struct {
			Tools      []json.RawMessage `json:"tools"`
			NextCursor string            `json:"nextCursor,omitempty"`
		}
		if err = json.Unmarshal(resp.Result, &page); err != nil {
			return nil, fmt.Errorf("mcp %s: tools/list decode: %w", c.name, err)
		}
		for _, raw := range page.Tools {
			def, ok, warn := parseTool(raw, c.cfg.ReadOnly)
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
	return out, nil
}

func listToolParams(cursor string) any {
	p := map[string]any{}
	if cursor != "" {
		p["cursor"] = cursor
	}
	return p
}

// resourcePage is one page of a resources/list response.
type resourcePage struct {
	Resources  []Resource `json:"resources"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

// promptPage is one page of a prompts/list response.
type promptPage struct {
	Prompts    []PromptDef `json:"prompts"`
	NextCursor string      `json:"nextCursor,omitempty"`
}

// Resources lists the server's resources, following pagination. A failed or
// unsupported listing returns an error; callers treat discovery as best effort.
func (c *Client) Resources(ctx context.Context) ([]Resource, error) {
	t := c.c.GetTransport()
	var out []Resource
	cursor := ""
	for {
		resp, err := t.SendRequest(ctx, transport.JSONRPCRequest{
			JSONRPC: mcp.JSONRPC_VERSION,
			ID:      mcp.NewRequestId(c.rawSeq.Add(1)),
			Method:  string(mcp.MethodResourcesList),
			Params:  listToolParams(cursor),
		})
		if err != nil {
			return out, fmt.Errorf("mcp %s: resources/list: %w", c.name, err)
		}
		if resp.Error != nil {
			return out, fmt.Errorf("mcp %s: resources/list: %s", c.name, resp.Error.Message)
		}
		var page resourcePage
		if err = json.Unmarshal(resp.Result, &page); err != nil {
			return out, fmt.Errorf("mcp %s: resources/list decode: %w", c.name, err)
		}
		out = append(out, page.Resources...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}

// Prompts lists the server's prompt templates, following pagination.
func (c *Client) Prompts(ctx context.Context) ([]PromptDef, error) {
	t := c.c.GetTransport()
	var out []PromptDef
	cursor := ""
	for {
		resp, err := t.SendRequest(ctx, transport.JSONRPCRequest{
			JSONRPC: mcp.JSONRPC_VERSION,
			ID:      mcp.NewRequestId(c.rawSeq.Add(1)),
			Method:  string(mcp.MethodPromptsList),
			Params:  listToolParams(cursor),
		})
		if err != nil {
			return out, fmt.Errorf("mcp %s: prompts/list: %w", c.name, err)
		}
		if resp.Error != nil {
			return out, fmt.Errorf("mcp %s: prompts/list: %s", c.name, resp.Error.Message)
		}
		var page promptPage
		if err = json.Unmarshal(resp.Result, &page); err != nil {
			return out, fmt.Errorf("mcp %s: prompts/list decode: %w", c.name, err)
		}
		out = append(out, page.Prompts...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}

// parseTool decodes one tools/list entry preserving inputSchema byte for byte.
func parseTool(raw json.RawMessage, readOnly FlexStrings) (def ToolDef, ok bool, warn string) {
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
	var probe map[string]any
	if err := json.Unmarshal(wire.InputSchema, &probe); err != nil || probe["type"] != "object" {
		return def, false, fmt.Sprintf("tool %q has a non-object input schema; skipped", wire.Name)
	}
	def = ToolDef{
		Name:        wire.Name,
		Description: wire.Description,
		InputSchema: slices.Clone(wire.InputSchema),
	}
	if wire.Annotations != nil && wire.Annotations.ReadOnlyHint != nil {
		def.ReadOnly = *wire.Annotations.ReadOnlyHint
	}
	for _, pat := range readOnly { // config globs mark additional tools read-only; "*" marks all
		if pathMatch(pat, def.Name) {
			def.ReadOnly = true
		}
	}
	return def, true, ""
}

// Call invokes a tool with raw arguments and maps the result content.
func (c *Client) Call(ctx context.Context, name string, args json.RawMessage, out agent.Output) (Result, error) {
	c.mu.Lock()
	token := c.nextID
	c.nextID++
	if out != nil {
		c.output[token] = out
	}
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
		return Result{}, fmt.Errorf("mcp %s: call %q: %w", c.name, name, err)
	}
	return mapCallResult(res), nil
}

// OnNotification registers a handler for server notifications (progress,
// tools/list_changed). Handlers are invoked asynchronously: mcp-go delivers
// notifications on its transport's single reader goroutine, so doing blocking I/O
// inside a handler (e.g. re-discovering after list_changed) would deadlock stdio —
// the reader is what returns those responses. Dispatching each notification to its
// own goroutine keeps that invariant at this boundary for every current and future
// handler.
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

// Ping verifies the server is responsive.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.c.Ping(ctx); err != nil {
		return fmt.Errorf("mcp %s: ping: %w", c.name, err)
	}
	return nil
}

// Close shuts down the client and sweeps a stdio child's whole process group so
// grandchildren do not linger.
func (c *Client) Close() error {
	var err error
	if c.c != nil {
		err = c.c.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
	}
	return err
}

// Request sends an arbitrary JSON-RPC request to the server — the raw seam
// extensions use for methods we do not type.
func (c *Client) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	resp, err := c.c.GetTransport().SendRequest(ctx, transport.JSONRPCRequest{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      mcp.NewRequestId(c.rawSeq.Add(1)),
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp %s: request %q: %w", c.name, method, err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("mcp %s: request %q: %s", c.name, method, resp.Error.Message)
	}
	return slices.Clone(resp.Result), nil
}

// Handle installs a handler for an incoming server→client request method.
// mcp-go's own handlers are replaced after Start; ping is re-implemented.
func (c *Client) Handle(method string, h func(ctx context.Context, params json.RawMessage) (any, error)) {
	bidir, ok := c.c.GetTransport().(transport.BidirectionalInterface)
	if !ok {
		return
	}
	handlers := map[string]handlerFunc{
		string(mcp.MethodPing): pingHandler,
	}
	if h != nil {
		handlers[method] = wrapIncoming(h)
	}
	bidir.SetRequestHandler(func(ctx context.Context, req transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		if hf, ok := handlers[req.Method]; ok {
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
