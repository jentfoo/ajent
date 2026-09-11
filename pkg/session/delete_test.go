package session

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deleteWorkspace points the cwd and AJENT_HOME at fresh temp dirs so a case sees
// only the sessions it writes.
func deleteWorkspace(t *testing.T) (*Store, string) {
	t.Helper()
	ws := t.TempDir()
	t.Chdir(ws)
	t.Setenv("AJENT_HOME", t.TempDir())
	store, err := NewStore()
	require.NoError(t, err)
	return store, ws
}

// writeDeletableSession saves a one-message session, named when name is set, and
// returns its root id.
func writeDeletableSession(t *testing.T, store *Store, ws, name string) string {
	t.Helper()
	w, err := store.Create(ws, SessionData{Version: Version(), Name: name})
	require.NoError(t, err)
	id := w.Head() // the session entry, before any message advances the cursor
	_, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "hello")})
	require.NoError(t, aerr)
	require.NoError(t, w.Close())
	return id
}

func TestDeleteSession(t *testing.T) {
	t.Run("deletes_by_name", func(t *testing.T) {
		store, ws := deleteWorkspace(t)
		writeDeletableSession(t, store, ws, "fix-parser")

		var buf bytes.Buffer
		require.NoError(t, DeleteSession(&buf, store, ws, "fix-parser"))

		list, err := store.List(ws)
		require.NoError(t, err)
		assert.Empty(t, list)
		assert.Contains(t, buf.String(), "fix-parser")
	})

	t.Run("deletes_by_id_prefix", func(t *testing.T) {
		store, ws := deleteWorkspace(t)
		id := writeDeletableSession(t, store, ws, "")

		var buf bytes.Buffer
		require.NoError(t, DeleteSession(&buf, store, ws, id[:10]))

		list, err := store.List(ws)
		require.NoError(t, err)
		assert.Empty(t, list)
	})

	t.Run("keeps_other_sessions", func(t *testing.T) {
		store, ws := deleteWorkspace(t)
		writeDeletableSession(t, store, ws, "drop-me")
		keep := writeDeletableSession(t, store, ws, "keep-me")

		var buf bytes.Buffer
		require.NoError(t, DeleteSession(&buf, store, ws, "drop-me"))

		list, err := store.List(ws)
		require.NoError(t, err)
		require.Len(t, list, 1)
		assert.Equal(t, keep, list[0].ID)
	})

	t.Run("unknown_target_fails", func(t *testing.T) {
		store, ws := deleteWorkspace(t)

		var buf bytes.Buffer
		err := DeleteSession(&buf, store, ws, "no-such-session")
		assert.ErrorContains(t, err, "no session matches")
	})
}

func TestDeleteOldSessions(t *testing.T) {
	// every unnamed session is past a cutoff in the future, so these cases exercise
	// the prompt and the sweep; which sessions qualify is TestStoreStale's job.
	future := time.Now().UTC().Add(time.Hour)

	t.Run("confirmed_removes_stale", func(t *testing.T) {
		store, ws := deleteWorkspace(t)
		writeDeletableSession(t, store, ws, "")
		writeDeletableSession(t, store, ws, "")
		keep := writeDeletableSession(t, store, ws, "keep-me")

		var buf bytes.Buffer
		require.NoError(t, DeleteOldSessions(&buf, strings.NewReader("y\n"), store, ws, future))

		list, err := store.List(ws)
		require.NoError(t, err)
		require.Len(t, list, 1)
		assert.Equal(t, keep, list[0].ID) // a named session is never swept
		assert.Contains(t, buf.String(), "Deleted 2 sessions.")
	})

	t.Run("declined_keeps_all", func(t *testing.T) {
		store, ws := deleteWorkspace(t)
		writeDeletableSession(t, store, ws, "")

		var buf bytes.Buffer
		require.NoError(t, DeleteOldSessions(&buf, strings.NewReader("n\n"), store, ws, future))

		list, err := store.List(ws)
		require.NoError(t, err)
		assert.Len(t, list, 1)
		assert.Contains(t, buf.String(), "Cancelled")
	})

	t.Run("eof_declines", func(t *testing.T) {
		store, ws := deleteWorkspace(t)
		writeDeletableSession(t, store, ws, "")

		var buf bytes.Buffer
		require.NoError(t, DeleteOldSessions(&buf, strings.NewReader(""), store, ws, future))

		list, err := store.List(ws)
		require.NoError(t, err)
		assert.Len(t, list, 1)
		assert.Contains(t, buf.String(), "Cancelled")
	})

	t.Run("nothing_old_enough", func(t *testing.T) {
		store, ws := deleteWorkspace(t)
		writeDeletableSession(t, store, ws, "")

		var buf bytes.Buffer
		past := time.Now().UTC().AddDate(0, 0, -28)
		require.NoError(t, DeleteOldSessions(&buf, strings.NewReader("y\n"), store, ws, past))

		list, err := store.List(ws)
		require.NoError(t, err)
		assert.Len(t, list, 1)
		assert.Contains(t, buf.String(), "No unnamed sessions")
	})
}
