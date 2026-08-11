package tools

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingTool records whether Execute ran, so a denial can be shown to touch
// nothing on disk.
type recordingTool struct {
	ran bool
}

func (t *recordingTool) Name() string { return "bash" }
func (t *recordingTool) Label(agent.ToolCall) string {
	return "bash: ..."
}
func (t *recordingTool) Description() string       { return "" }
func (t *recordingTool) Schema() llm.ToolSchema    { return llm.ToolSchema{Name: "bash"} }
func (t *recordingTool) Mode() agent.ExecutionMode { return agent.ModeSerial }
func (t *recordingTool) Execute(context.Context, agent.ToolCall, agent.Output) (agent.ToolResult, error) {
	t.ran = true
	return agent.ToolResult{}, nil
}

// TestGuardDenyProducesErrorAndNoSideEffect asserts a denied call returns an
// error result carrying the reason and never reaches the wrapped tool.
func TestGuardDenyProducesErrorAndNoSideEffect(t *testing.T) {
	t.Parallel()

	r := New()
	inner := &recordingTool{}
	r.Register(inner, true)
	r.AddGuard(func(context.Context, agent.ToolCall) Decision {
		return Deny("bash is blocked")
	})

	tool, ok := r.Get("bash")
	require.True(t, ok)

	res, err := tool.Execute(t.Context(), callWith(json.RawMessage(`{}`)), nil)
	require.NoError(t, err) // a denial is a result, not an error
	assert.True(t, res.IsError)
	assert.Contains(t, textOf(res), "bash is blocked")
	assert.False(t, inner.ran) // nothing executed, so disk was untouched
}

// TestGuardAllowPassesThrough asserts a passing guard lets the tool run.
func TestGuardAllowPassesThrough(t *testing.T) {
	t.Parallel()

	r := New()
	inner := &recordingTool{}
	r.Register(inner, true)
	r.AddGuard(func(context.Context, agent.ToolCall) Decision { return Allow(callWith(nil)) })

	tool, _ := r.Get("bash")
	res, err := tool.Execute(t.Context(), callWith(json.RawMessage(`{}`)), nil)
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.True(t, inner.ran)
}

// TestGuardAskWithoutAskerTreatedAsDenial asserts an Ask decision refuses when
// no asker is registered.
func TestGuardAskWithoutAskerTreatedAsDenial(t *testing.T) {
	t.Parallel()

	r := New()
	inner := &recordingTool{}
	r.Register(inner, true)
	r.AddGuard(func(context.Context, agent.ToolCall) Decision {
		return Decision{Action: ActionAsk, Reason: "needs approval"}
	})

	tool, _ := r.Get("bash")
	res, err := tool.Execute(t.Context(), callWith(json.RawMessage(`{}`)), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, textOf(res), "needs approval")
	assert.False(t, inner.ran)
}

func TestGuardChainFirstNonAllowWins(t *testing.T) {
	t.Parallel()

	r := New()
	inner := &recordingTool{}
	r.Register(inner, true)
	r.AddGuard(func(context.Context, agent.ToolCall) Decision { return Allow(callWith(nil)) })
	r.AddGuard(func(context.Context, agent.ToolCall) Decision { return Deny("second guard") })

	tool, _ := r.Get("bash")
	res, _ := tool.Execute(t.Context(), callWith(json.RawMessage(`{}`)), nil)
	assert.True(t, res.IsError)
	assert.Contains(t, textOf(res), "second guard")
}

func TestGuardDeniedCallLeavesFileOnDiskUntouched(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/keep.txt"
	require.NoError(t, os.WriteFile(path, []byte("original"), 0o644))

	r := New()
	r.Register(&fakeTool{name: "write"}, true)
	r.AddGuard(func(context.Context, agent.ToolCall) Decision { return Deny("no writes") })

	tool, _ := r.Get("write")
	res, err := tool.Execute(t.Context(), callWith(json.RawMessage(`{"path":"`+path+`","content":"changed"}`)), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)

	data, _ := os.ReadFile(path)
	assert.Equal(t, "original", string(data)) // nothing on disk changed
}
