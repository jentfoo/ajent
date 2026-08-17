package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// editCall builds an edit call replacing old with new in path.
func editCall(path, old, new string) agent.ToolCall {
	args, _ := json.Marshal(editParams{Path: path, Edits: []editOp{{OldText: old, NewText: new}}})
	return agent.ToolCall{ID: "c", Name: "edit", Input: args}
}

// guardedEdit registers edit against a fresh registry and returns its guarded
// wrapper plus the env backing it.
func guardedEdit(t *testing.T) (agent.Tool, *toolEnv) {
	t.Helper()
	e := newToolEnv(t.TempDir())
	r := New()
	r.Register(&editTool{policy: e.policy, tracker: e.tracker}, true)
	tool, ok := r.Get("edit")
	require.True(t, ok)
	return tool, e
}

// TestGuardedToolPreviewOrdering pins when the change is rendered: before the
// guard chain runs, so an approval dialog opens below the full diff.
func TestGuardedToolPreviewOrdering(t *testing.T) {
	t.Parallel()

	t.Run("renders_before_the_guard", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		e.writeFile("a.txt", "hello world\n")
		e.readExec(context.Background(), `{"path":"a.txt"}`)

		r := New()
		r.Register(&editTool{policy: e.policy, tracker: e.tracker}, true)
		dc := &diffCatcher{}
		var diffsAtGuard int
		r.AddGuard(func(context.Context, agent.ToolCall) Decision {
			diffsAtGuard = len(dc.calls)
			return Allow(agent.ToolCall{})
		})
		tool, ok := r.Get("edit")
		require.True(t, ok)

		res, err := tool.Execute(context.Background(), editCall("a.txt", "world", "ajent"), dc)
		require.NoError(t, err)
		assert.False(t, res.IsError)

		assert.Equal(t, 1, diffsAtGuard) // already rendered by the time the guard ran
		require.Len(t, dc.calls, 1)      // and not a second time from Execute
		assert.Equal(t, "hello world\n", dc.last().Before)
		assert.Equal(t, "hello ajent\n", dc.last().After)
	})

	t.Run("renders_even_when_denied", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		e.writeFile("a.txt", "hello world\n")
		e.readExec(context.Background(), `{"path":"a.txt"}`)

		r := New()
		r.Register(&editTool{policy: e.policy, tracker: e.tracker}, true)
		r.AddGuard(func(context.Context, agent.ToolCall) Decision { return Deny("nope") })
		tool, ok := r.Get("edit")
		require.True(t, ok)

		dc := &diffCatcher{}
		res, err := tool.Execute(context.Background(), editCall("a.txt", "world", "ajent"), dc)
		require.NoError(t, err)
		assert.True(t, res.IsError) // the record shows what was proposed and refused
		require.Len(t, dc.calls, 1)
		assert.Equal(t, "hello ajent\n", dc.last().After)

		got, readErr := readAllFile(e.policy.Cwd + "/a.txt")
		require.NoError(t, readErr)
		assert.Equal(t, "hello world\n", string(got)) // nothing touched disk
	})

	t.Run("renders_once_when_asker_reasks", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		e.writeFile("a.txt", "hello world\n")
		e.readExec(context.Background(), `{"path":"a.txt"}`)

		r := New()
		r.Register(&editTool{policy: e.policy, tracker: e.tracker}, true)
		r.AddGuard(func(context.Context, agent.ToolCall) Decision {
			return Decision{Action: ActionAsk, Reason: "approval"}
		})
		r.SetAsker(func(context.Context, agent.ToolCall, Decision) Decision {
			return Decision{Action: ActionAsk}
		})
		tool, ok := r.Get("edit")
		require.True(t, ok)

		dc := &diffCatcher{}
		_, err := tool.Execute(context.Background(), editCall("a.txt", "world", "ajent"), dc)
		require.NoError(t, err)
		assert.Len(t, dc.calls, 1)
	})

	t.Run("bad_args_render_nothing", func(t *testing.T) {
		tool, _ := guardedEdit(t)
		dc := &diffCatcher{}
		c := agent.ToolCall{ID: "c", Name: "edit", Input: json.RawMessage(`{"path":`)}
		res, err := tool.Execute(context.Background(), c, dc)
		require.NoError(t, err)
		assert.True(t, res.IsError)
		assert.Empty(t, dc.calls)
	})

	t.Run("nil_output_is_safe", func(t *testing.T) {
		tool, e := guardedEdit(t)
		e.writeFile("a.txt", "hello world\n")
		e.readExec(context.Background(), `{"path":"a.txt"}`)
		_, err := tool.Execute(context.Background(), editCall("a.txt", "world", "ajent"), nil)
		require.NoError(t, err)
	})
}

// TestGuardedToolPreviewSkipsNonPreviewers asserts a tool without a Preview runs
// untouched and renders nothing.
func TestGuardedToolPreviewSkipsNonPreviewers(t *testing.T) {
	t.Parallel()

	r := New()
	inner := &recordingTool{}
	r.Register(inner, true)
	tool, ok := r.Get("bash")
	require.True(t, ok)

	dc := &diffCatcher{}
	_, err := tool.Execute(t.Context(), callWith(json.RawMessage(`{}`)), dc)
	require.NoError(t, err)
	assert.True(t, inner.ran)
	assert.Empty(t, dc.calls)
}
