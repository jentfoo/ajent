package app

import (
	"context"
	"strconv"
	"sync"
	"time"
)

const (
	typingIdle    = 10 * time.Second       // unchanged prompt resumes the boundary
	typingHandoff = 300 * time.Millisecond // grace for a submitted line to reach the queue
	typingPoll    = 100 * time.Millisecond // hold re-check interval
)

// typingGate holds a step boundary while the user is mid-message, so a prompt they
// are still typing lands in this step instead of the next. Windows are fields so tests can shorten them.
type typingGate struct {
	pending func() int               // queued steer items; nil-safe
	status  func(text, short string) // status segment; empty text removes it
	idle    time.Duration            // unchanged draft resumes the boundary after this
	handoff time.Duration            // grace for a submitted line to reach the queue
	poll    time.Duration            // hold re-check interval

	mu       sync.Mutex
	draft    string    // editor text as of the last edit
	at       time.Time // when the draft or handoff last changed
	inFlight bool      // a submitted line may still be reaching the queue
	session  int       // incremented on each clear, so a retype restarts the countdown
}

// edit records a draft change. A clear ends the typing session; a visible draft
// supersedes any handoff wait.
func (g *typingGate) edit(text string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case text != "":
		g.inFlight = false
	case g.draft != "":
		g.session++ // a clear ends one session; retyping begins another
	}
	g.draft = text
	g.at = time.Now()
}

// submitted arms the handoff grace for a line that left the editor but has not
// reached the queue yet.
func (g *typingGate) submitted() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inFlight = true
	g.at = time.Now()
}

// taken ends the handoff grace: the submitted line resolved, as a queued item or
// as the start of its own turn.
func (g *typingGate) taken() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inFlight = false
}

// hold is the AwaitInput hook: it waits at a step boundary while a draft is being
// typed or a just-submitted line is still reaching the queue. ctx cancels on an interrupt.
func (g *typingGate) hold(ctx context.Context) {
	var shown bool   // published; cleared on exit so an unheld boundary repaints nothing
	last := -1       // last displayed second; forces the first publish
	lastSession := 0 // session the current countdown dedup is for
	defer func() {
		if !shown || g.status == nil {
			return
		}
		g.status("", "")
	}()

	for {
		// a queued prompt already lands at this boundary, whatever the editor says
		if g.pending != nil && g.pending() > 0 {
			return
		}

		g.mu.Lock()
		draft := g.draft
		inFlight := g.inFlight
		at := g.at
		session := g.session
		g.mu.Unlock()

		var deadline time.Time
		countdown := false
		switch {
		case draft != "":
			deadline = at.Add(g.idle)
			countdown = true // only a visible draft shows the countdown
			// a clear ended the previous session; retyping must republish its own
			// countdown, even when the remaining seconds match what was shown before
			if session != lastSession {
				last = -1
				lastSession = session
			}
		case inFlight:
			// no flash during the handoff grace: nothing should blink for 300ms
			deadline = at.Add(g.handoff)
		default:
			return
		}

		if !g.wait(ctx, deadline, countdown, &shown, &last) {
			if !countdown {
				// the handoff grace elapsed; it must never stall a later boundary too
				g.mu.Lock()
				g.inFlight = false
				g.mu.Unlock()
			}
			return // cancelled or window elapsed; defer clears a published segment
		}
	}
}

// wait sleeps one poll step toward deadline. It publishes the remaining-seconds
// status when counting down and republishes only as the displayed second changes;
// it reports false once cancelled or the window elapsed.
func (g *typingGate) wait(ctx context.Context, deadline time.Time, countdown bool, shown *bool, last *int) bool {
	if ctx.Err() != nil {
		return false
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false // the window elapsed; let the boundary resume
	}
	poll := g.poll
	if poll <= 0 {
		poll = typingPoll // a zero poll would spin the hold hot
	}
	timer := time.NewTimer(min(remaining, poll))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
	}
	if countdown && g.status != nil {
		g.publishStatus(deadline, shown, last)
	}
	return true
}

// publishStatus shows the remaining idle seconds as a status segment, only when the
// displayed second changes.
func (g *typingGate) publishStatus(deadline time.Time, shown *bool, last *int) {
	secs := int((time.Until(deadline) + time.Second - 1) / time.Second)
	if secs <= 0 || secs == *last { // never show 0; republish only on a second change
		return
	}
	*last = secs
	s := strconv.Itoa(secs)
	g.status("paused "+s+"s", s+"s")
	*shown = true
}
