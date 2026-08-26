package session

import (
	"fmt"
	"os"
	"path/filepath"
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

// TestStoreListLatestFind creates sessions at distinct timestamps and checks
// listing order plus id lookup. It mutates the package clock so it is sequential.
func TestStoreListLatestFind(t *testing.T) {
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

// TestStoreListSkipsNonJsonlFiles verifies side files (output-*.txt, the
// editor-history line file) never surface as phantom sessions in the picker.
func TestStoreListSkipsNonJsonlFiles(t *testing.T) {
	t.Parallel()

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
}

func TestStoreListEmptyWhenMissingDir(t *testing.T) {
	t.Parallel()

	s := StoreAt(filepath.Join(os.TempDir(), "no-such-store-"+t.Name()))
	list, err := s.List("anywhere")
	require.NoError(t, err)
	assert.Empty(t, list)
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

// TestReadInfoCountsActiveBranchOnly regresses the fork over-count: after a
// branch, readInfo must describe only the persisted head's chain, so resume
// metadata matches what resuming would actually rebuild.
func TestReadInfoCountsActiveBranchOnly(t *testing.T) {
	t.Parallel()

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
}

// TestReadInfoBranchPointsAtFork verifies metadata reflects a head that sits on
// the abandoned fork rather than the file tail.
func TestReadInfoBranchPointsAtFork(t *testing.T) {
	t.Parallel()

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
}
