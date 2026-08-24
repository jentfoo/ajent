package projinit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/stretchr/testify/require"
)

// stubTool is a scriptable stand-in for agent_start and agent_poll, recording
// every call so a test can assert what the survey asked for.
type stubTool struct {
	name string
	exec func(call agent.ToolCall) agent.ToolResult

	mu    sync.Mutex
	calls []agent.ToolCall
}

func (t *stubTool) Name() string                { return t.name }
func (t *stubTool) Label(agent.ToolCall) string { return t.name }
func (t *stubTool) Description() string         { return "stub" }
func (t *stubTool) Schema() llm.ToolSchema      { return llm.ToolSchema{Name: t.name} }
func (t *stubTool) Mode() agent.ExecutionMode   { return agent.ModeParallel }
func (t *stubTool) Execute(_ context.Context, call agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	t.mu.Lock()
	t.calls = append(t.calls, call)
	t.mu.Unlock()
	return t.exec(call), nil
}

// inputs returns the recorded call arguments, decoded.
func (t *stubTool) inputs() []map[string]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]map[string]string, 0, len(t.calls))
	for _, c := range t.calls {
		var m map[string]string
		_ = json.Unmarshal(c.Input, &m)
		out = append(out, m)
	}
	return out
}

// startStub hands out sub-1, sub-2, ... in call order.
func startStub() *stubTool {
	var n int
	var mu sync.Mutex
	return &stubTool{name: "agent_start", exec: func(agent.ToolCall) agent.ToolResult {
		mu.Lock()
		n++
		id := "sub-" + strconv.Itoa(n)
		mu.Unlock()
		return agent.ToolResult{
			Content: llm.BlockList{llm.TextBlock{Text: "Sub-agent " + id + " started."}},
			Details: map[string]string{"id": id},
		}
	}}
}

// pollStub reports done immediately, echoing the id in its summary.
func pollStub() *stubTool {
	return &stubTool{name: "agent_poll", exec: func(call agent.ToolCall) agent.ToolResult {
		var p map[string]string
		_ = json.Unmarshal(call.Input, &p)
		return agent.ToolResult{
			Content: llm.BlockList{llm.TextBlock{Text: "summary of " + p["id"]}},
			Details: map[string]string{"id": p["id"], "status": "done"},
		}
	}}
}

// newRegistry returns the built-in tools rooted at cwd plus the given stubs.
func newRegistry(t *testing.T, cwd string, stubs ...agent.Tool) *tools.Registry {
	t.Helper()
	reg, err := tools.Builtins(tools.Options{Cwd: cwd})
	require.NoError(t, err)
	for _, s := range stubs {
		reg.RegisterFrom(tools.SourceBuiltin, s, true)
	}
	return reg
}

// writeTree creates each named file under dir with placeholder content.
func writeTree(t *testing.T, dir string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		full := filepath.Join(dir, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte("package x\n"), 0o644))
	}
}

// toolNames returns the tool name of every call block in msgs, in order.
func toolNames(msgs []llm.Message) []string {
	var out []string
	for _, m := range msgs {
		for _, b := range m.Content {
			if cb, ok := b.(llm.ToolCallBlock); ok {
				out = append(out, cb.Name)
			}
		}
	}
	return out
}

// resultTexts returns the text of every tool result block in msgs, in order.
func resultTexts(msgs []llm.Message) []string {
	var out []string
	for _, m := range msgs {
		for _, b := range m.Content {
			rb, ok := b.(llm.ToolResultBlock)
			if !ok {
				continue
			}
			var text strings.Builder
			for _, c := range rb.Content {
				if tb, tok := c.(llm.TextBlock); tok {
					text.WriteString(tb.Text)
				}
			}
			out = append(out, text.String())
		}
	}
	return out
}

// decodeID reads the id argument of an agent_poll call.
func decodeID(input []byte) string {
	var p map[string]string
	_ = json.Unmarshal(input, &p)
	return p["id"]
}

// count reports how many entries of s equal want.
func count(s []string, want string) int {
	var n int
	for _, v := range s {
		if v == want {
			n++
		}
	}
	return n
}

// callIDs returns the tool_use id of every call block in msgs.
func callIDs(msgs []llm.Message) []string {
	var out []string
	for _, m := range msgs {
		for _, b := range m.Content {
			if cb, ok := b.(llm.ToolCallBlock); ok {
				out = append(out, cb.ID)
			}
		}
	}
	return out
}
