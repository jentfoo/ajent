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
	defer setClock(time.UnixMilli(1_700_000_000).UTC())()

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

	byPrefix, pferr := s.Find(ws, aID[:8])
	require.NoError(t, pferr)
	assert.Equal(t, aID, byPrefix.ID)
}

func TestStoreFindAmbiguousAndMissing(t *testing.T) {
	s := StoreAt(filepath.Join(t.TempDir(), "sessions"))

	// no sessions yet is ErrNoSessions, not an error from listing
	_, err := s.Latest("nowhere")
	require.ErrorIs(t, err, ErrNoSessions)

	ws := t.TempDir()
	defer setClock(time.UnixMilli(1_900_000_001).UTC())()

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

func TestStoreListEmptyWhenMissingDir(t *testing.T) {
	t.Parallel()

	s := StoreAt(filepath.Join(os.TempDir(), "no-such-store-"+t.Name()))
	list, err := s.List("anywhere")
	require.NoError(t, err)
	assert.Empty(t, list)
}
