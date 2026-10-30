package app

import (
	"sync"
	"time"
)

// stateLane is the one serialization point for the transactions that swap or
// move the branch: rewind and fork hold it across their whole transaction, the
// compactor across its append-then-swap section, and turn dispatch only long
// enough to re-check conditions and commit the queue's draining mark, never
// across the turn itself. The gen counter ages out pump lines queued before a
// rewind, so a buffered prompt never lands on a branch it was not written
// against. A nil lane runs every holder unserialized, for harnesses driving a
// single surface.
type stateLane struct {
	mu    sync.Mutex    // the lane itself: one holder at a time
	state sync.Mutex    // guards gen and the idle signal, never taken nested
	idle  chan struct{} // closed while no holder, for shutdown's bounded wait
	gen   uint64        // bumped per rewind; pump lines carry the value queued at
}

func newStateLane() *stateLane {
	idle := make(chan struct{})
	close(idle) // born idle
	return &stateLane{idle: idle}
}

// lock claims the lane for one transaction, blocking until the current holder
// finishes. Nested claims deadlock: every holder runs its transaction to
// completion before unlocking.
func (l *stateLane) lock() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.state.Lock()
	l.idle = make(chan struct{})
	l.state.Unlock()
}

// unlock releases the lane, waking quiesce.
func (l *stateLane) unlock() {
	if l == nil {
		return
	}
	l.state.Lock()
	close(l.idle)
	l.state.Unlock()
	l.mu.Unlock()
}

// stamp returns the dispatch generation a pump line carries at enqueue.
func (l *stateLane) stamp() uint64 {
	if l == nil {
		return 0
	}
	l.state.Lock()
	defer l.state.Unlock()
	return l.gen
}

// bump marks a rewind complete: every line queued before it went stale.
func (l *stateLane) bump() {
	if l == nil {
		return
	}
	l.state.Lock()
	defer l.state.Unlock()
	l.gen++
}

// dispatch runs the pump's dispatch decision under the lane: a line queued
// before the last rewind is stale and must not dispatch, and the draining mark
// offer sets commits with that check, so a rewind cannot slip between the
// decision and the turn it starts. offer runs holding the lane.
func (l *stateLane) dispatch(gen uint64, offer func() bool) (stale, queued bool) {
	if l == nil {
		return false, offer()
	}
	l.lock()
	defer l.unlock()
	if gen != l.stamp() {
		return true, false
	}
	return false, offer()
}

// quiesce waits, bounded, for the transaction holding the lane to finish,
// reporting whether the bound was hit. A nil lane is idle at once.
func (l *stateLane) quiesce(timeout time.Duration) bool {
	if l == nil {
		return false
	}
	l.state.Lock()
	ch := l.idle
	l.state.Unlock()
	select {
	case <-ch:
		return false
	case <-time.After(timeout):
		return true
	}
}
