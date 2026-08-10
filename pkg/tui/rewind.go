package tui

import "time"

// defaultDoubleEscWindow is how close two idle Esc presses must be to count as
// the rewind gesture when Options.DoubleEscWindow is left at zero.
const defaultDoubleEscWindow = 400 * time.Millisecond

// escToken cancels a pending lone-Esc flush. *time.Timer satisfies it; tests may
// substitute their own to avoid wall-clock timing.
type escToken interface {
	Stop() bool
}

// SetOnRewind arms or disarms the double-Esc rewind gesture after construction,
// for hosts that build the UI before they know what to rewind onto.
func (u *UI) SetOnRewind(cb func()) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.onRewind = cb
}

// SetIdle marks whether the agent is at rest awaiting input. The double-Esc
// rewind gesture is only recognized while idle, so mid-turn Esc keeps its
// interrupt role unchanged. Leaving the prompt cancels any half-armed rewind.
func (u *UI) SetIdle(idle bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.idle == idle {
		return
	}
	u.idle = idle
	if !idle && u.escPending {
		u.cancelRewindLocked() // a turn started while Esc was pending: drop the gesture
	}
}

// armRewindLocked defers a lone-Esc emission by one window so a second press can
// rewind instead. Caller holds the lock.
func (u *UI) armRewindLocked() {
	if u.rewTimer != nil {
		u.rewTimer.Stop()
	}
	fn := u.afterDelay
	if fn == nil {
		fn = time.AfterFunc // UI built without New (tests) still arms a real timer
	}
	t := fn(u.doubleEscWindow, func() { u.flushLoneEscape() })
	u.rewTimer = t
	u.escPending = true
}

// cancelRewindLocked aborts a pending rewind gesture without emitting anything.
func (u *UI) cancelRewindLocked() {
	if u.rewTimer != nil {
		u.rewTimer.Stop()
		u.rewTimer = nil
	}
	u.escPending = false
}

// flushLoneEscape emits the deferred single-Esc control once the rewind window
// elapses without a second press.
func (u *UI) flushLoneEscape() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.escPending {
		return // superseded by a rewind or cancelled on Close/SetIdle(false)
	}
	u.rewTimer = nil
	u.escPending = false
	u.emitControl(ControlEscape)
}

// triggerRewind runs the host's OnRewind off the key loop, so opening an
// interaction from it can never block further input routing.
func (u *UI) triggerRewind() {
	if cb := u.onRewind; cb != nil {
		go cb()
	}
}
