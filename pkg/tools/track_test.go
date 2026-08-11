package tools

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrackerNotRead(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	err := tr.Check("/nonexistent")
	assert.ErrorIs(t, err, ErrNotRead)
}

func TestTrackerReadThenUnchanged(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "f.txt")
	data := []byte("hello\nworld\n")
	require.NoError(t, os.WriteFile(path, data, 0o644))

	tr := NewTracker()
	info, _ := os.Stat(path)
	tr.Observe(path, data, info)

	assert.NoError(t, tr.Check(path))
}

func TestTrackerReadThenModified(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "f.txt")
	data := []byte("hello\nworld\n")
	require.NoError(t, os.WriteFile(path, data, 0o644))

	tr := NewTracker()
	info, _ := os.Stat(path)
	tr.Observe(path, data, info)

	// modify the file in place; size changes so Check must report stale
	changed := []byte("hello\nworld\nmore")
	require.NoError(t, os.WriteFile(path, changed, 0o644))
	assert.Error(t, tr.Check(path)) // ErrStale via IsStale
}

func TestTrackerRecordsSnapshotIsCopy(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	path := filepath.Join(t.TempDir(), "f.txt")
	data := []byte("x")
	require.NoError(t, os.WriteFile(path, data, 0o644))
	info, _ := os.Stat(path)
	tr.Observe(path, data, info)

	snap := tr.Records()
	assert.Contains(t, snap, path)
	delete(snap, path) // mutating the snapshot must not affect the tracker
	assert.NotEmpty(t, tr.Records())
}

func TestTrackerConcurrentObservers(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := filepath.Join(t.TempDir(), "f")
			d := []byte{byte('a' + i)}
			_ = os.WriteFile(p, d, 0o644)
			fi, _ := os.Stat(p)
			tr.Observe(p, d, fi)
		}(i)
	}
	wg.Wait()
	assert.Len(t, tr.Records(), 10) // -race validates the map is safe
}
