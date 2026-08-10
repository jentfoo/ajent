package session

import (
	"path/filepath"
	"sync"
	"testing"

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
