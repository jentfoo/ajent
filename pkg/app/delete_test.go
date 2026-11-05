package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-analyze/bulk"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/client"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/paths"
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

func TestRunDeleteGatesOnServedSessions(t *testing.T) {
	// writeServedRecord fakes one serving instance's discovery record for id, so
	// the delete gate sees a live serve without binding a socket.
	writeServedRecord := func(t *testing.T, id string) {
		t.Helper()
		base, _, err := paths.SubDir(paths.ControlDirName)
		require.NoError(t, err)
		reg := filepath.Join(base, id, "deadbee.json")
		require.NoError(t, os.MkdirAll(filepath.Dir(reg), paths.DirPerm))
		b, merr := json.Marshal(client.Instance{
			SessionID: id, PID: os.Getpid(), Token: "deadbee", Socket: filepath.Join(filepath.Dir(reg), "deadbee.sock"),
			StartedAt: time.Now().Add(-2 * time.Minute).UnixMilli(),
		})
		require.NoError(t, merr)
		require.NoError(t, os.WriteFile(reg, b, 0o600))
	}

	t.Run("served_delete_refuses_then_forces", func(t *testing.T) {
		store, ws := deleteWorkspace(t)
		id := writeDeletableSession(t, store, ws, "shared")
		writeServedRecord(t, id)

		err := session.DeleteSession(&bytes.Buffer{}, store, ws, "shared", false, servedSessions())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "served by a running ajent instance")

		code := RunDelete(&bytes.Buffer{}, strings.NewReader(""), DeleteOptions{Target: "shared"})
		assert.Equal(t, ExitUsage, code) // the refusal is a usage error, served session kept

		code = RunDelete(&bytes.Buffer{}, strings.NewReader(""), DeleteOptions{Target: "shared", Force: true})
		assert.Equal(t, ExitOK, code)
		list, lerr := store.List(ws)
		require.NoError(t, lerr)
		assert.Empty(t, bulk.SliceFilter(func(in session.Info) bool { return in.Name == "shared" }, list))
	})

	t.Run("sweep_skips_served_rows", func(t *testing.T) {
		store, ws := deleteWorkspace(t)
		id := writeDeletableSession(t, store, ws, "")
		writeServedRecord(t, id)

		// a cutoff past now marks the fresh session stale, isolating the
		// served gate from the age window
		var buf bytes.Buffer
		cutoff := time.Now().UTC().Add(time.Hour)
		require.NoError(t, session.DeleteOldSessions(&buf, strings.NewReader("y\n"), store, ws, cutoff, servedSessions()))
		assert.Contains(t, buf.String(), "live, skipped")
		assert.Contains(t, buf.String(), "Nothing to delete")
		rows, lerr := store.List(ws)
		require.NoError(t, lerr)
		assert.Len(t, rows, 1) // the served row survived
	})
}
