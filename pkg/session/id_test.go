package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// setClock pins the package clock to t, returning a restore func.
func setClock(t time.Time) func() {
	old := clock
	clock = func() time.Time { return t }
	return func() { clock = old }
}

// resetIDCounter drops the monotonic floor so the next NewID adopts the pinned
// clock outright, regardless of what earlier tests left in it.
func resetIDCounter() {
	mu.Lock()
	defer mu.Unlock()
	lastMS = 0
}

func TestNewID(t *testing.T) {
	// the sorted-by-time and monotonic cases mutate the package clock, so this cannot run in parallel

	t.Run("length_and_alphabet", func(t *testing.T) {
		id := NewID()
		assert.Len(t, id, 26)
		for _, c := range id {
			assert.Contains(t, crockford, string(c))
		}
	})

	// increasing timestamps produce ids that sort in that order
	t.Run("sorted_by_time", func(t *testing.T) {
		t.Cleanup(setClock(time.UnixMilli(1_700_000_000_123).UTC()))

		base := int64(1_750_234_567_890)
		var prev string
		for i := range 5 {
			setClock(time.UnixMilli(base + int64(i)*1000).UTC())
			id := NewID()
			if prev != "" {
				assert.Greater(t, id, prev)
			}
			prev = id
		}
	})

	// one pinned timestamp still yields increasing ids
	t.Run("monotonic_within_ms", func(t *testing.T) {
		t.Cleanup(setClock(time.UnixMilli(1_700_000_123).UTC()))

		var prev string
		for range 200 {
			id := NewID()
			if prev != "" {
				assert.Greater(t, id, prev)
			}
			prev = id
		}
	})
}

func TestNewULID(t *testing.T) {
	// resets the package counter and clock, so this cannot run in parallel

	t.Run("ms_matches_embedded_id", func(t *testing.T) {
		base := time.Now().Add(time.Hour).UTC()
		resetIDCounter()
		t.Cleanup(setClock(base))

		id, ms := newULID()

		assert.Equal(t, base.UnixMilli(), ms) // a fresh clock is adopted outright
		assert.Equal(t, idTimeMS(id), ms)     // and is exactly what the id encodes
	})

	// a wall clock stepping backward holds the timestamp while ids still advance
	t.Run("backward_clock_holds_ms", func(t *testing.T) {
		base := time.Now().Add(time.Hour).UTC()
		resetIDCounter()
		t.Cleanup(setClock(base))

		id1, ms1 := newULID()
		setClock(base.Add(-time.Minute)) // the wall clock steps backward
		id2, ms2 := newULID()

		assert.Equal(t, base.UnixMilli(), ms1)
		assert.Equal(t, idTimeMS(id1), ms1) // ts equals what each id encodes
		assert.Equal(t, idTimeMS(id2), ms2)
		assert.Equal(t, ms1, ms2) // the counter holds its millisecond
		assert.Greater(t, id2, id1)
	})
}
