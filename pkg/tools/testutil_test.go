package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// callWith builds a tool call carrying raw args.
func callWith(raw json.RawMessage) agent.ToolCall {
	return agent.ToolCall{ID: "c1", Name: "bash", Input: raw}
}

// textOf returns the concatenated plain text of a result's content blocks.
func textOf(res agent.ToolResult) string {
	var b strings.Builder
	for _, blk := range res.Content {
		if tb, ok := blk.(llm.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

// fakeTool is a minimal agent.Tool for registry and guard tests.
type fakeTool struct {
	name string
	mode agent.ExecutionMode
}

func (f *fakeTool) Name() string { return f.name }
func (f *fakeTool) Label(agent.ToolCall) string {
	return f.name
}
func (f *fakeTool) Description() string       { return "test tool" }
func (f *fakeTool) Schema() llm.ToolSchema    { return llm.ToolSchema{Name: f.name} }
func (f *fakeTool) Mode() agent.ExecutionMode { return f.mode }
func (f *fakeTool) Execute(context.Context, agent.ToolCall, agent.Output) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
}

// recordingTool records whether Execute ran so a denial can be shown to touch
// nothing on disk.
type recordingTool struct {
	done bool
}

func (t *recordingTool) Name() string { return "bash" }
func (t *recordingTool) Label(agent.ToolCall) string {
	return "bash: ..."
}
func (t *recordingTool) Description() string       { return "" }
func (t *recordingTool) Schema() llm.ToolSchema    { return llm.ToolSchema{Name: "bash"} }
func (t *recordingTool) Mode() agent.ExecutionMode { return agent.ModeSerial }
func (t *recordingTool) Execute(context.Context, agent.ToolCall, agent.Output) (agent.ToolResult, error) {
	t.done = true
	return agent.ToolResult{}, nil
}

// diffCatcher records every Diff emitted through a tool's output.
type diffCatcher struct {
	calls []Change
}

func (d *diffCatcher) Write(p []byte) (int, error) { return len(p), nil }
func (d *diffCatcher) Diff(path, before, after string) {
	d.calls = append(d.calls, Change{Path: path, Before: before, After: after})
}

// last returns the most recent recorded change.
func (d *diffCatcher) last() Change { return d.calls[len(d.calls)-1] }
