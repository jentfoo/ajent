package tools

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
)

// writeParams is the model-facing parameter block for write.
type writeParams struct {
	Path    string `json:"path"`
	Content string `json:"content" desc:"full new contents of the file"`
}

// writeTool writes a whole file atomically. It refuses to overwrite a file this
// session has not read, forcing the model to look before it clobbers.
type writeTool struct {
	policy  PathPolicy
	tracker *Tracker
}

var _ agent.Tool = (*writeTool)(nil)
var _ Previewer = (*writeTool)(nil)

// Preview returns the path with the file's current and new content for a write
// call, so an approval dialog can show what would be written (a brand-new file
// previews as all additions).
func (t *writeTool) Preview(call agent.ToolCall) (path, before, after string, err error) {
	var p writeParams
	if err := decode(call.Input, &p); err != nil {
		return "", "", "", errors.New("bad args: " + err.Error())
	}
	full, err := t.policy.Resolve(p.Path)
	if err != nil {
		return "", "", "", err
	}
	var existing string
	if _, statErr := os.Stat(full); statErr == nil { // show the delta when overwriting too
		b, _ := readAllFile(full)
		existing = string(b)
	}
	return p.Path, existing, p.Content, nil
}

func (t *writeTool) Name() string { return "write" }
func (t *writeTool) Label(agent.ToolCall) string {
	return "write"
}
func (t *writeTool) Description() string {
	return "Write content to a file. Creates the file if it doesn't exist, overwrites if it does. Automatically creates parent directories. Read an existing file first."
}
func (t *writeTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{Parameters: SchemaOf[writeParams]()}
}
func (t *writeTool) Mode() agent.ExecutionMode {
	return agent.ModeSerial
}

// Execute writes the file, emitting a diff through out and refusing stale or
// unread overwrites.
func (t *writeTool) Execute(ctx context.Context, call agent.ToolCall, out agent.Output) (agent.ToolResult, error) {
	out = ensureOutput(out)
	var p writeParams
	if err := decode(call.Input, &p); err != nil {
		return resultErr("bad args: " + err.Error()), nil
	}
	full, err := t.policy.Resolve(p.Path)
	if err != nil {
		return resultErr(err.Error()), nil
	}

	var before string
	if _, statErr := os.Stat(full); statErr == nil { // existing file needs a fresh read
		if ck := t.tracker.Check(full); ck != nil {
			return resultErr("refusing to overwrite: " + ck.Error()), nil
		}
		beforeBytes, _ := readAllFile(full)
		before = string(beforeBytes)
	}

	data := []byte(p.Content)
	if err := config.WriteFileAtomic(full, data, 0o644); err != nil {
		return resultErr("write: " + err.Error()), nil
	}
	t.tracker.Observe(full, data, fileInfo(full))
	out.Diff(p.Path, before, string(data))

	return agent.ToolResult{
		Content: llmBlock(fmt.Sprintf("wrote %s (%d lines)", p.Path, countLines(string(data)))),
	}, nil
}
