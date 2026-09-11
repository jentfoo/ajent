package app

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deleteWorkspace points the cwd and AJENT_HOME at fresh temp dirs so a case sees
// only the sessions it writes.
func deleteWorkspace(t *testing.T) (*session.Store, string) {
	t.Helper()
	ws := t.TempDir()
	t.Chdir(ws)
	t.Setenv("AJENT_HOME", t.TempDir())
	store, err := session.NewStore()
	require.NoError(t, err)
	return store, ws
}

// writeDeletableSession saves a one-message session, named when name is set, and
// returns its root id.
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
