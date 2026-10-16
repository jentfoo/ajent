package session

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pinScanWorkers fixes the scan pool bound for one test and restores it after.
func pinScanWorkers(t *testing.T, n int) {
	t.Helper()

	prev := scanWorkers
	scanWorkers = n
	t.Cleanup(func() { scanWorkers = prev })
}

func TestScanEach(t *testing.T) {
	// pins the shared scanWorkers var, so the subtests run sequentially
	t.Run("keeps input order", func(t *testing.T) {
		paths := []string{"a", "b", "c", "d"}
		out := scanEach(paths, func(p string) string { return p })
		assert.Equal(t, paths, out)
	})

	t.Run("bounds concurrent runs", func(t *testing.T) {
		pinScanWorkers(t, 2)

		var inFlight atomic.Int32
		paired := make(chan struct{})
		var pairOnce sync.Once
		paths := []string{"a", "b", "c"}
		out := scanEach(paths, func(p string) string {
			n := inFlight.Add(1)
			defer inFlight.Add(-1)
			require.LessOrEqual(t, n, int32(2), "scan exceeded the pool bound")
			if n == 2 { // two runs admitted: the bound holds and the pool is live
				pairOnce.Do(func() { close(paired) })
			}
			select {
			case <-paired:
			case <-time.After(2 * time.Second):
				t.Error("scan never admitted two concurrent runs")
			}
			return p
		})
		assert.Equal(t, paths, out)
	})

	t.Run("pinned_zero_still_scans", func(t *testing.T) {
		pinScanWorkers(t, 0)

		paths := []string{"a", "b"}
		out := scanEach(paths, func(p string) string { return p })
		assert.Equal(t, paths, out)
	})
}
