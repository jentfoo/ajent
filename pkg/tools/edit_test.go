package tools

import (
	"encoding/json"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// TestEditPreview asserts an edit previews the post-edit text on the after side,
// so what is rendered is what the call would produce.
func TestEditPreview(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "hello world\n")
	c := agent.ToolCall{ID: "c", Name: "edit",
		Input: json.RawMessage(`{"path":"a.txt","edits":[{"oldText":"world","newText":"ajent"}]}`)}
	ch, err := (&editTool{policy: e.policy, tracker: e.tracker}).Preview(c)
	require.NoError(t, err)

	assert.Equal(t, "a.txt", ch.Path)
	assert.Equal(t, "hello world\n", ch.Before) // pre-edit text
	assert.Equal(t, "hello ajent\n", ch.After)  // post-edit text, not the same buffer twice
}

// TestWritePreview asserts a new file previews as all additions.
func TestWritePreview(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	c := agent.ToolCall{ID: "c", Name: "write",
		Input: json.RawMessage(`{"path":"new.txt","content":"hello\n"}`)}
	ch, err := (&writeTool{policy: e.policy, tracker: e.tracker}).Preview(c)
	require.NoError(t, err)

	assert.Equal(t, "new.txt", ch.Path)
	assert.Empty(t, ch.Before)
	assert.Equal(t, "hello\n", ch.After)
}
