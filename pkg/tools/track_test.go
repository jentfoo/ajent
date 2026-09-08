package tools

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTracker(t *testing.T) {
	t.Parallel()

	// no baseline means a path is never reported unchanged
	t.Run("never_read_is_unchanged_false", func(t *testing.T) {
		tr := NewTracker()
		assert.False(t, tr.Unchanged("/nonexistent")) // no baseline for dedupe
	})

	t.Run("observe_then_unchanged", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f.txt")
		data := []byte("hello\nworld\n")
		require.NoError(t, os.WriteFile(path, data, 0o644))

		tr := NewTracker()
		info, _ := os.Stat(path)
		tr.Observe(path, data, info)

		assert.True(t, tr.Unchanged(path))
	})

	// modifying the file in place reports unchanged=false
	t.Run("observe_then_modified", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f.txt")
		data := []byte("hello\nworld\n")
		require.NoError(t, os.WriteFile(path, data, 0o644))

		tr := NewTracker()
		info, _ := os.Stat(path)
		tr.Observe(path, data, info)

		// modify the file in place; size changes so Unchanged reports false
		changed := []byte("hello\nworld\nmore")
		require.NoError(t, os.WriteFile(path, changed, 0o644))
		assert.False(t, tr.Unchanged(path))
	})

	t.Run("reset", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f.txt")
		data := []byte("hello\nworld\n")
		require.NoError(t, os.WriteFile(path, data, 0o644))

		tr := NewTracker()
		info, _ := os.Stat(path)
		tr.Observe(path, data, info)
		require.True(t, tr.Unchanged(path))

		tr.Reset()
		assert.False(t, tr.Unchanged(path)) // a forgotten read must inject again
		assert.Empty(t, tr.Records())
	})

	t.Run("records_snapshot_is_copy", func(t *testing.T) {
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
	})

	t.Run("concurrent_observers", func(t *testing.T) {
		tr := NewTracker()
		// pre-create temp paths before spawning goroutines: *testing.T methods are
		// not safe to call concurrently with the test goroutine.
		paths := make([]string, 10)
		datas := make([][]byte, len(paths))
		for i := range paths {
			p := filepath.Join(t.TempDir(), "f")
			paths[i] = p
			datas[i] = []byte{byte('a' + i)}
		}

		var wg sync.WaitGroup
		for i, p := range paths {
			wg.Add(1)
			go func(i int, p string) {
				defer wg.Done()
				d := datas[i]
				_ = os.WriteFile(p, d, 0o644)
				fi, _ := os.Stat(p)
				tr.Observe(p, d, fi)
			}(i, p)
		}
		wg.Wait()
		assert.Len(t, tr.Records(), 10) // -race validates the map is safe
	})
}

func TestTrackerEditedRanges(t *testing.T) {
	t.Parallel()

	t.Run("returned_for_the_observed_content", func(t *testing.T) {
		tr := NewTracker()
		tr.Observe("a.go", []byte("one\ntwo\n"), nil)
		tr.markEdited("a.go", []lineRange{{from: 1, to: 2}})
		assert.Equal(t, []lineRange{{from: 1, to: 2}}, tr.editedFor("a.go", []byte("one\ntwo\n")))
	})

	t.Run("dropped_after_move", func(t *testing.T) {
		tr := NewTracker()
		tr.Observe("a.go", []byte("one\ntwo\n"), nil)
		tr.markEdited("a.go", []lineRange{{from: 1, to: 2}})
		assert.Empty(t, tr.editedFor("a.go", []byte("something else\n")))
	})

	t.Run("a_later_observe_clears_them", func(t *testing.T) {
		tr := NewTracker()
		tr.Observe("a.go", []byte("one\n"), nil)
		tr.markEdited("a.go", []lineRange{{from: 0, to: 1}})
		tr.Observe("a.go", []byte("two\n"), nil) // what an overwriting write does
		assert.Empty(t, tr.editedFor("a.go", []byte("two\n")))
	})

	t.Run("reread_of_same_content_keeps", func(t *testing.T) {
		tr := NewTracker()
		tr.Observe("a.go", []byte("one\ntwo\n"), nil)
		tr.markEdited("a.go", []lineRange{{from: 1, to: 2}})
		tr.Observe("a.go", []byte("one\ntwo\n"), nil) // what a read of the edited file does
		assert.Equal(t, []lineRange{{from: 1, to: 2}}, tr.editedFor("a.go", []byte("one\ntwo\n")))
	})

	t.Run("unobserved_path_records_nothing", func(t *testing.T) {
		tr := NewTracker()
		tr.markEdited("a.go", []lineRange{{from: 0, to: 1}})
		assert.Empty(t, tr.editedFor("a.go", []byte("x")))
	})
}
