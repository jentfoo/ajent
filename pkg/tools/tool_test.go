package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/jentfoo/ajent/pkg/agent"
)

// toolEnv holds a built read/write/edit toolset bound to one temp directory so
// tests share the same tracker and cwd.
type toolEnv struct {
	cwd     string
	tracker *Tracker
	policy  PathPolicy
}

func newToolEnv(cwd string) *toolEnv {
	return &toolEnv{
		cwd:     cwd,
		tracker: NewTracker(),
		policy:  PathPolicy{Cwd: cwd},
	}
}

// readExec executes the read tool with raw args.
func (e *toolEnv) readExec(ctx context.Context, args string) agent.ToolResult {
	c := agent.ToolCall{ID: "c", Name: "read", Input: json.RawMessage(args)}
	res, _ := (&readTool{policy: e.policy, tracker: e.tracker}).Execute(ctx, c, nil)
	return res
}

func (e *toolEnv) writeExec(ctx context.Context, args string) agent.ToolResult {
	c := agent.ToolCall{ID: "c", Name: "write", Input: json.RawMessage(args)}
	res, _ := (&writeTool{policy: e.policy, tracker: e.tracker}).Execute(ctx, c, nil)
	return res
}

func (e *toolEnv) editExec(ctx context.Context, args string) agent.ToolResult {
	c := agent.ToolCall{ID: "c", Name: "edit", Input: json.RawMessage(args)}
	res, _ := (&editTool{policy: e.policy, tracker: e.tracker}).Execute(ctx, c, nil)
	return res
}

func (e *toolEnv) writeFile(name, content string) {
	_ = os.WriteFile(filepath.Join(e.cwd, name), []byte(content), 0o644)
}
