package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// statusRecorder records the segment texts a gate publishes, safe for concurrent use.
type statusRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *statusRecorder) record(text, short string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, text)
}

func (r *statusRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *statusRecorder) first() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return ""
	}
	return r.calls[0]
}

func (r *statusRecorder) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return ""
	}
	return r.calls[len(r.calls)-1]
}

// newTypingGate builds a gate with short windows so tests stay deterministic; the
// idle window is wide by default so only an explicit release or cancel ends a hold.
func newTypingGate() *typingGate {
	return &typingGate{
		idle:    time.Hour,
		handoff: 30 * time.Millisecond,
		poll:    5 * time.Millisecond,
	}
}

// holdIn returns a goroutine that runs the gate and reports when it returned.
func (g *typingGate) holdIn(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() { g.hold(ctx); close(done) }()
	return done
}

func TestTypingGateHold(t *testing.T) {
	t.Parallel()

	t.Run("empty_draft_returns", func(t *testing.T) {
		g := newTypingGate()
		rec := &statusRecorder{}
		g.status = rec.record

		select {
		case <-g.holdIn(context.Background()):
		case <-time.After(2 * time.Second):
			t.Fatal("an empty draft must not hold the boundary")
		}
		assert.Equal(t, 0, rec.count(), "an unheld boundary publishes nothing")
	})

	t.Run("clear_releases", func(t *testing.T) {
		g := newTypingGate()
		rec := &statusRecorder{}
		g.status = rec.record

		g.edit("draft")
		done := g.holdIn(context.Background())
		// wait for the first countdown publish: proof the hold is engaged on the draft
		require.Eventually(t, func() bool { return rec.count() > 0 }, time.Second, time.Millisecond,
			"the hold must begin waiting on a visible draft")

		g.edit("") // Esc / backspace to empty releases at once (after the short handoff)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("clearing the prompt did not release the hold")
		}
	})

	t.Run("retype_after_clear_republishes", func(t *testing.T) {
		g := newTypingGate()
		rec := &statusRecorder{}
		g.status = rec.record

		// a fresh typing session after clearing must republish its own countdown, even
		// when the remaining seconds match what was shown before the clear
		g.edit("draft A")
		done := g.holdIn(context.Background())
		require.Eventually(t, func() bool { return rec.count() > 0 }, time.Second, time.Millisecond,
			"the first draft must publish its countdown")

		g.edit("")  // clear: arms the handoff grace
		g.edit("B") // retype within that grace
		require.Eventually(t, func() bool { return rec.count() >= 2 }, time.Second, time.Millisecond,
			"retyping must publish a fresh countdown rather than leave the stale one")

		g.edit("") // clear again so the hold can exit cleanly
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("clearing after the retype did not release the hold")
		}
	})

	t.Run("idle_window_releases", func(t *testing.T) {
		g := newTypingGate()
		g.idle = 40 * time.Millisecond

		g.edit("draft")
		select {
		case <-g.holdIn(context.Background()):
		case <-time.After(2 * time.Second):
			t.Fatal("the idle window must release the boundary without further edits")
		}
	})

	t.Run("queued_prompt_releases", func(t *testing.T) {
		g := newTypingGate()
		var pending atomic.Int32
		g.pending = func() int { return int(pending.Load()) }
		rec := &statusRecorder{}
		g.status = rec.record

		g.edit("draft")
		done := g.holdIn(context.Background())
		require.Eventually(t, func() bool { return rec.count() > 0 }, time.Second, time.Millisecond,
			"the hold must be waiting on the draft before a prompt lands")

		pending.Store(1) // a submitted line reached the queue during the hold
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("a queued prompt must release the boundary")
		}
	})

	t.Run("context_cancel_releases", func(t *testing.T) {
		g := newTypingGate()
		rec := &statusRecorder{}
		g.status = rec.record

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g.edit("draft")
		done := g.holdIn(ctx)
		require.Eventually(t, func() bool { return rec.count() > 0 }, time.Second, time.Millisecond,
			"the hold must be waiting before it is cancelled")

		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("a cancelled context did not release the hold")
		}
	})

	t.Run("taken_skips_handoff", func(t *testing.T) {
		g := newTypingGate()
		g.submitted() // a line left the editor and is reaching the queue
		g.taken()     // ...but it resolved, so there is no handoff to wait for

		select {
		case <-g.holdIn(context.Background()):
		case <-time.After(2 * time.Second):
			t.Fatal("taken must skip the handoff grace entirely")
		}
	})

	t.Run("late_clear_notification_does_not_stall", func(t *testing.T) {
		g := newTypingGate()
		g.handoff = time.Hour

		// the pump may resolve a submitted line before the async empty edit is
		// delivered; that late clear must not arm a fresh handoff
		g.edit("hello")
		g.submitted()
		g.taken()
		g.edit("")

		select {
		case <-g.holdIn(context.Background()):
		case <-time.After(2 * time.Second):
			t.Fatal("a late clear notification must not stall the boundary")
		}
	})

	t.Run("stale_handoff_expires_once", func(t *testing.T) {
		g := newTypingGate()
		g.handoff = 0 // grace already elapsed at every check

		g.submitted()
		<-g.holdIn(context.Background())
		assert.False(t, g.inFlight, "an elapsed handoff must clear so later boundaries never stall")
	})

	t.Run("countdown_status_set_and_cleared", func(t *testing.T) {
		g := newTypingGate()
		g.idle = 40 * time.Millisecond
		rec := &statusRecorder{}
		g.status = rec.record

		g.edit("draft")
		<-g.holdIn(context.Background())

		require.NotZero(t, rec.count())
		assert.Contains(t, rec.first(), "paused ", "the countdown must name the remaining seconds")
		assert.Empty(t, rec.last(), "release clears the published segment")
	})
}

func TestTypingGateEdit(t *testing.T) {
	t.Parallel()

	g := newTypingGate()
	g.edit("hello")
	assert.Equal(t, "hello", g.draft)
	assert.False(t, g.inFlight, "typing into an empty draft must not arm inFlight")

	at := g.at

	g.edit("world") // same non-empty -> non-empty transition keeps inFlight clear
	assert.Equal(t, "world", g.draft)
	assert.False(t, g.inFlight)
	assert.True(t, g.at.After(at), "each edit advances the change timestamp")

	g.edit("") // a real submit: ends the session; arming is submitted()'s job
	assert.Empty(t, g.draft)
	assert.False(t, g.inFlight, "an editor clear alone must not arm the handoff")
	sess := g.session

	// editing to empty again when already empty must not re-arm after taken cleared it
	g.taken()
	assert.False(t, g.inFlight)
	g.edit("")
	assert.False(t, g.inFlight, "empty -> empty is not a submit; inFlight stays clear")
	assert.Equal(t, sess, g.session, "an already-empty draft must not start a new session")

	g.edit("draft") // typing again clears the stale handoff arm
	assert.Equal(t, "draft", g.draft)
	assert.False(t, g.inFlight)

	// clearing that non-empty draft starts a fresh session for the next retype
	g.edit("")
	assert.Greater(t, g.session, sess)
}

func TestTypingGateSubmitted(t *testing.T) {
	t.Parallel()

	g := newTypingGate()
	g.edit("draft")
	g.submitted()
	assert.True(t, g.inFlight, "a submitted line arms the handoff grace")

	g.edit("next") // typing again supersedes the handoff wait
	assert.False(t, g.inFlight)

	g.submitted()
	g.taken()
	assert.False(t, g.inFlight, "taken ends the grace once the line resolves")
}
