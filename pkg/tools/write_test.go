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

func TestWrite(t *testing.T) {
	t.Parallel()

	// a new file creates parent dirs and writes content.
	t.Run("new_file_creates_parents_and_diffs", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		res := e.writeExec(t.Context(), `{"path":"sub/out.txt","content":"hi\n"}`)
		assert.False(t, res.IsError)
		data, err := os.ReadFile(filepath.Join(e.cwd, "sub", "out.txt"))
		require.NoError(t, err)
		assert.Equal(t, "hi\n", string(data))
	})

	// an unread file may still be overwritten (content may come from grep/sed).
	t.Run("allows_unread_overwrite", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		e.writeFile("a.txt", "original")
		res := e.writeExec(t.Context(), `{"path":"a.txt","content":"changed"}`)
		assert.False(t, res.IsError) // no read gate: content may come from grep/sed
		data, err := os.ReadFile(filepath.Join(e.cwd, "a.txt"))
		require.NoError(t, err)
		assert.Equal(t, "changed", string(data)) // applied
	})

	t.Run("allows_after_read", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		e.writeFile("a.txt", "old\n")
		_ = e.readExec(t.Context(), `{"path":"a.txt"}`)
		res := e.writeExec(t.Context(), `{"path":"a.txt","content":"new\n"}`)
		assert.False(t, res.IsError)
		data, err := os.ReadFile(filepath.Join(e.cwd, "a.txt"))
		require.NoError(t, err)
		assert.Equal(t, "new\n", string(data))
	})

	t.Run("overwrites_after_external_change", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		e.writeFile("a.txt", "v1")
		_ = e.readExec(t.Context(), `{"path":"a.txt"}`)
		e.writeFile("a.txt", "changed externally") // not through the tracker
		res := e.writeExec(t.Context(), `{"path":"a.txt","content":"mine"}`)
		assert.False(t, res.IsError) // no stale gate: write overwrites as told
	})

	t.Run("preview", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		c := agent.ToolCall{ID: "c", Name: "write",
			Input: json.RawMessage(`{"path":"new.txt","content":"hello\n"}`)}
		ch, err := (&writeTool{policy: e.policy, tracker: e.tracker}).Preview(c)
		require.NoError(t, err)

		assert.Equal(t, "new.txt", ch.Path) // a new file previews as all additions
		assert.Empty(t, ch.Before)
		assert.Equal(t, "hello\n", ch.After)
	})

	t.Run("preserves_existing_perms", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		p := filepath.Join(e.cwd, "secret.txt")
		require.NoError(t, os.WriteFile(p, []byte("old\n"), 0o600))

		res := e.writeExec(t.Context(), `{"path":"secret.txt","content":"new\n"}`)
		assert.False(t, res.IsError)
		fi, err := os.Stat(p)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm()) // owner-only mode kept
	})

	// LF content written over a CRLF file restores CRLF so the document's convention survives.
	t.Run("overwrite_crlf_keeps_line_ending", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		p := filepath.Join(e.cwd, "a.txt")
		require.NoError(t, os.WriteFile(p, []byte("old\r\n"), 0o644))

		res := e.writeExec(t.Context(), `{"path":"a.txt","content":"new line here\n"}`)
		assert.False(t, res.IsError)

		data, err := os.ReadFile(p)
		require.NoError(t, err)
		assert.Equal(t, "new line here\r\n", string(data)) // CRLF restored on write
	})

	// a brand-new file defaults to LF.
	t.Run("new_file_gets_lf", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		res := e.writeExec(t.Context(), `{"path":"fresh.txt","content":"one\ntwo\n"}`)
		assert.False(t, res.IsError)

		data, err := os.ReadFile(filepath.Join(e.cwd, "fresh.txt"))
		require.NoError(t, err)
		assert.Equal(t, "one\ntwo\n", string(data))
	})
}
