package session

import (
	"strings"
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

// pinClock pins the clock at t and rewinds the monotonic floor so ids adopt the
// pinned time outright; the returned restore puts both back, keeping a future
// stamp from leaking into later tests through lastMS.
func pinClock(t time.Time) func() {
	mu.Lock()
	oldFloor := lastMS
	oldSec := idSec
	mu.Unlock()
	restoreClock := setClock(t)
	resetIDCounter()
	return func() {
		restoreClock()
		mu.Lock()
		lastMS = oldFloor
		idSec = oldSec
		mu.Unlock()
	}
}

// resetIDCounter drops the monotonic floor so the next NewID adopts the pinned
// clock outright, regardless of what earlier tests left in it.
func resetIDCounter() {
	mu.Lock()
	defer mu.Unlock()
	lastMS = 0
	idSec = 0
}

// idValue decodes an id's packed integer (seconds above the random tail), so
// the lexical sort pins against numeric order.
func idValue(id string) uint64 {
	var v uint64
	for i := range len(id) {
		v = v<<5 | uint64(strings.IndexByte(crockford, id[i]))
	}
	return v
}

func TestNewID(t *testing.T) {
	// the cases below mutate the package clock, so this cannot run in parallel

	t.Run("length_and_alphabet", func(t *testing.T) {
		id := NewID()
		assert.Len(t, id, 12)
		for _, c := range id {
			assert.Contains(t, crockford, string(c))
		}
	})

	// increasing seconds produce ids that sort in that order
	t.Run("sorted_by_time", func(t *testing.T) {
		base := time.Now().Add(time.Hour).UTC()
		t.Cleanup(pinClock(base)) // a future stamp is adopted outright, floors restored after

		var prev string
		for i := range 5 {
			setClock(base.Add(time.Duration(i) * time.Second))
			id := NewID()
			if prev != "" {
				assert.Greater(t, id, prev)
				assert.Greater(t, idValue(id), idValue(prev)) // lexical order reduces to the integers
			}
			prev = id
		}
	})

	// one pinned second still yields increasing ids
	t.Run("monotonic_within_second", func(t *testing.T) {
		base := time.Now().Add(time.Hour).UTC()
		t.Cleanup(pinClock(base))

		var prev string
		for range 200 {
			id := NewID()
			if prev != "" {
				assert.Greater(t, id, prev)
			}
			prev = id
		}
	})

	// a wall clock stepping backward holds the stamp while ids still advance
	t.Run("backward_clock_holds_stamp", func(t *testing.T) {
		base := time.Now().Add(time.Hour).UTC()
		t.Cleanup(pinClock(base))

		id1 := NewID()
		ms := monoMilli()
		setClock(base.Add(-time.Minute)) // the wall clock steps backward
		id2 := NewID()

		assert.Equal(t, base.UnixMilli(), ms)    // a fresh clock is adopted outright
		assert.Equal(t, ms, monoMilli())         // and held through the backward step
		assert.Equal(t, ms/1000, idTimeSec(id2)) // the id shares that held second
		assert.Greater(t, id2, id1)              // ids keep advancing past it
	})
}
