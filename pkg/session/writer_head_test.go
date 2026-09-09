package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriterHeadCursor(t *testing.T) {
	t.Parallel()

	// SetHead persists the cursor immediately.
	t.Run("set_head_persists_cursor", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := Create(p, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		e1, err := w.Append(TypeMessage, MessageData{Message: llmText("one")})
		require.NoError(t, err)

		w.SetHead(e1.ID) // rewind onto the first message

		id, ok := readHead(p)
		require.True(t, ok)
		assert.Equal(t, e1.ID, id)
		require.NoError(t, w.Close())
	})

	// a fork to a new root leaves no cursor to point back at the abandoned branch.
	t.Run("empty_head_drops_cursor", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := Create(p, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		t.Cleanup(func() { _ = w.Close() })
		e1, err := w.Append(TypeMessage, MessageData{Message: llmText("one")})
		require.NoError(t, err)
		w.SetHead(e1.ID)
		require.True(t, fileExists(headPath(p)))

		w.SetHead("")

		assert.False(t, fileExists(headPath(p)))
	})

	// Sync alone must record the appended head at a turn boundary.
	t.Run("sync_persists_cursor_at_turn_boundary", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := Create(p, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		t.Cleanup(func() { _ = w.Close() })
		e1, err := w.Append(TypeMessage, MessageData{Message: llmText("one")})
		require.NoError(t, err)

		// no SetHead; Sync alone must record the appended head
		require.NoError(t, w.Sync())

		id, ok := readHead(p)
		require.True(t, ok)
		assert.Equal(t, e1.ID, id)
	})

	// a reopen resumes the persisted branch rather than the file tail.
	t.Run("open_recovers_persisted_branch_not_tail", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := Create(p, SessionData{Version: sessionVersion})
		require.NoError(t, err)

		e1, aerr := w.Append(TypeMessage, MessageData{Message: llmText("one")})
		require.NoError(t, aerr)
		e2, aerr := w.Append(TypeMessage, MessageData{Message: llmText("two")}) // tail
		require.NoError(t, aerr)
		w.SetHead(e1.ID) // fork back to one
		require.NoError(t, w.Close())

		// the file tail is e2, but the cursor points at e1; a reopen must resume from e1.
		w2, oerr := Open(p)
		require.NoError(t, oerr)
		assert.Equal(t, e1.ID, w2.Head())
		e3, err := w2.Append(TypeMessage, MessageData{Message: llmText("three")})
		require.NoError(t, err)
		assert.Equal(t, e1.ID, e3.ParentID)

		// and that fork is now a new tip alongside the abandoned one
		entries, _, rerr := Read(p)
		require.NoError(t, rerr)
		// both the abandoned tip and the new fork stay reachable.
		assert.Equal(t, []string{e2.ID, e3.ID}, tipIDs(entries))
	})

	// a sibling session in the same directory keeps its own branch.
	t.Run("sibling_session_keeps_its_branch", func(t *testing.T) {
		dir := t.TempDir()
		pa := filepath.Join(dir, "a.jsonl")
		wa, err := Create(pa, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		a1, err := wa.Append(TypeMessage, MessageData{Message: llmText("one")})
		require.NoError(t, err)
		_, err = wa.Append(TypeMessage, MessageData{Message: llmText("two")}) // tail
		require.NoError(t, err)
		wa.SetHead(a1.ID) // rewind, then leave this session alone
		require.NoError(t, wa.Close())

		// a second session syncing afterwards must not clear a's cursor
		pb := filepath.Join(dir, "b.jsonl")
		wb, err := Create(pb, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		t.Cleanup(func() { _ = wb.Close() })
		_, err = wb.Append(TypeMessage, MessageData{Message: llmText("other")})
		require.NoError(t, err)
		require.NoError(t, wb.Sync())

		wa2, oerr := Open(pa)
		require.NoError(t, oerr)
		t.Cleanup(func() { _ = wa2.Close() })
		assert.Equal(t, a1.ID, wa2.Head())
	})

	// a discard writer never writes a cursor.
	t.Run("discard_writes_no_head_cursor", func(t *testing.T) {
		w := Discard()
		e, err := w.Append(TypeMessage, MessageData{Message: llmText("x")})
		require.NoError(t, err)
		w.SetHead(e.ID)
		assert.False(t, fileExists(headPath(w.path)))
	})
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
