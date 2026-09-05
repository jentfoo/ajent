package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	// the latest/find case pins the package clock via setClock, so this is sequential.

	t.Run("latest_find", func(t *testing.T) {
		s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
		ws := t.TempDir()
		t.Cleanup(setClock(time.UnixMilli(1_700_000_000).UTC()))

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
		assert.Positive(t, list[0].Messages)

		latest, lerr := s.Latest(ws)
		require.NoError(t, lerr)
		assert.Equal(t, bID, latest.ID)

		found, ferr := s.Find(ws, aID)
		require.NoError(t, ferr)
		assert.Equal(t, aID, found.ID)
	})

	// side files (output-*.txt, the editor-history line file) never surface as phantom sessions.
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
		// side files beside the transcript must be ignored by List.
		require.NoError(t, os.WriteFile(filepath.Join(dir, "output-abc.txt"), []byte("tool out"), 0o600))
		hh, hherr := NewEditorHistory(s, ws, "")
		require.NoError(t, hherr)
		hh.Append("/model")

		list, lerr := s.List(ws)
		require.NoError(t, lerr)
		assert.Len(t, list, 1)
		assert.Equal(t, txID, list[0].ID)
	})

	t.Run("empty_when_missing_dir", func(t *testing.T) {
		s := StoreAt(filepath.Join(os.TempDir(), "no-such-store-"+t.Name()))
		list, err := s.List("anywhere")
		require.NoError(t, err)
		assert.Empty(t, list)
	})
}

func TestStoreFindAmbiguousAndMissing(t *testing.T) {
	s := StoreAt(filepath.Join(t.TempDir(), "sessions"))

	// no sessions yet is ErrNoSessions, not an error from listing
	_, err := s.Latest("nowhere")
	require.ErrorIs(t, err, ErrNoSessions)

	ws := t.TempDir()
	t.Cleanup(setClock(time.UnixMilli(1_900_000_001).UTC()))

	// two sessions at the same millisecond share a timestamp prefix.
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

	// a short prefix covering both sessions is ambiguous.
	_, ferr = s.Find(ws, idA[:8])
	require.EqualError(t, ferr, fmt.Sprintf("ambiguous session id %q", idA[:8]))

	// exact and unique-prefix lookups resolve. The ids differ in their random
	// tail, so a long enough prefix is unique.
	found, ferr := s.Find(ws, idB)
	require.NoError(t, ferr)
	assert.Equal(t, idB, found.ID)
}

// TestStoreRemoveDeletesOneSession verifies Remove drops the transcript and its
// head cursor without touching a sibling session in the same workspace.
func TestStoreRemoveDeletesOneSession(t *testing.T) {
	s := StoreAt(filepath.Join(t.TempDir(), "sessions"))
	ws := t.TempDir()

	w1, err := s.Create(ws, SessionData{Version: sessionVersion})
	require.NoError(t, err)
	_, aerr := w1.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "keep me")})
	require.NoError(t, aerr)
	require.NoError(t, w1.Sync()) // persist HEAD for w1
	require.NoError(t, w1.Close())

	w2, err := s.Create(ws, SessionData{Version: sessionVersion})
	require.NoError(t, err)
	removedID := w2.Head()
	require.NoError(t, w2.Sync()) // HEAD now names w2
	require.NoError(t, w2.Close())

	dir, derr := s.Dir(ws)
	require.NoError(t, derr)
	_, herr := ReadHead(dir)
	require.True(t, herr) // cursor points at the removed session before cleanup

	// removing the empty w2 leaves only its own file and clears HEAD for it.
	rerr := s.Remove(w2.Path())
	require.NoError(t, rerr)

	list, lerr := s.List(ws)
	require.NoError(t, lerr)
	assert.Len(t, list, 1)
	assert.NotEqual(t, removedID, list[0].ID)

	dir2, _ := s.Dir(ws)
	_, ok := ReadHead(dir2)
	assert.False(t, ok) // cursor for the removed file is gone
}

// TestStoreStale verifies the --delete-old selection: unnamed sessions past the
// cutoff only, judged on both halves of Info.Updated.
func TestStoreStale(t *testing.T) {
	now := time.UnixMilli(1_900_000_000_000).UTC()
	cutoff := now.AddDate(0, 0, -28)

	// aged writes a session whose entry timestamps and file mtime both sit at at,
	// so Updated (the later of the two) lands there.
	aged := func(t *testing.T, s *Store, ws string, at time.Time, d SessionData) string {
		t.Helper()
		restore := setClock(at)
		w, err := s.Create(ws, d)
		require.NoError(t, err)
		_, aerr := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "hello")})
		require.NoError(t, aerr)
		require.NoError(t, w.Close())
		restore()
		require.NoError(t, os.Chtimes(w.Path(), at, at))
		return w.Path()
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

	// a restored backup carries old entries but a fresh mtime; the later half wins
	// so it is not swept.
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

	// metadata reflects a head that sits on the abandoned fork rather than the file tail.
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
