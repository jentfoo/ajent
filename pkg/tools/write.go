package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
)

// writeParams is the model-facing parameter block for write.
type writeParams struct {
	Path    string `json:"path"`
	Content string `json:"content" desc:"full new contents of the file"`
}

// writeTool writes a whole file atomically.
type writeTool struct {
	policy  PathPolicy
	tracker *Tracker
}

var _ agent.Tool = (*writeTool)(nil)
var _ Previewer = (*writeTool)(nil)

// Preview returns the file's current and new content for a write call, so the
// change is shown before it runs. A brand-new file previews as all additions,
// and an existing file whose bytes cannot be read previews with a marker line
// naming its stat size in place of the displaced lines.
func (t *writeTool) Preview(call agent.ToolCall) (Change, error) {
	var p writeParams
	if err := decode(call.Input, &p); err != nil {
		return Change{}, errors.New("bad args: " + err.Error())
	}
	full, err := t.policy.Resolve(p.Path)
	if err != nil {
		return Change{}, err
	}
	after := normalizeToLF(p.Content) // preview in LF space so the diff stays clean
	disp := readDisplaced(full)
	var before string
	switch disp.state {
	case dispReadable: // show the delta when overwriting too
		before = normalizeToLF(string(disp.data))
	case dispUnreadable:
		before = "[" + unreadableExisting(disp.size) + "]"
	}
	return Change{Path: p.Path, Before: before, After: after}, nil
}

func (t *writeTool) Name() string { return "write" }
func (t *writeTool) Label(agent.ToolCall) string {
	return "write"
}
func (t *writeTool) Description() string {
	return "Creates a file if it doesn't exist, overwrites with the content if it does. Automatically creates parent directories."
}
func (t *writeTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{Parameters: SchemaOf[writeParams]()}
}
func (t *writeTool) Mode() agent.ExecutionMode {
	return agent.ModeSerial
}
func (t *writeTool) Close() {}

// selfBounding: write's feedback is bounded by its own Elide pass.
func (*writeTool) selfBounding() {}

// Execute writes the file, then observes it so @ref dedupe sees current content.
func (t *writeTool) Execute(ctx context.Context, call agent.ToolCall, out agent.Output) (agent.ToolResult, error) {
	var p writeParams
	if err := decode(call.Input, &p); err != nil {
		return agent.ErrorToolResult("bad args: " + err.Error()), nil
	}
	full, err := t.policy.Resolve(p.Path)
	if err != nil {
		return agent.ErrorToolResult(err.Error()), nil
	}

	// an overwrite keeps the existing file's majority line ending, a new file gets LF
	disp := readDisplaced(full)
	data := []byte(restoreLineEndings(normalizeToLF(p.Content), detectLineEnding(disp.data)))
	if err := config.WriteFileAtomic(full, data, writePerm(full)); err != nil {
		return agent.ErrorToolResult("write: " + err.Error()), nil
	}
	t.tracker.Observe(full, data, fileInfo(full), true) // a write produces the whole file

	msg := fmt.Sprintf("wrote %s (%d lines)", p.Path, countLines(string(data)))
	// an overwrite displaced content the model never saw, since it supplied the
	// whole new file. A new file has nothing to report beyond its size, and
	// unreadable bytes have no diff, so stat's size stands in for what was lost
	switch disp.state {
	case dispReadable:
		if d := unifiedDiff(p.Path, p.Path, normalizeToLF(string(disp.data)), normalizeToLF(p.Content)); d != "" {
			msg += "\n\n" + d
		}
	case dispUnreadable: // reporting only, the write flow is unchanged
		msg += "\n\n" + unreadableExisting(disp.size) +
			"; overwritten without a displaced-content diff"
	}
	return agent.ToolResult{Content: llmBlock(msg)}, nil
}

// dispState classifies the file an overwrite would replace.
type dispState int

const (
	dispAbsent     dispState = iota // nothing verifiably there, a new file
	dispReadable                    // existing bytes are in hand
	dispUnreadable                  // exists but could not be read; size is all stat offers
)

// displaced is what overwriting a path would replace.
type displaced struct {
	state dispState
	data  []byte // set when state is dispReadable
	size  int64  // stat-derived byte count, set when state is dispUnreadable
}

// readDisplaced classifies the current state of path. A directory is never
// displaced content, writing over one fails regardless.
func readDisplaced(path string) displaced {
	b, err := os.ReadFile(path)
	if err == nil {
		return displaced{state: dispReadable, data: b}
	}
	if errors.Is(err, fs.ErrNotExist) {
		return displaced{}
	}
	fi, statErr := os.Stat(path)
	if statErr != nil || fi.IsDir() { // vanished since the read, or not a file
		return displaced{}
	}
	return displaced{state: dispUnreadable, size: fi.Size()}
}

// unreadableExisting renders the displaced-content note when only stat's byte
// count is known for what a write would replace.
func unreadableExisting(size int64) string {
	return "existing content (" + strutil.HumanSize(size) + ") could not be read"
}
