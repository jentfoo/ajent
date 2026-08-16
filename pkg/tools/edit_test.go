package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// diffCatcher records the last Diff a tool emitted through its output.
type diffCatcher struct {
	path, before, after string
}

func (d *diffCatcher) Write(p []byte) (int, error) { return len(p), nil }
func (d *diffCatcher) Diff(path, before, after string) {
	d.path, d.before, d.after = path, before, after
}

// TestEditDiffShowsAppliedText asserts a successful edit renders the post-edit
// text on the diff's after side, so history shows what actually changed.
func TestEditDiffShowsAppliedText(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "hello world\n")
	dc := &diffCatcher{}
	// the tracker refuses edits to a file this session has not read first
	e.readExec(context.Background(), `{"path":"a.txt"}`)
	c := agent.ToolCall{ID: "c", Name: "edit",
		Input: json.RawMessage(`{"path":"a.txt","edits":[{"oldText":"world","newText":"ajent"}]}`)}
	res, err := (&editTool{policy: e.policy, tracker: e.tracker}).Execute(context.Background(), c, dc)
	require.NoError(t, err)
	assert.False(t, res.IsError)

	assert.Equal(t, "a.txt", dc.path)
	assert.Equal(t, "hello world\n", dc.before) // pre-edit text
	assert.Equal(t, "hello ajent\n", dc.after)  // post-edit text, not the same buffer twice
}
