package app

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/go-analyze/bulk"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
)

// deleteWorkspace points the cwd and AJENT_HOME at fresh temp dirs so a case sees only the sessions it writes.
func deleteWorkspace(t *testing.T) (*session.Store, string) {
	t.Helper()

	ws := t.TempDir()
	t.Chdir(ws)
	t.Setenv("AJENT_HOME", t.TempDir())
	store, err := session.NewStore()
	require.NoError(t, err)
	return store, ws
}

// writeDeletableSession saves a one-message session, named when name is set, and returns its root id.
func writeDeletableSession(t *testing.T, store *session.Store, ws, name string) string {
	t.Helper()

	w, err := store.Create(ws, session.SessionData{Version: session.Version(), Name: name})
	require.NoError(t, err)
	id := w.Head() // the session entry, before any message advances the cursor
	_, aerr := w.Append(session.TypeMessage, session.MessageData{Message: llm.Text(llm.RoleUser, "hello")})
	require.NoError(t, aerr)
	require.NoError(t, w.Close())
	return id
}

func TestRunDelete(t *testing.T) {
	t.Run("delete_reports_ok", func(t *testing.T) {
		store, ws := deleteWorkspace(t)
		writeDeletableSession(t, store, ws, "fix-parser")

		var buf bytes.Buffer
		code := RunDelete(&buf, strings.NewReader(""), DeleteOptions{Target: "fix-parser"})
		assert.Equal(t, ExitOK, code)

		list, err := store.List(ws)
		require.NoError(t, err)
		assert.Empty(t, list)
	})

	t.Run("unknown_target_reports_usage", func(t *testing.T) {
		deleteWorkspace(t)

		var buf bytes.Buffer
		code := RunDelete(&buf, strings.NewReader(""), DeleteOptions{Target: "no-such"})
		assert.Equal(t, ExitUsage, code)
	})

	t.Run("delete_old_uses_the_window", func(t *testing.T) {
		store, ws := deleteWorkspace(t)
		writeDeletableSession(t, store, ws, "")

		var buf bytes.Buffer
		code := RunDelete(&buf, strings.NewReader("y\n"), DeleteOptions{OldDays: 28})
		assert.Equal(t, ExitOK, code)

		list, err := store.List(ws)
		require.NoError(t, err)
		assert.Len(t, list, 1) // a session written just now is not stale
		assert.Contains(t, buf.String(), "No unnamed sessions")
	})
}

// TestRunDeleteGatesOnLiveWriters pins the liveness rule: a session another
// writer holds live refuses naming the count, --force escapes for --delete
// only, and the sweep lists live rows as skipped rather than deleting them.
func TestRunDeleteGatesOnLiveWriters(t *testing.T) {
	t.Run("live_delete_refuses_then_forces", func(t *testing.T) {
		store, ws := deleteWorkspace(t)
		writeDeletableSession(t, store, ws, "shared")
		w, err := store.Create(ws, session.SessionData{Version: session.Version()})
		require.NoError(t, err)
		t.Cleanup(func() { _ = w.Close() })
		// reopen the saved transcript the way a second ajent would: a live slot
		// now stands beside the saved session
		peer, err := store.Find(ws, "shared")
		require.NoError(t, err)
		live, err := session.Open(peer.Path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = live.Close() })

		err = session.DeleteSession(&bytes.Buffer{}, store, ws, "shared", false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "live in 1 running ajent instance")

		code := RunDelete(&bytes.Buffer{}, strings.NewReader(""), DeleteOptions{Target: "shared"})
		assert.Equal(t, ExitUsage, code) // the refusal is a usage error, live session kept

		code = RunDelete(&bytes.Buffer{}, strings.NewReader(""), DeleteOptions{Target: "shared", Force: true})
		assert.Equal(t, ExitOK, code)
		list, lerr := store.List(ws)
		require.NoError(t, lerr)
		assert.Empty(t, bulk.SliceFilter(func(in session.Info) bool { return in.Name == "shared" }, list))
	})

	t.Run("sweep_skips_live_rows", func(t *testing.T) {
		store, ws := deleteWorkspace(t)
		writeDeletableSession(t, store, ws, "")
		list, err := store.List(ws)
		require.NoError(t, err)
		require.Len(t, list, 1)
		livePath := list[0].Path
		// a live writer's slot marks the transcript live regardless of mtime
		live, err := session.Open(livePath)
		require.NoError(t, err)
		t.Cleanup(func() { _ = live.Close() })

		// a cutoff past now marks the fresh session stale, isolating the
		// liveness gate from the age window
		var buf bytes.Buffer
		cutoff := time.Now().UTC().Add(time.Hour)
		require.NoError(t, session.DeleteOldSessions(&buf, strings.NewReader("y\n"), store, ws, cutoff))
		assert.Contains(t, buf.String(), "live, skipped")
		assert.Contains(t, buf.String(), "Nothing to delete")
		rows, lerr := store.List(ws)
		require.NoError(t, lerr)
		assert.Len(t, rows, 1) // the live row survived
	})
}
