package session

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/paths"
)

// writeAt creates a session holding one message whose entry timestamps and file
// mtime both sit at at, so Info.Updated (the later of the two) lands there.
func writeAt(t *testing.T, s *Store, ws string, at time.Time, d SessionData, text string) *Writer {
	t.Helper()

	restore := setClock(at)
	resetIDCounter() // historical times only stamp when the counter adopts them
	w, err := s.Create(ws, d)
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	restore()
	appendAt(t, w, at, text)
	return w
}

// appendAt adds one message to w at at, moving its last-used time with it.
func appendAt(t *testing.T, w *Writer, at time.Time, text string) {
	t.Helper()

	restore := setClock(at)
	resetIDCounter()
	_, err := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, text)})
	require.NoError(t, err)
	restore()
	require.NoError(t, os.Chtimes(w.Path(), at, at))
}

// TestNewStorePinsRoot covers the sessions root's permission policy: created
// owner-only, and a wider existing mode tightened with a startup note.
func TestNewStorePinsRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not meaningful")
	}
	// not parallel: AJENT_HOME pins the resolution
	home := t.TempDir()
	t.Setenv(paths.EnvHome, home)

	store, err := NewStore()
	require.NoError(t, err)
	assert.Empty(t, store.Note())
	fi, err := os.Stat(filepath.Join(home, sessionsRoot))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(paths.DirPerm), fi.Mode().Perm())

	// a root that slipped wider is tightened with a note
	require.NoError(t, os.Chmod(filepath.Join(home, sessionsRoot), 0o750))
	store2, err := NewStore()
	require.NoError(t, err)
	assert.Contains(t, store2.Note(), "tightened to 0700")
}

func TestStoreDirDeterministic(t *testing.T) {
	t.Parallel()

	s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
	d1, err := s.Dir("/repo/My Project")
	require.NoError(t, err)
	d2, err := s.Dir("/repo/My Project")
	require.NoError(t, err)
	assert.Equal(t, d1, d2)

	other, err := s.Dir("/repo/Other Proj")
	require.NoError(t, err)
	assert.NotEqual(t, d1, other)
}

func TestStoreList(t *testing.T) {
	// the latest/find case pins the package clock via setClock, so this is sequential

	t.Run("latest_find", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		t.Cleanup(setClock(time.UnixMilli(1_700_000_000).UTC()))
		resetIDCounter() // both pins must adopt so the entry timestamps differ

		// far-apart timestamps so the ids share no prefix and newest-first is clear
		setClock(time.UnixMilli(1_700_000_001).UTC())
		w1, err := s.Create(ws, SessionData{Version: sessionVersion, Model: "a/b"})
		require.NoError(t, err)
		aID := w1.Head()
		_, aerr := w1.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "hello world")})
		require.NoError(t, aerr)
		require.NoError(t, w1.Close())

		setClock(time.UnixMilli(9_999_000_002).UTC())
		w2, err := s.Create(ws, SessionData{Version: sessionVersion, Model: "c/d"})
		require.NoError(t, err)
		bID := w2.Head()
		_, berr := w2.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "second")})
		require.NoError(t, berr)
		require.NoError(t, w2.Close())

		list, lerr := s.List(ws)
		require.NoError(t, lerr)
		assert.Len(t, list, 2)
		assert.Equal(t, bID, list[0].ID) // newest first
		assert.Equal(t, "c/d", list[0].Model)
		assert.Equal(t, aID, list[1].ID)
		// one message was appended to each session
		assert.Equal(t, 1, list[0].Messages)

		found, ferr := s.Find(ws, aID)
		require.NoError(t, ferr)
		assert.Equal(t, aID, found.ID)
	})

	// side files (output-*.txt, the editor-history line file) never surface as phantom sessions
	t.Run("skips_non_jsonl_files", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		w, err := s.Create(ws, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		txID := w.Head()
		_, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "hello")})
		require.NoError(t, aerr)
		require.NoError(t, w.Close())

		dir, derr := s.Dir(ws)
		require.NoError(t, derr)
		// side files beside the transcript must be ignored by List
		require.NoError(t, os.WriteFile(filepath.Join(dir, "output-abc.txt"), []byte("tool out"), 0o600))
		hh, hherr := NewEditorHistory(s, ws, "")
		require.NoError(t, hherr)
		hh.Append("/model")
		slotEntries(t, w.Path(), "t1", txID, 0, 0) // the branch slot sidecar

		list, lerr := s.List(ws)
		require.NoError(t, lerr)
		assert.Len(t, list, 1)
		assert.Equal(t, txID, list[0].ID)
	})

	// --continue means the session last worked in, not the one started last
	t.Run("orders_by_last_use", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		base := time.UnixMilli(1_800_000_000_000).UTC()
		t.Cleanup(setClock(base))

		// old starts first, new second, then old is worked in again
		old := writeAt(t, s, ws, base, SessionData{Version: sessionVersion}, "old")
		newer := writeAt(t, s, ws, base.Add(time.Hour), SessionData{Version: sessionVersion}, "new")
		appendAt(t, old, base.Add(2*time.Hour), "back to old")

		list, lerr := s.List(ws)
		require.NoError(t, lerr)
		require.Len(t, list, 2)
		assert.Equal(t, old.Path(), list[0].Path)
		assert.Equal(t, newer.Path(), list[1].Path)
	})

	t.Run("empty_when_missing_dir", func(t *testing.T) {
		s := StoreAt(filepath.Join(os.TempDir(), "no-such-store-"+t.Name()))
		list, err := s.List("anywhere")
		require.NoError(t, err)
		assert.Empty(t, list)
	})

	// a zero-byte .jsonl (crash between create and first write) must not surface
	// as an id-less row, even one whose fresh mtime would top the --continue list
	t.Run("drops_files_with_no_session", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		w, err := s.Create(ws, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		sessID := w.Head()
		_, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "hello")})
		require.NoError(t, aerr)
		require.NoError(t, w.Close())

		dir, derr := s.Dir(ws)
		require.NoError(t, derr)
		phantom := filepath.Join(dir, "2026-01-01T00-00-00Z-dead.jsonl")
		require.NoError(t, os.WriteFile(phantom, nil, 0o600))
		future := time.Now().Add(time.Hour) // newest mtime, so List ordering alone cannot save it
		require.NoError(t, os.Chtimes(phantom, future, future))

		list, lerr := s.List(ws)
		require.NoError(t, lerr)
		require.Len(t, list, 1)
		assert.Equal(t, sessID, list[0].ID)
		assert.Equal(t, w.Path(), list[0].Path)

		_, ferr := s.Find(ws, phantom) // invisible to Find too, not just unlisted
		assert.ErrorIs(t, ferr, ErrNotFound)
	})
}

func TestStoreFindAmbiguousAndMissing(t *testing.T) {
	s := StoreAt(filepath.Join(t.TempDir(), "sessions"))

	ws := t.TempDir()
	t.Cleanup(setClock(time.UnixMilli(1_900_000_001).UTC()))

	// two sessions at the same millisecond share a timestamp prefix
	w1, cerr := s.Create(ws, SessionData{Version: sessionVersion})
	require.NoError(t, cerr)
	idA := w1.Head()
	require.NoError(t, w1.Close())
	w2, cerr := s.Create(ws, SessionData{Version: sessionVersion})
	require.NoError(t, cerr)
	idB := w2.Head()
	require.NoError(t, w2.Close())

	_, ferr := s.Find(ws, "no-such-id")
	require.ErrorIs(t, ferr, ErrNotFound)

	// a short prefix covering both sessions is ambiguous
	_, ferr = s.Find(ws, idA[:8])
	require.EqualError(t, ferr, fmt.Sprintf("ambiguous session id %q", idA[:8]))

	// exact and unique-prefix lookups resolve. The ids differ in their random
	// tail, so a long enough prefix is unique.
	found, ferr := s.Find(ws, idB)
	require.NoError(t, ferr)
	assert.Equal(t, idB, found.ID)
}

func TestStoreRemoveDeletesOneSession(t *testing.T) {
	t.Parallel()

	s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
	ws := t.TempDir()

	w1, err := s.Create(ws, SessionData{Version: sessionVersion})
	require.NoError(t, err)
	_, aerr := w1.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "keep me")})
	require.NoError(t, aerr)
	require.NoError(t, w1.Sync())
	require.NoError(t, w1.Close())

	w2, err := s.Create(ws, SessionData{Version: sessionVersion})
	require.NoError(t, err)
	removedID := w2.Head()
	require.NoError(t, w2.Sync())
	require.NoError(t, w2.Close())

	require.NotEmpty(t, readSlots(w2.Path())) // each session carries its own slots

	// removing the empty w2 takes its file and slots, leaving w1 whole
	rerr := s.Remove(w2.Path())
	require.NoError(t, rerr)

	list, lerr := s.List(ws)
	require.NoError(t, lerr)
	assert.Len(t, list, 1)
	assert.NotEqual(t, removedID, list[0].ID)

	assert.Empty(t, readSlots(w2.Path()))
	assert.NotEmpty(t, readSlots(w1.Path())) // the sibling's slots are untouched
}

func TestStoreRemoveCursorBestEffort(t *testing.T) {
	t.Parallel()

	// a stuck cursor sidecar must not report a session as kept once its transcript
	// is gone: DeleteSession would exit non-zero and delete-old sweeps would
	// under-count. An unremovable directory stands in for the cleanup failure.
	s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
	ws := t.TempDir()
	w, err := s.Create(ws, SessionData{Version: sessionVersion})
	require.NoError(t, err)
	_, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "gone soon")})
	require.NoError(t, aerr)
	require.NoError(t, w.Sync()) // the append's cursor sidecar
	require.NoError(t, w.Close())

	head := headsDir(w.Path())
	require.NoError(t, os.RemoveAll(head)) // the writer's own slot is inside
	require.NoError(t, os.Mkdir(head, paths.DirPerm))
	require.NoError(t, os.WriteFile(filepath.Join(head, "lock"), []byte("x"), 0o600))

	require.NoError(t, s.Remove(w.Path()))

	_, serr := os.Stat(w.Path())
	assert.True(t, os.IsNotExist(serr)) // the transcript itself is gone
	list, lerr := s.List(ws)
	require.NoError(t, lerr)
	assert.Empty(t, list) // and nothing surfaces in the picker
}

func TestStoreRemoveTakesTornSidecar(t *testing.T) {
	t.Parallel()

	s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
	ws := t.TempDir()
	w, err := s.Create(ws, SessionData{Version: sessionVersion})
	require.NoError(t, err)
	e1, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "torn tail")})
	require.NoError(t, aerr)
	line, merr := json.Marshal(e1)
	require.NoError(t, merr)
	require.NoError(t, w.Close())

	// crash mid-append, then reopen so torn-tail repair quarantines the stray bytes
	f, oerr := os.OpenFile(w.Path(), os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, oerr)
	_, werr := f.Write(line[:len(line)/2])
	require.NoError(t, werr)
	require.NoError(t, f.Close())
	w2, oerr := Open(w.Path())
	require.NoError(t, oerr)
	require.NoError(t, w2.Sync()) // leave the branch cursor sidecar behind too
	require.NoError(t, w2.Close())
	require.True(t, fileExists(headsDir(w.Path())))
	require.True(t, fileExists(tornPath(w.Path())))

	// removing the session takes every sidecar with it
	require.NoError(t, s.Remove(w.Path()))

	assert.False(t, fileExists(w.Path()))
	assert.False(t, fileExists(headsDir(w.Path())))
	assert.False(t, fileExists(tornPath(w.Path())))
}

func TestStoreStale(t *testing.T) {
	now := time.UnixMilli(1_900_000_000_000).UTC()
	cutoff := now.AddDate(0, 0, -28)

	aged := func(t *testing.T, s *Store, ws string, at time.Time, d SessionData) string {
		t.Helper()

		return writeAt(t, s, ws, at, d, "hello").Path()
	}

	t.Run("old_unnamed_is_stale", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		p := aged(t, s, ws, now.AddDate(0, 0, -60), SessionData{Version: sessionVersion})

		stale, err := s.Stale(ws, cutoff)
		require.NoError(t, err)
		require.Len(t, stale, 1)
		assert.Equal(t, p, stale[0].Path)
	})

	t.Run("named_is_never_stale", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		aged(t, s, ws, now.AddDate(0, 0, -60), SessionData{Version: sessionVersion, Name: "keep-me"})

		stale, err := s.Stale(ws, cutoff)
		require.NoError(t, err)
		assert.Empty(t, stale)
	})

	t.Run("recent_is_kept", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		aged(t, s, ws, now.AddDate(0, 0, -3), SessionData{Version: sessionVersion})

		stale, err := s.Stale(ws, cutoff)
		require.NoError(t, err)
		assert.Empty(t, stale)
	})

	// a restored backup carries old entries but a fresh mtime, so the later half wins and is not swept
	t.Run("fresh_mtime_keeps_it", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		p := aged(t, s, ws, now.AddDate(0, 0, -60), SessionData{Version: sessionVersion})
		require.NoError(t, os.Chtimes(p, now, now))

		stale, err := s.Stale(ws, cutoff)
		require.NoError(t, err)
		assert.Empty(t, stale)
	})

	// ordered on last use, not the start time List sorts by
	t.Run("orders_by_last_used", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		older := aged(t, s, ws, now.AddDate(0, 0, -90), SessionData{Version: sessionVersion})
		newer := aged(t, s, ws, now.AddDate(0, 0, -40), SessionData{Version: sessionVersion})

		stale, err := s.Stale(ws, cutoff)
		require.NoError(t, err)
		require.Len(t, stale, 2)
		assert.Equal(t, []string{newer, older}, []string{stale[0].Path, stale[1].Path})
	})

	t.Run("empty_workspace_is_empty", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))

		stale, err := s.Stale(t.TempDir(), cutoff)
		require.NoError(t, err)
		assert.Empty(t, stale)
	})
}

func TestReadInfo(t *testing.T) {
	t.Parallel()

	// regresses the fork over-count: after a branch, readInfo must describe only
	// the persisted head's chain, so resume metadata matches what resuming would rebuild.
	t.Run("counts_active_branch_only", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "2026-01-01T00-00-00Z-abcd.jsonl")
		w, err := Create(p, SessionData{Version: sessionVersion})
		require.NoError(t, err)

		e1, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "first on main")})
		require.NoError(t, aerr)
		e2, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleAssistant, "reply one")})
		require.NoError(t, aerr)

		w.SetHead(e1.ID) // fork from the first message
		_, ferr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "fork prompt")})
		require.NoError(t, ferr)
		_, ferr = w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleAssistant, "fork reply")})
		require.NoError(t, ferr)
		w.SetHead(e2.ID) // head the resume picker would actually use
		require.NoError(t, w.Close())

		info, ok := readInfo(p)
		require.True(t, ok)

		assert.Equal(t, 2, info.Messages) // fork messages are not counted
		assert.Contains(t, info.First, "first on main")
	})

	// metadata reflects a head that sits on the abandoned fork rather than the file tail
	t.Run("branch_points_at_fork", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "s.jsonl")
		w, err := Create(p, SessionData{Version: sessionVersion})
		require.NoError(t, err)

		e1, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "main prompt")})
		require.NoError(t, aerr)
		_, aerr = w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleAssistant, "main reply")})
		require.NoError(t, aerr)

		w.SetHead(e1.ID) // rewind onto the fork
		forkA, ferr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "fork prompt")})
		require.NoError(t, ferr)
		_, ferr = w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleAssistant, "fork reply")})
		require.NoError(t, ferr)
		w.SetHead(forkA.ID) // active head is the fork start
		require.NoError(t, w.Close())

		info, ok := readInfo(p)
		require.True(t, ok)

		assert.Equal(t, 2, info.Messages) // root,e1,forkA: e1 + fork prompt
		assert.Contains(t, info.First, "main prompt")
	})

	// an old mtime must not age a transcript whose entries are recent, so a copy or
	// a restore cannot make live work look abandoned.
	t.Run("updated_tracks_newest_entry", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "s.jsonl")
		w, err := Create(p, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		last, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "recent")})
		require.NoError(t, aerr)
		require.NoError(t, w.Close())

		old := time.Now().AddDate(0, 0, -90)
		require.NoError(t, os.Chtimes(p, old, old))

		info, ok := readInfo(p)
		require.True(t, ok)
		assert.Equal(t, time.UnixMilli(last.TS).UTC(), info.Updated)
	})

	t.Run("carries_the_session_name", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "s.jsonl")
		w, err := Create(p, SessionData{Version: sessionVersion, Name: "fix-parser"})
		require.NoError(t, err)
		require.NoError(t, w.Close())

		info, ok := readInfo(p)
		require.True(t, ok)
		assert.Equal(t, "fix-parser", info.Name)
	})

	// identity survives a fresh root: ID/Started/Model belong to the file, so a
	// head on a second root still resolves them from line 1.
	t.Run("identity_survives_new_root", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "2026-01-02T03-04-05Z-f00d.jsonl")
		w, err := Create(p, SessionData{Version: sessionVersion, Model: "trunk-model", Name: "plan-session"})
		require.NoError(t, err)

		_, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "prior chat")})
		require.NoError(t, aerr)
		w.SetHead("")
		rootTip, rerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "kickoff")})
		require.NoError(t, rerr)

		// SetHead("") dropped the cursor sidecar, re-persist a head on the new root so
		// tail recovery lands there and Branch never reaches line 1.
		w.SetHead(rootTip.ID)
		require.NoError(t, w.Close())

		sessID := readEntries(t, p)[0].ID
		info, ok := readInfo(p)
		require.True(t, ok)

		assert.Equal(t, sessID, info.ID) // identity comes from line 1, not the branch
		assert.False(t, info.Started.IsZero())
		assert.Equal(t, "trunk-model", info.Model)
		assert.Equal(t, 1, info.Messages) // only kickoff sits on the new root's branch
	})

	// a zero-byte .jsonl (crash between create and first write) is not a session:
	// readInfo returns zero Info so no listing surface shows an id-less row
	t.Run("empty_file_dropped", func(t *testing.T) {
		dir := t.TempDir()
		empty := filepath.Join(dir, "2026-01-01T00-00-00Z-0.jsonl")
		require.NoError(t, os.WriteFile(empty, nil, 0o600))

		info, ok := readInfo(empty)
		assert.False(t, ok)
		assert.Zero(t, info)
	})

	// entries without a decodable identity are real work this build cannot
	// classify: they mark the file damaged rather than vanishing, so Find reaches
	// it by path and --delete can remove it deliberately
	t.Run("entries_without_identity_damaged", func(t *testing.T) {
		dir := t.TempDir()
		data, derr := json.Marshal(MessageData{Message: llm.Text(llm.RoleUser, "stray")})
		require.NoError(t, derr)
		line, lerr := json.Marshal(Entry{ID: "handmade", Type: TypeMessage, TS: 1_700_000_000_000, Data: data})
		require.NoError(t, lerr)
		stray := filepath.Join(dir, "2026-01-01T00-00-01Z-1.jsonl")
		require.NoError(t, os.WriteFile(stray, append(line, '\n'), 0o600))

		info, ok := readInfo(stray)
		require.True(t, ok)

		assert.True(t, info.Damaged)
		assert.Equal(t, stray, info.Path)
		assert.Empty(t, info.ID) // identity fields stay empty
	})

	// a torn identity line is dropped by Read with a warning, so hiding requires
	// every session line to fail: one bad entry never hides the file
	t.Run("torn_identity_line_damaged", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "2026-01-01T00-00-02Z-2.jsonl")
		writeLines(t, p, []string{
			`{"id":"lost","type":"session","ts":1700000000000`, // torn mid-write, newline intact
			`{"id":"m1","parentId":"lost","type":"notice","ts":1700000000001,"data":{"message":"kept"}}`,
		})

		info, ok := readInfo(p)
		require.True(t, ok)
		assert.True(t, info.Damaged)
	})
}

func TestNameOf(t *testing.T) {
	t.Parallel()

	newSession := func(t *testing.T, d SessionData) *Writer {
		t.Helper()

		w, err := Create(filepath.Join(t.TempDir(), "s.jsonl"), d)
		require.NoError(t, err)
		t.Cleanup(func() { _ = w.Close() })
		return w
	}
	read := func(t *testing.T, w *Writer) string {
		t.Helper()

		entries, _, err := Read(w.Path())
		require.NoError(t, err)
		return NameOf(entries)
	}

	t.Run("unnamed_is_empty", func(t *testing.T) {
		w := newSession(t, SessionData{Version: sessionVersion})
		assert.Empty(t, read(t, w))
	})

	t.Run("session_data_name", func(t *testing.T) {
		w := newSession(t, SessionData{Version: sessionVersion, Name: "fix-parser"})
		assert.Equal(t, "fix-parser", read(t, w))
	})

	t.Run("rename_entry_wins", func(t *testing.T) {
		w := newSession(t, SessionData{Version: sessionVersion, Name: "fix-parser"})
		_, err := w.Append(TypeSessionName, NameData{Name: "renamed"})
		require.NoError(t, err)
		assert.Equal(t, "renamed", read(t, w))
	})

	t.Run("newest_rename_wins", func(t *testing.T) {
		w := newSession(t, SessionData{Version: sessionVersion})
		for _, n := range []string{"one", "two", "three"} {
			_, err := w.Append(TypeSessionName, NameData{Name: n})
			require.NoError(t, err)
		}
		assert.Equal(t, "three", read(t, w))
	})

	// the name identifies the file, so a rename left behind by a rewind still counts
	t.Run("rename_off_branch_still_wins", func(t *testing.T) {
		w := newSession(t, SessionData{Version: sessionVersion, Name: "original"})
		root := w.Head()
		_, err := w.Append(TypeSessionName, NameData{Name: "renamed"})
		require.NoError(t, err)

		w.SetHead(root) // fork away from the rename
		_, err = w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "on the fork")})
		require.NoError(t, err)

		assert.Equal(t, "renamed", read(t, w))
	})

	// raw file order decides: a rename sitting before line 1 (hand-built or damaged
	// transcript) still beats the session entry's own name
	t.Run("rename_before_session_wins", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		writeLines(t, p, []string{
			`{"id":"n1","type":"session_name","ts":1700000000000,"data":{"name":"renamed"}}`,
			`{"id":"root","type":"session","ts":1700000000001,"data":{"version":1}}`,
		})
		entries, _, err := Read(p)
		require.NoError(t, err)
		assert.Equal(t, "renamed", NameOf(entries))
	})
}

func TestValidateName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		in    string
		want  string
		valid bool
	}{
		{"plain_name", "fix-parser", "fix-parser", true},
		{"trims_surrounding_space", "  fix-parser\t", "fix-parser", true},
		{"keeps_typed_case", "FixParser", "FixParser", true},
		{"rejects_empty", "", "", false},
		{"rejects_blank", "   ", "", false},
		{"allows_slug_punctuation", "feature/v2.1_b", "feature/v2.1_b", true},
		{"rejects_interior_whitespace", "fix parser", "", false},
		{"rejects_leading_dash", "-p", "", false},
		{"rejects_non_ascii", "fixé", "", false},
		{"rejects_command_substitution", "x$(id)", "", false},
		{"rejects_backticks", "a`id`", "", false},
		{"rejects_semicolon", "foo;rm", "", false},
		{"rejects_pipe", "a|b", "", false},
		{"rejects_over_limit", strings.Repeat("a", maxNameLen+1), "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateName(tc.in)
			if !tc.valid {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestStoreFindByName(t *testing.T) {
	s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
	ws := t.TempDir()
	t.Cleanup(setClock(time.UnixMilli(2_100_000_001).UTC()))

	named, err := s.Create(ws, SessionData{Version: sessionVersion, Name: "fix-parser"})
	require.NoError(t, err)
	namedID := named.Head()
	require.NoError(t, named.Close())

	plain, err := s.Create(ws, SessionData{Version: sessionVersion})
	require.NoError(t, err)
	plainID := plain.Head()
	require.NoError(t, plain.Close())

	t.Run("exact_name_match", func(t *testing.T) {
		got, ferr := s.Find(ws, "fix-parser")
		require.NoError(t, ferr)
		assert.Equal(t, namedID, got.ID)
		assert.Equal(t, "fix-parser", got.Name)
	})

	t.Run("name_match_is_case_insensitive", func(t *testing.T) {
		got, ferr := s.Find(ws, "FIX-PARSER")
		require.NoError(t, ferr)
		assert.Equal(t, namedID, got.ID)
	})

	t.Run("id_prefix_still_works", func(t *testing.T) {
		got, ferr := s.Find(ws, plainID)
		require.NoError(t, ferr)
		assert.Equal(t, plainID, got.ID)
	})

	t.Run("unknown_name_not_found", func(t *testing.T) {
		_, ferr := s.Find(ws, "no-such-name")
		assert.ErrorIs(t, ferr, ErrNotFound)
	})

	// a name that also prefixes another session's id resolves to the named one
	t.Run("name_beats_id_prefix", func(t *testing.T) {
		w, cerr := s.Create(ws, SessionData{Version: sessionVersion, Name: plainID[:4]})
		require.NoError(t, cerr)
		collidingID := w.Head()
		require.NoError(t, w.Close())

		got, ferr := s.Find(ws, plainID[:4])
		require.NoError(t, ferr)
		assert.Equal(t, collidingID, got.ID)
	})
}

func TestStoreFindNamed(t *testing.T) {
	s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
	ws := t.TempDir()
	t.Cleanup(setClock(time.UnixMilli(2_150_000_001).UTC()))

	named, err := s.Create(ws, SessionData{Version: sessionVersion, Name: "fix-parser"})
	require.NoError(t, err)
	namedID := named.Head()
	require.NoError(t, named.Close())

	plain, err := s.Create(ws, SessionData{Version: sessionVersion})
	require.NoError(t, err)
	plainID := plain.Head()
	require.NoError(t, plain.Close())

	t.Run("named_session_found", func(t *testing.T) {
		info, found, ferr := s.FindNamed(ws, "fix-parser")
		require.NoError(t, ferr)
		assert.True(t, found)
		assert.Equal(t, namedID, info.ID)
	})

	t.Run("name_match_is_case_insensitive", func(t *testing.T) {
		info, found, ferr := s.FindNamed(ws, "FIX-PARSER")
		require.NoError(t, ferr)
		assert.True(t, found)
		assert.Equal(t, namedID, info.ID)
	})

	t.Run("id_match_is_not_a_name", func(t *testing.T) {
		_, _, ferr := s.FindNamed(ws, plainID)
		assert.ErrorContains(t, ferr, "matches session id")
	})

	t.Run("unknown_target_not_found", func(t *testing.T) {
		_, found, ferr := s.FindNamed(ws, "no-such-name")
		require.NoError(t, ferr)
		assert.False(t, found)
	})
}

func TestStoreNameConflict(t *testing.T) {
	s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
	ws := t.TempDir()
	t.Cleanup(setClock(time.UnixMilli(2_200_000_001).UTC()))

	named, err := s.Create(ws, SessionData{Version: sessionVersion, Name: "taken"})
	require.NoError(t, err)
	namedPath := named.Path()
	require.NoError(t, named.Close())

	plain, err := s.Create(ws, SessionData{Version: sessionVersion})
	require.NoError(t, err)
	plainID, plainPath := plain.Head(), plain.Path()
	require.NoError(t, plain.Close())

	t.Run("free_name_is_usable", func(t *testing.T) {
		assert.NoError(t, s.NameConflict(ws, "free", plainPath))
	})

	t.Run("own_session_is_not_conflict", func(t *testing.T) {
		assert.NoError(t, s.NameConflict(ws, "taken", namedPath))
	})

	t.Run("other_session_name_conflicts", func(t *testing.T) {
		assert.ErrorIs(t, s.NameConflict(ws, "taken", plainPath), ErrNameConflict)
	})

	t.Run("existing_id_conflicts", func(t *testing.T) {
		assert.ErrorIs(t, s.NameConflict(ws, plainID, namedPath), ErrNameConflict)
	})

	t.Run("new_session_has_no_self", func(t *testing.T) {
		require.ErrorIs(t, s.NameConflict(ws, "taken", ""), ErrNameConflict)
		assert.NoError(t, s.NameConflict(ws, "free", ""))
	})
}

// futureVersion is a session format no current build accepts.
const futureVersion = 999

// writeNewer plants one newer-format transcript in the workspace's store, named
// when name is set, as a too-new format would have left behind.
func writeNewer(t *testing.T, s *Store, ws, name string) Info {
	t.Helper()

	dir, err := s.Dir(ws)
	require.NoError(t, err)
	line := fmt.Sprintf(`{"id":"newer-root","type":"session","ts":%d,"data":{"version":%d,"name":%q}}`,
		time.Now().Add(time.Hour).UnixMilli(), futureVersion, name)
	p := filepath.Join(dir, "2099-01-01T00-00-00Z-newer.jsonl")
	writeLines(t, p, []string{line})

	var info Info
	info.Path = p
	info.ID = "newer-root"
	info.Name = name
	return info
}

func TestStoreNewerFormat(t *testing.T) {
	t.Run("read_info_marks_blocked", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		want := writeNewer(t, s, t.TempDir(), "future-work")

		info, ok := readInfo(want.Path)
		require.True(t, ok)

		assert.True(t, info.Blocked)
		assert.Equal(t, want.ID, info.ID)
		assert.Equal(t, "future-work", info.Name)
		assert.False(t, info.Started.IsZero())
		assert.Zero(t, info.Messages) // branch-derived fields stay unset
		assert.Empty(t, info.First)
	})

	t.Run("list_keeps_the_row_visible", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		blocked := writeNewer(t, s, ws, "")
		readable := writeAt(t, s, ws, time.UnixMilli(1_900_000_000_000).UTC(), SessionData{Version: sessionVersion}, "older readable")

		list, err := s.List(ws)
		require.NoError(t, err)
		require.Len(t, list, 2) // the blocked transcript is not invisible

		row := func(path string) Info {
			i := slices.IndexFunc(list, func(in Info) bool { return in.Path == path })
			require.GreaterOrEqual(t, i, 0)
			return list[i]
		}
		assert.True(t, row(blocked.Path).Blocked)
		assert.Equal(t, blocked.ID, row(blocked.Path).ID)
		assert.False(t, row(readable.Path()).Blocked)
	})

	t.Run("find_resolves_blocked", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		writeNewer(t, s, ws, "future-work")

		for _, target := range []string{"future-work", "newer-root", "newer-ro"} { // name, id, prefix
			info, err := s.Find(ws, target)
			require.NoError(t, err)
			assert.True(t, info.Blocked)
			assert.Equal(t, "newer-root", info.ID)
		}

		found, exists, ferr := s.FindNamed(ws, "future-work")
		require.NoError(t, ferr)
		assert.True(t, exists) // --session sees it instead of re-minting the name
		assert.True(t, found.Blocked)
	})

	t.Run("name_conflict_guards_held_name", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		writeNewer(t, s, ws, "future-work")
		readable := writeAt(t, s, ws, time.UnixMilli(1_900_000_000_100).UTC(), SessionData{Version: sessionVersion}, "plain")

		// /session rename must not hand the held name to another transcript
		assert.ErrorIs(t, s.NameConflict(ws, "future-work", readable.Path()), ErrNameConflict)
	})

	t.Run("stale_never_sweeps_blocked", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		blocked := writeNewer(t, s, ws, "") // unnamed and old enough by entry ts

		cutoff := time.Now().UTC().Add(time.Hour)
		stale, err := s.Stale(ws, cutoff)
		require.NoError(t, err)
		assert.Empty(t, stale) // this build cannot read what it would be deleting

		_, staterr := os.Stat(blocked.Path)
		require.NoError(t, staterr) // and the file survived untouched
	})
}

// writeDamaged plants a transcript whose session entry cannot decode into
// SessionData, with an optional later rename via a session_name entry: what a
// torn or hand-edited identity line leaves behind. It returns the Info listing
// would hide.
func writeDamaged(t *testing.T, s *Store, ws, name string) Info {
	t.Helper()

	dir, err := s.Dir(ws)
	require.NoError(t, err)
	ts := time.Now().Add(time.Hour).UnixMilli()
	lines := []string{fmt.Sprintf(`{"id":"lost-root","type":"session","ts":%d,"data":"gone"}`, ts)}
	if name != "" {
		lines = append(lines, fmt.Sprintf(`{"id":"lost-name","type":"session_name","ts":%d,"data":{"name":%q}}`, ts, name))
	}
	p := filepath.Join(dir, "2099-01-01T00-00-01Z-lost.jsonl")
	writeLines(t, p, lines)

	var info Info
	info.Path = p
	info.Damaged = true
	info.Name = name
	return info
}

func TestStoreDamaged(t *testing.T) {
	t.Run("read_info_marks_damaged", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		want := writeDamaged(t, s, t.TempDir(), "")

		info, ok := readInfo(want.Path)
		require.True(t, ok)

		assert.True(t, info.Damaged)
		assert.Equal(t, want.Path, info.Path)
		assert.Empty(t, info.ID) // identity fields stay empty: nothing decodes
		assert.True(t, info.Started.IsZero())
		assert.Zero(t, info.Messages) // branch-derived fields stay unset too
	})

	t.Run("list_hides_the_row", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		damaged := writeDamaged(t, s, ws, "")
		readable := writeAt(t, s, ws, time.UnixMilli(1_900_000_000_000).UTC(), SessionData{Version: sessionVersion}, "readable")

		list, err := s.List(ws)
		require.NoError(t, err)
		require.Len(t, list, 1) // hidden rather than presented as an id-less row
		assert.Equal(t, readable.Path(), list[0].Path)

		_, serr := os.Stat(damaged.Path)
		require.NoError(t, serr) // and the file survived untouched
	})

	t.Run("find_resolves_by_path", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		damaged := writeDamaged(t, s, ws, "fix-parser")

		for _, target := range []string{damaged.Path, filepath.Base(damaged.Path)} { // full path, basename
			info, ferr := s.Find(ws, target)
			require.NoError(t, ferr)

			assert.True(t, info.Damaged)
			assert.Equal(t, damaged.Path, info.Path)
			assert.Equal(t, "fix-parser", info.Name) // a rename entry still identifies the file
		}
	})

	// the rename is not a listed row to match, so a name for a damaged transcript
	// still misses; the miss then names the file instead of reading as absence
	t.Run("name_still_misses_loudly", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		damaged := writeDamaged(t, s, ws, "fix-parser")

		_, ferr := s.Find(ws, "fix-parser")
		require.ErrorIs(t, ferr, ErrNotFound)
		assert.Contains(t, ferr.Error(), damaged.Path)
	})

	t.Run("miss_error_names_it", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		damaged := writeDamaged(t, s, ws, "")

		_, ferr := s.Find(ws, "no-such-target")
		require.ErrorIs(t, ferr, ErrNotFound)
		assert.Contains(t, ferr.Error(), `no session matches "no-such-target"`)
		assert.Contains(t, ferr.Error(), damaged.Path)
	})

	t.Run("path_outside_dir_refused", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws, other := t.TempDir(), t.TempDir()
		writeDamaged(t, s, ws, "")
		lost := writeDamaged(t, s, other, "")

		info, ferr := s.Find(other, lost.Path) // its own workspace resolves it
		require.NoError(t, ferr)
		assert.True(t, info.Damaged)

		wsDir, derr := s.Dir(ws)
		require.NoError(t, derr)
		rel, rerr := filepath.Rel(wsDir, lost.Path) // a ../ escape across workspaces
		require.NoError(t, rerr)
		_, ferr = s.Find(ws, rel)
		assert.ErrorIs(t, ferr, ErrNotFound)
	})

	t.Run("stale_never_sweeps", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		damaged := writeDamaged(t, s, ws, "") // unnamed and old enough: sweep bait

		cutoff := time.Now().UTC().Add(time.Hour)
		stale, err := s.Stale(ws, cutoff)
		require.NoError(t, err)
		assert.Empty(t, stale) // automatic deletion stays out of scope for these

		_, serr := os.Stat(damaged.Path)
		require.NoError(t, serr)
	})
}

func TestStoreFindByPath(t *testing.T) {
	t.Run("healthy_resolves", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		w, err := s.Create(ws, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		id := w.Head()
		require.NoError(t, w.Close())

		info, ferr := s.Find(ws, w.Path()) // a full path stands in for the id
		require.NoError(t, ferr)

		assert.Equal(t, id, info.ID)
		assert.False(t, info.Damaged)
	})

	// path resolution is uniform: a newer-format transcript resolves Blocked by
	// path exactly as its listed row does
	t.Run("newer_format_resolves_blocked", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		blocked := writeNewer(t, s, ws, "")

		info, ferr := s.Find(ws, blocked.Path)
		require.NoError(t, ferr)

		assert.True(t, info.Blocked)
	})

	t.Run("sidecars_unaddressable", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		w, err := s.Create(ws, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		require.NoError(t, w.Close())

		for _, p := range []string{headsDir(w.Path()), tornPath(w.Path())} {
			_, ferr := s.Find(ws, p) // only .jsonl transcripts resolve by path
			assert.ErrorIs(t, ferr, ErrNotFound)
		}
	})

	t.Run("missing_file_not_found", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))

		_, ferr := s.Find(t.TempDir(), filepath.Join(s.root, "nowhere", "gone.jsonl"))
		assert.ErrorIs(t, ferr, ErrNotFound)
	})
}

func TestStoreListMatchesSerial(t *testing.T) {
	// pins the package clock via setClock, so this is sequential

	s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
	ws := t.TempDir()
	base := time.UnixMilli(1_700_000_000).UTC()
	for i := range 5 {
		at := base.Add(time.Duration(i) * time.Minute)
		restore := setClock(at)
		resetIDCounter()
		w, err := s.Create(ws, SessionData{Version: sessionVersion, Model: "m"})
		require.NoError(t, err)
		_, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, fmt.Sprintf("msg %d", i))})
		require.NoError(t, aerr)
		require.NoError(t, w.Close())
		require.NoError(t, os.Chtimes(w.Path(), at, at))
		restore()
	}

	list, lerr := s.List(ws)
	require.NoError(t, lerr)
	require.Len(t, list, 5)

	// serial reference: the pre-parallel walk, one file at a time
	dir, derr := s.Dir(ws)
	require.NoError(t, derr)
	des, rerr := os.ReadDir(dir)
	require.NoError(t, rerr)
	var serial []Info
	for _, de := range des {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".jsonl") {
			continue
		}
		if info, ok := readInfo(filepath.Join(dir, de.Name())); ok {
			serial = append(serial, info)
		}
	}
	slices.SortFunc(serial, func(a, b Info) int {
		return cmp.Or(b.Updated.Compare(a.Updated), b.Started.Compare(a.Started))
	})

	assert.Equal(t, serial, list)
}

func TestStoreListTieKeepsNameOrder(t *testing.T) {
	// pins the package clock, so this is sequential

	s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
	ws := t.TempDir()
	at := time.UnixMilli(1_700_000_000).UTC()
	for i := range 3 { // same pinned clock and mtime: Updated and Started tie across all three
		writeAt(t, s, ws, at, SessionData{Version: sessionVersion}, fmt.Sprintf("m%d", i))
	}

	list, lerr := s.List(ws)
	require.NoError(t, lerr)
	require.Len(t, list, 3)

	dir, derr := s.Dir(ws)
	require.NoError(t, derr)
	names, nerr := os.ReadDir(dir)
	require.NoError(t, nerr)
	var sorted []string
	for _, de := range names {
		if !de.IsDir() && strings.HasSuffix(de.Name(), ".jsonl") {
			sorted = append(sorted, de.Name())
		}
	}
	slices.Sort(sorted) // the tie order the stable sort must preserve
	for i, name := range sorted {
		assert.Equal(t, filepath.Join(dir, name), list[i].Path)
	}
}
