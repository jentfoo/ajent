package app

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStateLaneDispatch(t *testing.T) {
	t.Parallel()

	t.Run("fresh_line_dispatches", func(t *testing.T) {
		lane := newStateLane()
		stale, queued := lane.dispatch(lane.stamp(), func() bool { return false })
		assert.False(t, stale)
		assert.False(t, queued)
	})

	t.Run("aged_line_refuses", func(t *testing.T) {
		lane := newStateLane()
		gen := lane.stamp()
		lane.bump()

		var offered atomic.Bool
		stale, queued := lane.dispatch(gen, func() bool {
			offered.Store(true)
			return true
		})
		assert.True(t, stale)
		assert.False(t, queued)
		assert.False(t, offered.Load()) // a stale line never reaches the queue
	})

	t.Run("offer_runs_under_the_lane", func(t *testing.T) {
		lane := newStateLane()
		var inside atomic.Bool
		lane.dispatch(lane.stamp(), func() bool {
			inside.Store(lane.quiesce(time.Millisecond)) // the lane is held: the bound hits
			return true
		})
		assert.True(t, inside.Load())
		assert.False(t, lane.quiesce(time.Millisecond)) // released again
	})

	t.Run("nil_lane_dispatches", func(t *testing.T) {
		var lane *stateLane
		stale, queued := lane.dispatch(lane.stamp(), func() bool { return true })
		assert.False(t, stale)
		assert.True(t, queued)
	})
}

func TestStateLaneQuiesce(t *testing.T) {
	t.Parallel()

	t.Run("idle_returns_at_once", func(t *testing.T) {
		lane := newStateLane()
		assert.False(t, lane.quiesce(time.Hour))
	})

	t.Run("waits_for_holder", func(t *testing.T) {
		lane := newStateLane()
		lane.lock()

		var done atomic.Bool
		go func() {
			lane.quiesce(time.Hour)
			done.Store(true)
		}()
		require.Never(t, done.Load, 50*time.Millisecond, 5*time.Millisecond)

		lane.unlock()
		require.Eventually(t, done.Load, 2*time.Second, 5*time.Millisecond)
	})

	t.Run("bounded_on_stuck_holder", func(t *testing.T) {
		lane := newStateLane()
		lane.lock()
		t.Cleanup(lane.unlock)

		assert.True(t, lane.quiesce(20*time.Millisecond))
	})

	t.Run("nil_lane_idle", func(t *testing.T) {
		var lane *stateLane
		assert.False(t, lane.quiesce(0))
	})
}
