package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteNewFileCreatesParentsAndDiffs(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	res := e.writeExec(t.Context(), `{"path":"sub/out.txt","content":"hi\n"}`)
	assert.False(t, res.IsError)
	data, err := os.ReadFile(filepath.Join(e.cwd, "sub", "out.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hi\n", string(data))
}

func TestWriteRefusesUnreadOverwrite(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "original")
	res := e.writeExec(t.Context(), `{"path":"a.txt","content":"changed"}`)
	assert.True(t, res.IsError) // not read this session
	data, err := os.ReadFile(filepath.Join(e.cwd, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "original", string(data)) // untouched
}

func TestWriteAllowsAfterRead(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "old\n")
	_ = e.readExec(t.Context(), `{"path":"a.txt"}`)
	res := e.writeExec(t.Context(), `{"path":"a.txt","content":"new\n"}`)
	assert.False(t, res.IsError)
	data, err := os.ReadFile(filepath.Join(e.cwd, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "new\n", string(data))
}

func TestWriteRefusesStaleOverwriteAfterExternalChange(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "v1")
	_ = e.readExec(t.Context(), `{"path":"a.txt"}`)
	e.writeFile("a.txt", "changed externally") // not through the tracker
	res := e.writeExec(t.Context(), `{"path":"a.txt","content":"mine"}`)
	assert.True(t, res.IsError) // stale: file changed since read
}

func TestWritePreview(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	c := agent.ToolCall{ID: "c", Name: "write",
		Input: json.RawMessage(`{"path":"new.txt","content":"hello\n"}`)}
	ch, err := (&writeTool{policy: e.policy, tracker: e.tracker}).Preview(c)
	require.NoError(t, err)

	assert.Equal(t, "new.txt", ch.Path) // a new file previews as all additions
	assert.Empty(t, ch.Before)
	assert.Equal(t, "hello\n", ch.After)
}
