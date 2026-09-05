package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractDeleteOld(t *testing.T) {
	t.Parallel()

	given, days, rest := extractDeleteOld([]string{"--delete-old", "14"})
	assert.True(t, given)
	assert.Equal(t, "14", days)
	assert.Empty(t, rest)

	// bare --delete-old (no trailing token) -> the default window.
	given, days, rest = extractDeleteOld([]string{"--delete-old"})
	assert.True(t, given)
	assert.Empty(t, days)
	assert.Empty(t, rest)

	// only a day count is the value; anything else stays a positional the flag
	// parser can reject.
	given, days, rest = extractDeleteOld([]string{"--delete-old", "soon"})
	assert.True(t, given)
	assert.Empty(t, days)
	assert.Equal(t, []string{"soon"}, rest)

	// the = form carries the count too.
	_, days, _ = extractDeleteOld([]string{"--delete-old=7", "-m", "p/m"})
	assert.Equal(t, "7", days)

	given, _, rest = extractDeleteOld([]string{"-m", "p/m"})
	assert.False(t, given)
	assert.Equal(t, []string{"-m", "p/m"}, rest)
}

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

func TestDeleteSession(t *testing.T) {
	t.Run("deletes_by_name", func(t *testing.T) {
		store, ws := deleteWorkspace(t)
		writeDeletableSession(t, store, ws, "fix-parser")

		var buf bytes.Buffer
		require.NoError(t, deleteSession(&buf, store, ws, "fix-parser"))

		list, err := store.List(ws)
		require.NoError(t, err)
		assert.Empty(t, list)
		assert.Contains(t, buf.String(), "fix-parser")
	})

	t.Run("deletes_by_id_prefix", func(t *testing.T) {
		store, ws := deleteWorkspace(t)
		id := writeDeletableSession(t, store, ws, "")

		var buf bytes.Buffer
		require.NoError(t, deleteSession(&buf, store, ws, id[:10]))

		list, err := store.List(ws)
		require.NoError(t, err)
		assert.Empty(t, list)
	})

	t.Run("keeps_other_sessions", func(t *testing.T) {
		store, ws := deleteWorkspace(t)
		writeDeletableSession(t, store, ws, "drop-me")
		keep := writeDeletableSession(t, store, ws, "keep-me")

		var buf bytes.Buffer
		require.NoError(t, deleteSession(&buf, store, ws, "drop-me"))

		list, err := store.List(ws)
		require.NoError(t, err)
		require.Len(t, list, 1)
		assert.Equal(t, keep, list[0].ID)
	})

	t.Run("unknown_target_fails", func(t *testing.T) {
		store, ws := deleteWorkspace(t)

		var buf bytes.Buffer
		err := deleteSession(&buf, store, ws, "no-such-session")
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
		require.NoError(t, deleteOldSessions(&buf, strings.NewReader("y\n"), store, ws, future))

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
		require.NoError(t, deleteOldSessions(&buf, strings.NewReader("n\n"), store, ws, future))

		list, err := store.List(ws)
		require.NoError(t, err)
		assert.Len(t, list, 1)
		assert.Contains(t, buf.String(), "Cancelled")
	})

	// no terminal to answer the prompt declines rather than deleting blind
	t.Run("eof_declines", func(t *testing.T) {
		store, ws := deleteWorkspace(t)
		writeDeletableSession(t, store, ws, "")

		var buf bytes.Buffer
		require.NoError(t, deleteOldSessions(&buf, strings.NewReader(""), store, ws, future))

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
		require.NoError(t, deleteOldSessions(&buf, strings.NewReader("y\n"), store, ws, past))

		list, err := store.List(ws)
		require.NoError(t, err)
		assert.Len(t, list, 1)
		assert.Contains(t, buf.String(), "No unnamed sessions")
	})
}

func TestRunDelete(t *testing.T) {
	t.Run("delete_reports_ok", func(t *testing.T) {
		store, ws := deleteWorkspace(t)
		writeDeletableSession(t, store, ws, "fix-parser")

		var buf bytes.Buffer
		code := runDelete(&buf, strings.NewReader(""), cliFlags{deleteGiven: true, deleteTarget: "fix-parser"})
		assert.Equal(t, exitOK, code)

		list, err := store.List(ws)
		require.NoError(t, err)
		assert.Empty(t, list)
	})

	t.Run("unknown_target_reports_usage", func(t *testing.T) {
		deleteWorkspace(t)

		var buf bytes.Buffer
		code := runDelete(&buf, strings.NewReader(""), cliFlags{deleteGiven: true, deleteTarget: "no-such"})
		assert.Equal(t, exitUsage, code)
	})

	t.Run("delete_old_uses_the_window", func(t *testing.T) {
		store, ws := deleteWorkspace(t)
		writeDeletableSession(t, store, ws, "")

		var buf bytes.Buffer
		code := runDelete(&buf, strings.NewReader("y\n"), cliFlags{deleteOld: true, deleteOldDays: defaultStaleDays})
		assert.Equal(t, exitOK, code)

		list, err := store.List(ws)
		require.NoError(t, err)
		assert.Len(t, list, 1) // a session written just now is not stale
		assert.Contains(t, buf.String(), "No unnamed sessions")
	})
}
