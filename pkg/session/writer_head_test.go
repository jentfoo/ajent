package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriterSetHeadPersistsCursor(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := Create(p, SessionData{Version: sessionVersion})
	require.NoError(t, err)
	e1, err := w.Append(TypeMessage, MessageData{Message: llmText("one")})
	require.NoError(t, err)

	w.SetHead(e1.ID) // rewind onto the first message

	cur, ok := ReadHead(filepath.Dir(p))
	require.True(t, ok)
	assert.Equal(t, e1.ID, cur.ID)
	require.NoError(t, w.Close())
}

func TestWriterSyncPersistsCursorAtTurnBoundary(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := Create(p, SessionData{Version: sessionVersion})
	require.NoError(t, err)
	e1, err := w.Append(TypeMessage, MessageData{Message: llmText("one")})
	require.NoError(t, err)

	// no SetHead; Sync alone must record the appended head
	require.NoError(t, w.Sync())

	cur, ok := ReadHead(filepath.Dir(p))
	require.True(t, ok)
	assert.Equal(t, e1.ID, cur.ID)
}

func TestWriterOpenRecoversPersistedBranchNotTail(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := Create(p, SessionData{Version: sessionVersion})
	require.NoError(t, err)

	e1, _ := w.Append(TypeMessage, MessageData{Message: llmText("one")})
	e2, _ := w.Append(TypeMessage, MessageData{Message: llmText("two")}) // tail
	w.SetHead(e1.ID)                                                     // fork back to one
	require.NoError(t, w.Close())

	// the file tail is e2, but HEAD points at e1; a reopen must resume from e1.
	w2, oerr := Open(p)
	require.NoError(t, oerr)
	assert.Equal(t, e1.ID, w2.Head(), "reopen resumes the persisted branch")
	e3, err := w2.Append(TypeMessage, MessageData{Message: llmText("three")})
	require.NoError(t, err)
	assert.Equal(t, e1.ID, e3.ParentID)

	// and that fork is now a new tip alongside the abandoned one
	entries, _, rerr := Read(p)
	require.NoError(t, rerr)
	tips := Tips(entries)
	ids := make([]string, len(tips))
	for i, tp := range tips {
		ids[i] = tp.ID
	}
	assert.Equal(t, []string{e2.ID, e3.ID}, ids, "both the abandoned tip and the new one survive")
}

func TestWriterDiscardWritesNoHeadCursor(t *testing.T) {
	t.Parallel()

	w := Discard()
	e, err := w.Append(TypeMessage, MessageData{Message: llmText("x")})
	require.NoError(t, err)
	w.SetHead(e.ID)
	assert.False(t, fileExists(headPath(filepath.Dir(w.path))), "a discard writer never writes HEAD")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
