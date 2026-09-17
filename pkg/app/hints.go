package app

import (
	"sync"
	"time"

	"github.com/jentfoo/ajent/pkg/tui"
)

// hintBoard arbitrates the single "hint" status segment. Holders own slots; only the most
// recently requested live slot renders, so a newer hint takes the line from an older one
// and hands it back when freed. Safe for concurrent use; a nil board is a no-op.
type hintBoard struct {
	set   func(text, short string) // status writer; nil drops every update
	mu    sync.Mutex
	seq   int         // request counter, order of arrival
	live  []*hintSlot // active slots, oldest first
	shown string      // last painted text
	clean bool        // shown is current
}

// newHintBoard returns a board writing through set. Tests inject a recorder; production
// uses uiHintBoard.
func newHintBoard(set func(text, short string)) *hintBoard {
	return &hintBoard{set: set}
}

// uiHintBoard writes hints to the front end's hint segment.
func uiHintBoard(ui *tui.UI) *hintBoard {
	if ui == nil {
		return &hintBoard{}
	}
	return newHintBoard(func(text, short string) {
		ui.SetStatusSegment(segment(segHint, text, short))
	})
}

// hintSlot is one holder's claim on the hint line: shows what its owner last set while it
// holds the line, and keeps that text ready if it wins the line again after being masked.
type hintSlot struct {
	b     *hintBoard
	id    int
	text  string
	short string
	stop  func() // pending expiry, stopped when freed early; nil unless timed
}

// Request takes a slot showing text, newer than every slot held so far. nil for an empty
// text or a nil board.
func (b *hintBoard) Request(text, short string) *hintSlot {
	if b == nil || text == "" {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	b.seq++
	s := &hintSlot{b: b, id: b.seq, text: text, short: short}
	b.live = append(b.live, s)
	b.paintLocked()
	return s
}

// Set replaces the slot's text. A masked slot only stores it, so its newest text shows if
// it wins the line again later.
func (s *hintSlot) Set(text, short string) {
	if s == nil || s.b == nil {
		return
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()

	if !s.liveLocked() {
		return // freed: a late update from an expired owner is dropped
	}
	if s.text == text && s.short == short {
		return
	}
	s.text, s.short = text, short
	s.b.paintLocked()
}

// Free releases the claim, handing the line to the next most recently requested live slot.
// Idempotent; freeing a masked slot changes nothing shown.
func (s *hintSlot) Free() {
	if s == nil || s.b == nil {
		return
	}
	b := s.b
	b.mu.Lock()
	defer b.mu.Unlock()

	for i, held := range b.live {
		if held.id == s.id {
			b.live = append(b.live[:i], b.live[i+1:]...)
			break
		}
	}
	if s.stop != nil {
		s.stop() // idempotent, and safe to call from the expiry itself
	}
	s.text = "" // further Sets fall out of the liveness check
	b.paintLocked()
}

// ShowFor requests a slot that frees itself after d, so a one-shot notice cannot mask an
// older holder indefinitely. nil for empty text or a non-positive d.
func (b *hintBoard) ShowFor(text, short string, d time.Duration) *hintSlot {
	s := b.Request(text, short)
	if s == nil || d <= 0 {
		return s
	}
	timer := time.AfterFunc(d, s.Free)
	s.stop = func() { _ = timer.Stop() }
	return s
}

// hintLine is one owner's view of the board, shaped like a plain status writer: an empty
// text frees the line. Touched by its own goroutine only; the board itself is safe across
// them.
type hintLine struct {
	b    *hintBoard
	slot *hintSlot
}

// Line returns a fresh claim handle on b.
func (b *hintBoard) Line() *hintLine {
	if b == nil {
		return nil
	}
	return &hintLine{b: b}
}

// Set shows text on this owner's line, taking the claim on first use and releasing it when
// text is empty. Repeated calls refresh one claim without jumping the queue.
func (l *hintLine) Set(text, short string) {
	if l == nil {
		return
	}
	if text == "" {
		l.slot.Free() // Free is nil-safe
		l.slot = nil
		return
	}
	if l.slot == nil {
		l.slot = l.b.Request(text, short)
		return
	}
	l.slot.Set(text, short)
}

// liveLocked reports whether a claim remains. Caller holds b.mu.
func (s *hintSlot) liveLocked() bool {
	for _, held := range s.b.live {
		if held.id == s.id {
			return true
		}
	}
	return false
}

// paintLocked renders the winning slot, or clears the segment when none is alive. Writes
// only on a change, so per-second updates from masked holders stay silent. Caller holds
// b.mu.
func (b *hintBoard) paintLocked() {
	if b.set == nil {
		return
	}
	var text, short string
	if n := len(b.live); n > 0 {
		text, short = b.live[n-1].text, b.live[n-1].short // newest live request wins
	}
	if b.clean && text == b.shown {
		return
	}
	b.shown, b.clean = text, true
	b.set(text, short)
}
