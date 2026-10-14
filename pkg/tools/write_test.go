package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
)

func TestWrite(t *testing.T) {
	t.Parallel()

	// a new file creates parent dirs and writes content
	t.Run("new_file_creates_parents_and_diffs", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		res := e.writeExec(t.Context(), `{"path":"sub/out.txt","content":"hi\n"}`)
		assert.False(t, res.IsError)
		data, err := os.ReadFile(filepath.Join(e.cwd, "sub", "out.txt"))
		require.NoError(t, err)
		assert.Equal(t, "hi\n", string(data))
	})

	// an unread file may still be overwritten (content may come from grep/sed)
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

	// LF content written over a CRLF file restores CRLF so the document's convention survives
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

	// a brand-new file defaults to LF
	t.Run("new_file_gets_lf", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		res := e.writeExec(t.Context(), `{"path":"fresh.txt","content":"one\ntwo\n"}`)
		assert.False(t, res.IsError)

		data, err := os.ReadFile(filepath.Join(e.cwd, "fresh.txt"))
		require.NoError(t, err)
		assert.Equal(t, "one\ntwo\n", string(data))
	})
}

func TestReadDisplaced(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")

	t.Run("absent", func(t *testing.T) {
		d := readDisplaced(p)
		assert.Equal(t, dispAbsent, d.state)
	})

	t.Run("readable", func(t *testing.T) {
		require.NoError(t, os.WriteFile(p, []byte("old\n"), 0o644))
		t.Cleanup(func() { _ = os.Remove(p) })

		d := readDisplaced(p)
		assert.Equal(t, dispReadable, d.state)
		assert.Equal(t, "old\n", string(d.data))
	})

	t.Run("unreadable", func(t *testing.T) {
		unreadableFileSkip(t)
		writeChmod000(t, p, "hidden\n")
		t.Cleanup(func() { _ = os.Remove(p) })

		d := readDisplaced(p)
		assert.Equal(t, dispUnreadable, d.state)
		assert.Nil(t, d.data)
		assert.Equal(t, int64(7), d.size)
	})

	t.Run("directory", func(t *testing.T) {
		sub := filepath.Join(dir, "sub")
		require.NoError(t, os.Mkdir(sub, 0o755))

		d := readDisplaced(sub)
		assert.Equal(t, dispAbsent, d.state) // a directory is not displaced content
	})
}

func TestWriteReportsDisplacedContent(t *testing.T) {
	t.Parallel()

	t.Run("overwrite_shows_diff", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		e.writeFile("a.txt", "keep\nold line\ntail\n")

		res := e.writeExec(t.Context(), `{"path":"a.txt","content":"keep\nnew line\ntail\n"}`)
		require.False(t, res.IsError)

		out := textOf(res)
		assert.Contains(t, out, "wrote a.txt")
		assert.Contains(t, out, "-old line")
		assert.Contains(t, out, "+new line")
	})

	t.Run("new_file_has_no_diff", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		res := e.writeExec(t.Context(), `{"path":"fresh.txt","content":"only\n"}`)
		require.False(t, res.IsError)

		out := textOf(res)
		assert.Contains(t, out, "wrote fresh.txt")
		assert.NotContains(t, out, "@@") // nothing was displaced
	})

	t.Run("identical_overwrite_has_no_diff", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		e.writeFile("a.txt", "same\n")
		res := e.writeExec(t.Context(), `{"path":"a.txt","content":"same\n"}`)
		require.False(t, res.IsError)
		assert.NotContains(t, textOf(res), "@@")
	})
}

func TestWriteUnreadableOverwrite(t *testing.T) {
	t.Parallel()

	t.Run("execute_reports_size_not_diff", func(t *testing.T) {
		unreadableFileSkip(t)
		e := newToolEnv(t.TempDir())
		p := filepath.Join(e.cwd, "locked.txt")
		writeChmod000(t, p, "hidden\n")

		res := e.writeExec(t.Context(), `{"path":"locked.txt","content":"new\n"}`)
		require.False(t, res.IsError) // reporting only: the write proceeds

		out := textOf(res)
		assert.Contains(t, out, "wrote locked.txt")
		assert.Contains(t, out, "existing content (7b) could not be read")
		assert.NotContains(t, out, "@@") // no displaced-content diff can exist

		require.NoError(t, os.Chmod(p, 0o644)) // the overwrite kept mode 000
		data, err := os.ReadFile(p)
		require.NoError(t, err)
		assert.Equal(t, "new\n", string(data))
	})

	t.Run("preview_marks_displaced_side", func(t *testing.T) {
		unreadableFileSkip(t)
		e := newToolEnv(t.TempDir())
		p := filepath.Join(e.cwd, "locked.txt")
		writeChmod000(t, p, "hidden\n")

		c := agent.ToolCall{ID: "c", Name: "write",
			Input: json.RawMessage(`{"path":"locked.txt","content":"new\n"}`)}
		ch, err := (&writeTool{policy: e.policy, tracker: e.tracker}).Preview(c)
		require.NoError(t, err)

		assert.Equal(t, "[existing content (7b) could not be read]", ch.Before)
		assert.Equal(t, "new\n", ch.After)
	})
}

// unreadableFileSkip skips where mode bits cannot hide file contents, as for root.
func unreadableFileSkip(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file mode bits cannot make a file unreadable here")
	}
}

// writeChmod000 creates p with content and strips every permission bit.
func writeChmod000(t *testing.T, p, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	require.NoError(t, os.Chmod(p, 0o000))
}
