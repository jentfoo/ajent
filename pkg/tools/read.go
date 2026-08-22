package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// readParams is the model-facing parameter block for read.
type readParams struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty" desc:"1-based starting line; default 1"`
	Limit  int    `json:"limit,omitempty" desc:"max lines to return; defaults to the tool limit"`
}

// readTool reads a file with line numbers so edit and the model agree on
// positions. Image files are not yet supported and are refused rather than
// dumped as text.
type readTool struct {
	policy  PathPolicy
	tracker *Tracker
}

var _ agent.Tool = (*readTool)(nil)

func (t *readTool) Name() string { return "read" }

func (t *readTool) Label(agent.ToolCall) string { return "read" }

func (t *readTool) Description() string {
	return "Read the contents of a file. Returns line-numbered text; use offset/limit to page large files. Binary files are refused."
}

// Schema returns the JSON schema for read's parameters.
func (t *readTool) Schema() llm.ToolSchema { return llm.ToolSchema{Parameters: SchemaOf[readParams]()} }

func (t *readTool) Mode() agent.ExecutionMode { return agent.ModeParallel }

// Execute reads path, observing it in the tracker and returning line-numbered
// content bounded by ReadFile.
func (t *readTool) Execute(ctx context.Context, call agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	var p readParams
	if err := decode(call.Input, &p); err != nil {
		return resultErr("bad args: " + err.Error()), nil
	}
	full, err := t.policy.Resolve(p.Path)
	if err != nil {
		return resultErr(err.Error()), nil
	}

	data, info, kind, err := probeFile(full)
	if err != nil {
		return resultErr("read: " + err.Error()), nil
	}
	switch kind {
	case fileBinary:
		return resultErr("refusing to read a binary file; use bash if you need its bytes"), nil
	case fileImage:
		return resultErr("image files are not supported by the read tool yet"), nil
	}

	t.tracker.Observe(full, data, info)
	n := p.Limit
	if n <= 0 {
		n = ReadFileLimit().Lines
	}
	start := p.Offset
	if start < 1 {
		start = 1
	}
	out, lastEmitted, truncatedAt := numberLines(data, start, n)

	var b strings.Builder
	b.WriteString(out)
	if truncatedAt > 0 {
		fmt.Fprintf(&b, "\n... truncated at line %d, read again with offset=%d\n", truncatedAt, start+n)
	}
	content := b.String()

	// Display shows which section was read; Content stays the bare block so the
	// model reads no path it already supplied.
	display := content
	if lastEmitted > 0 {
		display = fmt.Sprintf("%s:%d-%d\n", relTo(t.policy.Cwd, full), start, lastEmitted) + content
	}

	return agent.ToolResult{
		Content: llmBlock(content),
		Display: display,
	}, nil
}
