package session

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriterCreateAndOpen(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := Create(p, SessionData{Version: sessionVersion})
	require.NoError(t, err)
	e, err := w.Append(TypeNotice, NoticeData{Message: "hi"})
	require.NoError(t, err)
	assert.NotEmpty(t, e.ID)

	entries, warns, rerr := Read(p)
	require.NoError(t, rerr)
	assert.Empty(t, warns)
	assert.Len(t, entries, 2)
	assert.Equal(t, TypeSession, entries[0].Type)

	w2, err := Open(p)
	require.NoError(t, err)
	assert.Equal(t, Head(entries), w2.Head())
	e2, err := w2.Append(TypeNotice, NoticeData{Message: "again"})
	require.NoError(t, err)
	assert.Equal(t, entries[1].ID, e2.ParentID)
}

func TestWriterParentChainLinear(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := Create(p, SessionData{Version: sessionVersion})
	require.NoError(t, err)
	for range 5 {
		_, err = w.Append(TypeCustom, CustomData{CustomType: "c"})
		require.NoError(t, err)
	}
	entries, _, rerr := Read(p)
	require.NoError(t, rerr)

	branch := Branch(entries, Head(entries))
	assert.Len(t, branch, 6) // session + five custom
	for i := range len(branch) - 1 {
		assert.Equal(t, branch[i].ID, branch[i+1].ParentID)
	}
}

func TestWriterDiscardWritesNoFile(t *testing.T) {
	t.Parallel()

	w := Discard()
	e, err := w.Append(TypeNotice, NoticeData{Message: "x"})
	require.NoError(t, err)
	assert.NotEmpty(t, e.ID)
	assert.Equal(t, e.ID, w.Head())
}

func TestWriterConcurrentAppendAtomic(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := Create(p, SessionData{Version: sessionVersion})
	require.NoError(t, err)

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				_, aerr := w.Append(TypeNotice, NoticeData{Message: "m"})
				assert.NoError(t, aerr)
			}
		}()
	}
	wg.Wait()

	entries, warns, rerr := Read(p)
	require.NoError(t, rerr)
	assert.Empty(t, warns)
	assert.Len(t, entries, 1001) // session + 2*500 notices, every line intact
}

func TestWriterAppendAfterCloseErrors(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "s.jsonl")
	w, err := Create(p, SessionData{Version: sessionVersion})
	require.NoError(t, err)
	require.NoError(t, w.Close())

	_, err = w.Append(TypeNotice, NoticeData{Message: "x"})
	assert.ErrorContains(t, err, "closed")
}

func TestWriterHeadCursor(t *testing.T) {
	t.Parallel()

	// SetHead persists the cursor immediately
	t.Run("set_head_persists_cursor", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := Create(p, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		e1, err := w.Append(TypeMessage, MessageData{Message: llmText("one")})
		require.NoError(t, err)

		w.SetHead(e1.ID) // rewind onto the first message

		cur, ok := readHead(p)
		require.True(t, ok)
		assert.Equal(t, e1.ID, cur.ID)
		require.NoError(t, w.Close())
	})

	// a fork to a new root leaves no cursor to point back at the abandoned branch
	t.Run("empty_head_drops_cursor", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := Create(p, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		t.Cleanup(func() { _ = w.Close() })
		e1, err := w.Append(TypeMessage, MessageData{Message: llmText("one")})
		require.NoError(t, err)
		w.SetHead(e1.ID)
		require.True(t, fileExists(headPath(p)))

		w.SetHead("")

		assert.False(t, fileExists(headPath(p)))
	})

	// Sync alone must record the appended head at a turn boundary
	t.Run("sync_persists_cursor_at_turn_boundary", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := Create(p, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		t.Cleanup(func() { _ = w.Close() })
		e1, err := w.Append(TypeMessage, MessageData{Message: llmText("one")})
		require.NoError(t, err)

		// no SetHead, Sync alone must record the appended head
		require.NoError(t, w.Sync())

		cur, ok := readHead(p)
		require.True(t, ok)
		assert.Equal(t, e1.ID, cur.ID)
	})

	// appends after the last Sync must not be skipped by a reopen
	t.Run("crash_mid_turn_resumes_tail", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := Create(p, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		e1, aerr := w.Append(TypeMessage, MessageData{Message: llmText("one")})
		require.NoError(t, aerr)
		require.NoError(t, w.Sync())

		e2, aerr := w.Append(TypeMessage, MessageData{Message: llmText("two")})
		require.NoError(t, aerr)
		e3, aerr := w.Append(TypeMessage, MessageData{Message: llmText("three")})
		require.NoError(t, aerr)
		// a crash: no Sync, the cursor sidecar still names e1
		require.NoError(t, w.Close())

		entries, _, rerr := Read(p)
		require.NoError(t, rerr)
		assert.Equal(t, []string{e1.ID, e2.ID, e3.ID}, ids(entries)[1:]) // skip the session entry

		w2, oerr := Open(p)
		require.NoError(t, oerr)
		t.Cleanup(func() { _ = w2.Close() })
		assert.Equal(t, e3.ID, w2.Head()) // the unsynced turn survives
		e4, aerr := w2.Append(TypeMessage, MessageData{Message: llmText("four")})
		require.NoError(t, aerr)
		assert.Equal(t, e3.ID, e4.ParentID)
	})

	// a fork followed by unsynced appends still resumes the fork's tip
	t.Run("crash_after_fork_keeps_fork", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := Create(p, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		e1, aerr := w.Append(TypeMessage, MessageData{Message: llmText("one")})
		require.NoError(t, aerr)
		_, aerr = w.Append(TypeMessage, MessageData{Message: llmText("two")})
		require.NoError(t, aerr)
		w.SetHead(e1.ID) // fork back to one, cursor recorded
		e3, aerr := w.Append(TypeMessage, MessageData{Message: llmText("three")})
		require.NoError(t, aerr)
		// a crash before the next Sync
		require.NoError(t, w.Close())

		w2, oerr := Open(p)
		require.NoError(t, oerr)
		t.Cleanup(func() { _ = w2.Close() })
		assert.Equal(t, e3.ID, w2.Head())
	})

	// data flushes before the cursor, so the cursor never names a lost entry
	t.Run("sync_flushes_before_cursor", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := Create(p, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		t.Cleanup(func() { _ = w.Close() })
		e1, aerr := w.Append(TypeMessage, MessageData{Message: llmText("one")})
		require.NoError(t, aerr)

		require.NoError(t, w.Sync())

		entries, _, rerr := Read(p) // the flushed data is on disk
		require.NoError(t, rerr)
		assert.Equal(t, e1.ID, Head(entries))
	})

	// a reopen resumes the persisted branch rather than the file tail
	t.Run("open_recovers_persisted_branch_not_tail", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := Create(p, SessionData{Version: sessionVersion})
		require.NoError(t, err)

		e1, aerr := w.Append(TypeMessage, MessageData{Message: llmText("one")})
		require.NoError(t, aerr)
		e2, aerr := w.Append(TypeMessage, MessageData{Message: llmText("two")}) // tail
		require.NoError(t, aerr)
		w.SetHead(e1.ID) // fork back to one
		require.NoError(t, w.Close())

		// the file tail is e2 but the cursor points at e1, so a reopen must resume from e1
		w2, oerr := Open(p)
		require.NoError(t, oerr)
		assert.Equal(t, e1.ID, w2.Head())
		e3, err := w2.Append(TypeMessage, MessageData{Message: llmText("three")})
		require.NoError(t, err)
		assert.Equal(t, e1.ID, e3.ParentID)

		// and that fork is now a new tip alongside the abandoned one
		entries, _, rerr := Read(p)
		require.NoError(t, rerr)
		// both the abandoned tip and the new fork stay reachable
		assert.Equal(t, []string{e2.ID, e3.ID}, tipIDs(entries))
	})

	// a sibling session in the same directory keeps its own branch
	t.Run("sibling_session_keeps_its_branch", func(t *testing.T) {
		dir := t.TempDir()
		pa := filepath.Join(dir, "a.jsonl")
		wa, err := Create(pa, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		a1, err := wa.Append(TypeMessage, MessageData{Message: llmText("one")})
		require.NoError(t, err)
		_, err = wa.Append(TypeMessage, MessageData{Message: llmText("two")}) // tail
		require.NoError(t, err)
		wa.SetHead(a1.ID) // rewind, then leave this session alone
		require.NoError(t, wa.Close())

		// a second session syncing afterwards must not clear a's cursor
		pb := filepath.Join(dir, "b.jsonl")
		wb, err := Create(pb, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		t.Cleanup(func() { _ = wb.Close() })
		_, err = wb.Append(TypeMessage, MessageData{Message: llmText("other")})
		require.NoError(t, err)
		require.NoError(t, wb.Sync())

		wa2, oerr := Open(pa)
		require.NoError(t, oerr)
		t.Cleanup(func() { _ = wa2.Close() })
		assert.Equal(t, a1.ID, wa2.Head())
	})

	// a discard writer never writes a cursor
	t.Run("discard_writes_no_head_cursor", func(t *testing.T) {
		w := Discard()
		e, err := w.Append(TypeMessage, MessageData{Message: llmText("x")})
		require.NoError(t, err)
		w.SetHead(e.ID)
		assert.False(t, fileExists(headPath(w.path)))
	})
}

// TestWriterAppendTimestamp pins the entry ts to the id's monotonic millisecond:
// metadata ordering must survive a wall clock stepping backward mid-session.
func TestWriterAppendTimestamp(t *testing.T) {
	// resets the package counter and clock, so this cannot run in parallel

	t.Run("ts_tracks_monotonic_not_wall", func(t *testing.T) {
		base := time.Now().Add(time.Hour).UTC()
		resetIDCounter() // ignore what earlier tests left in the counter
		t.Cleanup(setClock(base))

		p := filepath.Join(t.TempDir(), "s.jsonl")
		w, err := Create(p, SessionData{Version: sessionVersion})
		require.NoError(t, err)
		t.Cleanup(func() { _ = w.Close() })

		e1, aerr := w.Append(TypeNotice, NoticeData{Message: "one"})
		require.NoError(t, aerr)
		assert.Equal(t, base.UnixMilli(), e1.TS) // the pinned clock, not the real one
		assert.Equal(t, idTimeMS(e1.ID), e1.TS)  // and exactly what the entry's own id encodes

		setClock(base.Add(-time.Minute)) // the wall clock steps backward mid-session
		e2, aerr := w.Append(TypeNotice, NoticeData{Message: "two"})
		require.NoError(t, aerr)

		assert.Equal(t, e1.TS, e2.TS)           // ts holds its millisecond
		assert.Equal(t, idTimeMS(e2.ID), e2.TS) // still paired with the new id
		assert.Greater(t, e2.ID, e1.ID)         // ids keep advancing past it
	})
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
