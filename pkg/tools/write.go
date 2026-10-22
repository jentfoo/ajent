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

// resolveTarget decodes the call and resolves its target path, shared by
// Preview and Execute so both report identical failures. The returned error is
// model-facing as-is.
func (t *writeTool) resolveTarget(call agent.ToolCall) (writeParams, string, error) {
	var p writeParams
	if err := decode(call.Input, &p); err != nil {
		return writeParams{}, "", errors.New("bad args: " + err.Error())
	}
	full, err := t.policy.Resolve(p.Path)
	if err != nil {
		return writeParams{}, "", err
	}
	return p, full, nil
}

// Preview returns the file's current and new content for a write call, so the
// change is shown before it runs. A brand-new file previews as all additions,
// an existing file whose bytes cannot be read previews a marker naming its
// stat size, and a special file is refused as Execute refuses it.
func (t *writeTool) Preview(call agent.ToolCall) (Change, error) {
	p, full, err := t.resolveTarget(call)
	if err != nil {
		return Change{}, err
	}
	if err := refuseSpecial(full); err != nil {
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
	p, full, err := t.resolveTarget(call)
	if err != nil {
		return agent.ErrorToolResult(err.Error()), nil
	}
	// a write that cannot read its target must not destroy it: the rename
	// would unlink the node and leave a regular file in its place. fi feeds
	// writePerm, so one stat serves both the refusal and mode preservation
	fi, err := statRegular(full)
	if err != nil {
		return agent.ErrorToolResult("write: " + err.Error()), nil
	}

	norm := normalizeToLF(p.Content) // one pass for both the bytes written and the diff below
	// an overwrite keeps the existing file's majority line ending, a new file gets LF
	disp := readDisplaced(full)
	data := []byte(restoreLineEndings(norm, detectLineEnding(disp.data)))
	if err := config.WriteFileAtomic(full, data, writePerm(fi)); err != nil {
		return agent.ErrorToolResult("write: " + err.Error()), nil
	}
	t.tracker.Observe(full, data, fileInfo(full), true) // a write produces the whole file

	msg := fmt.Sprintf("wrote %s (%d lines)", p.Path, countLines(string(data)))
	// an overwrite displaced content the model never saw, since it supplied the
	// whole new file. A new file has nothing to report beyond its size, and
	// unreadable bytes have no diff, so stat's size stands in for what was lost
	switch disp.state {
	case dispReadable:
		if d := unifiedDiff(p.Path, p.Path, normalizeToLF(string(disp.data)), norm); d != "" {
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
// displaced content, writing over one fails regardless. A special file reads
// as unreadable, keeping stat's size so its note stays honest.
func readDisplaced(path string) displaced {
	if fi, err := os.Stat(path); err == nil && specialFile(fi.Mode()) {
		return displaced{state: dispUnreadable, size: fi.Size()}
	}
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
