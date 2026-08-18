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

func TestNewIDLengthAndAlphabet(t *testing.T) {
	t.Parallel()

	id := NewID()
	assert.Len(t, id, 26)
	for _, c := range id {
		assert.Contains(t, crockford, string(c))
	}
}

// TestNewIDSortedByTime pins increasing timestamps and checks the ids sort in
// that order. It mutates the package clock so it is not parallel.
func TestNewIDSortedByTime(t *testing.T) {
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
}

// TestNewIDMonotonicWithinMs pins one timestamp and checks ids stay increasing.
func TestNewIDMonotonicWithinMs(t *testing.T) {
	t.Cleanup(setClock(time.UnixMilli(1_700_000_123).UTC()))

	var prev string
	for range 200 {
		id := NewID()
		if prev != "" {
			assert.Greater(t, id, prev)
		}
		prev = id
	}
}
