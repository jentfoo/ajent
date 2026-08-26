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

func TestNewID(t *testing.T) {
	// the sorted-by-time and monotonic cases mutate the package clock, so this
	// test cannot run in parallel.

	t.Run("length_and_alphabet", func(t *testing.T) {
		id := NewID()
		assert.Len(t, id, 26)
		for _, c := range id {
			assert.Contains(t, crockford, string(c))
		}
	})

	// increasing timestamps produce ids that sort in that order.
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

	// one pinned timestamp still yields increasing ids.
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
