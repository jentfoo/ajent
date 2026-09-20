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

// readTool reads a file with line numbers so edit and the model agree on positions.
// An image file reads as an image block on a vision model; a text-only model is
// refused with today's note so it does not waste a read.
type readTool struct {
	policy  PathPolicy
	tracker *Tracker
	// vision reports the active model's image capability live, so a /model or
	// resume that changes it never leaves this gate stale. nil means no.
	vision func() bool
}

// visionOn reports whether image reads may return image blocks.
func (t *readTool) visionOn() bool { return t.vision != nil && t.vision() }

var _ agent.Tool = (*readTool)(nil)

func (t *readTool) Name() string { return ToolRead }

func (t *readTool) Label(agent.ToolCall) string { return ToolRead }

func (t *readTool) Description() string {
	return "Read the contents of a file. Returns line-numbered text; use offset/limit to page large files. Image files return the picture itself; binary files are refused."
}

// Schema returns the JSON schema for read's parameters.
func (t *readTool) Schema() llm.ToolSchema { return llm.ToolSchema{Parameters: SchemaOf[readParams]()} }

func (t *readTool) Mode() agent.ExecutionMode { return agent.ModeParallel }

// selfBounding: read bounds its own window and pages with offset; no spill.
func (*readTool) selfBounding() {}

// Execute reads path, observing it in the tracker and returning line-numbered
// content bounded by ReadFile. Oversized output never spills: the footer pages
// with offset instead. A range wider than the limit is an error result.
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
		if !t.visionOn() {
			return resultErr("model does not support images; this file can only be read by a vision model"), nil
		}
		t.tracker.Observe(full, data, info) // an image read dedupes like a text read
		return t.readImage(full, data)
	}

	lim := ReadFileLimit()
	start := p.Offset
	if start < 1 {
		start = 1
	}
	n := p.Limit
	if n <= 0 {
		n = lim.Lines
	}
	if n > lim.Lines {
		return resultErr(fmt.Sprintf(
			"read: limit %d exceeds the maximum of %d lines; narrow the range and page with offset", n, lim.Lines)), nil
	}

	t.tracker.Observe(full, data, info)
	out, lastEmitted, truncatedAt, total := numberLines(data, start, n, lim.Bytes)

	var b strings.Builder
	b.WriteString(out)
	if truncatedAt > 0 {
		_, _ = fmt.Fprintf(&b, "\n... truncated at line %d of %d (%d more); read again with offset=%d\n",
			truncatedAt, total, total-truncatedAt, truncatedAt+1)
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

// readImage fits one image file and returns it as model content. A rejected
// image becomes an error result naming why; a fitted one carries the sizing
// note so answers stay honest against the source pixels.
func (t *readTool) readImage(full string, data []byte) (agent.ToolResult, error) {
	blocks, err := FitImage(data)
	if err != nil {
		return resultErr(FitImageError(err)), nil
	}
	var notes strings.Builder
	notes.WriteString(relTo(t.policy.Cwd, full))
	for _, b := range blocks {
		if tb, ok := b.(llm.TextBlock); ok {
			notes.WriteString("\n")
			notes.WriteString(tb.Text)
		}
	}
	return agent.ToolResult{Content: blocks, Display: notes.String()}, nil
}
