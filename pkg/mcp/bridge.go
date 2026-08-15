package mcp

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// defaultCallTimeout bounds a single MCP tool call when the server sets none.
const defaultCallTimeout = 60 * time.Second

// maxCallTimeout clamps an over-large configured timeout, mirroring bash.go.
const maxCallTimeout = 10 * time.Minute

// BridgeOptions configures how one bridged tool behaves.
type BridgeOptions struct {
	ReadOnly bool          // from annotations or config globs
	Timeout  time.Duration // per-call cap; zero uses defaultCallTimeout
}

// bridgeTool adapts a remote MCP tool to agent.Tool. Names are namespaced
// server__tool so separate servers cannot collide; the model sees that name.
type bridgeTool struct {
	name     string  // server__tool, stable in the transcript
	label    string  // bare tool name for the UI when unambiguous
	def      ToolDef // description + schema from discovery
	c        *Client // to call through (never nil once registered)
	readOnly bool
	timeout  time.Duration
}

// Bridge turns a discovered tool into an agent.Tool that calls back into c.
func Bridge(server string, def ToolDef, c *Client, opts BridgeOptions) agent.Tool {
	return &bridgeTool{
		name:     server + "__" + def.Name,
		label:    def.Name,
		def:      def,
		c:        c,
		readOnly: opts.ReadOnly || def.ReadOnly,
		timeout:  opts.Timeout,
	}
}

func (b *bridgeTool) Name() string { return b.name }

// Label returns the bare tool name, falling back to it on bad input too.
func (b *bridgeTool) Label(_ agent.ToolCall) string {
	if b.label != "" {
		return b.label
	}
	return b.name
}

func (b *bridgeTool) Description() string { return b.def.Description }

// Schema returns the server's own JSON schema; name and description are filled by
// the registry when it builds the tool block.
func (b *bridgeTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{Parameters: b.def.InputSchema}
}

// Mode is serial unless read-only, which may run parallel with other reads.
func (b *bridgeTool) Mode() agent.ExecutionMode {
	if b.readOnly {
		return agent.ModeParallel
	}
	return agent.ModeSerial
}

// Execute calls the remote tool, mapping progress to out and a transport failure
// to an error result so the turn continues rather than aborting.
func (b *bridgeTool) Execute(ctx context.Context, call agent.ToolCall, out agent.Output) (agent.ToolResult, error) {
	timeout := b.timeout
	if timeout <= 0 {
		timeout = defaultCallTimeout
	}
	if timeout > maxCallTimeout {
		timeout = maxCallTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c := b.c
	res, err := c.Call(runCtx, b.def.Name, call.Input, out)
	if err != nil {
		return agent.ToolResult{
			Content: llm.BlockList{llm.TextBlock{Text: "mcp error: " + err.Error()}},
			IsError: true,
		}, nil
	}
	r := res.toBlocks()
	if len(r) == 0 {
		r = llm.BlockList{llm.TextBlock{Text: "(no content)"}}
	}
	return agent.ToolResult{
		Content: r,
		Display: displayOf(res),
		IsError: res.IsError,
	}, nil
}

// displayOf renders a one-line history summary of the result.
func displayOf(r Result) string {
	return elide(strings.Join(r.Content, ""), 1000)
}

// elide truncates s to at most n bytes with an ellipsis marker. The cut backs off
// to a UTF-8 rune boundary so multibyte content is never split mid-rune.
func elide(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) { // step back from the cut to a rune start
		n--
	}
	return s[:n] + "…"
}
