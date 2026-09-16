package app

import (
	"context"
	"strings"
	"sync"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// queueUI is the narrow TUI surface the steer queue needs; *tui.UI satisfies it.
type queueUI interface {
	SetQueued(texts []string)
	PrependInput(text string)
	SetInput(text string)
	UserEcho(text string)
}

// steerQueue holds prompts submitted while a turn runs: they render as dimmed
// rows above the prompt, hand over as newline-joined messages (one per
// provenance run) at the next step boundary or behind the next turn's prompt,
// and are recoverable to the editor until then. Lock order: q.mu before any
// tui.UI lock.
type steerQueue struct {
	ui     queueUI
	submit func(est int) // SetSubmit(sum) while anything is pending; nil-safe in main
	clear  func()        // SetSubmit(0) once the batch and its reads have landed

	mu       sync.Mutex
	items    []steerItem // oldest first
	draining bool        // a goroutine is running turns and will drain us
}

// steerItem is one queued prompt: its expanded input, the typed line shown in
// rows / recalled / echoed, and the token estimate carried until it lands.
type steerItem struct {
	input agent.Input // expanded text + Before blocks, ready to send
	label string      // the typed line, shown in rows / recalled / echoed
	est   int         // token estimate carried in the submit bucket until landed
}

func newSteerQueue(ui queueUI, submit func(int), clear func()) *steerQueue {
	return &steerQueue{ui: ui, submit: submit, clear: clear}
}

// offer is the pump entry. When a drain goroutine runs it queues the item and
// returns true; otherwise it marks draining and returns false so the caller
// spawns the single drain goroutine with this input.
func (s *steerQueue) offer(in agent.Input, label string, est int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.draining {
		s.draining = true // a turn is about to run; later offers queue
		return false
	}
	s.items = append(s.items, steerItem{input: in, label: label, est: est})
	s.refreshLocked()
	return true
}

// join splits the queue into contiguous provenance runs (caller holds the
// lock): items sharing an Injected value merge into one input, texts joined by
// newline, Before blocks and After resolvers chained in submit order. A
// provenance change starts a new input, so a mixed user + system batch keeps
// per-item attribution on the transcript rows instead of one OR-ed flag. The
// final run clears accounting once everything behind it has landed; each run
// echoes its labels as its message lands.
func (s *steerQueue) join() []agent.Input {
	type runAcc struct {
		in     agent.Input
		afters []func(context.Context) []llm.Message
		labels []string
	}
	var runs []runAcc

	cur := func() *runAcc {
		return &runs[len(runs)-1]
	}
	for _, it := range s.items {
		if len(runs) == 0 || cur().in.Injected != it.input.Injected {
			runs = append(runs, runAcc{
				in: agent.Input{Injected: it.input.Injected, Prepared: it.input.Prepared}})
		}
		r := cur()
		// merged text is only skip-the-seam clean when every item already ran it
		r.in.Prepared = r.in.Prepared && it.input.Prepared
		if r.in.Text != "" {
			r.in.Text += "\n"
		}
		r.in.Text += it.input.Text
		r.in.Before = append(r.in.Before, it.input.Before...)
		if it.input.After != nil {
			r.afters = append(r.afters, it.input.After)
		}
		if it.label != "" {
			r.labels = append(r.labels, it.label)
		}
	}
	s.items = nil

	out := make([]agent.Input, len(runs))
	for i, r := range runs {
		r.in.After = joinAfter(r.afters)
		if label := strings.Join(r.labels, "\n"); label != "" {
			joined := label // captured for the closure; landed takes no lock
			r.in.Delivered = func() { s.landed(joined) }
		}
		out[i] = r.in
	}
	if len(out) > 0 {
		out[len(out)-1].Settled = s.settled // released only once every read landed
	}
	s.refreshLocked()
	return out
}

// joinAfter chains every queued item's resolver into one, run in submit order.
// It returns nil when the batch has none, keeping the common case unset.
func joinAfter(afters []func(context.Context) []llm.Message) func(context.Context) []llm.Message {
	if len(afters) == 0 {
		return nil
	}
	return func(ctx context.Context) []llm.Message {
		msgs := make([]llm.Message, 0, 2*len(afters))
		for _, fn := range afters {
			msgs = append(msgs, fn(ctx)...)
		}
		return msgs
	}
}

// landed echoes the delivered labels on the loop goroutine. It must not take q.mu
// (Delivered can fire inside drainSteer).
func (s *steerQueue) landed(labels string) {
	if s.ui != nil && labels != "" {
		s.ui.UserEcho(labels)
	}
}

// settled releases the submit reserve once the batch and everything behind it has
// been accounted. Same locking rule as landed.
func (s *steerQueue) settled() {
	if s.clear != nil {
		s.clear() // SetSubmit(0): pending owns the text and its reads now
	}
}

// pending reports how many prompts are queued for the next boundary.
func (s *steerQueue) pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.items)
}

// pull is the OnBoundary callback: hand over every queued item joined into
// provenance runs at this step boundary. Returns nil when empty.
func (s *steerQueue) pull() []agent.Input {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.items) == 0 {
		return nil
	}
	return s.join()
}

// take returns the next batch of joined runs to deliver behind the next turn's
// prompt, or false when empty (which also clears draining so a later offer
// starts a fresh drain).
func (s *steerQueue) take() ([]agent.Input, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.items) == 0 {
		s.draining = false // the drain goroutine is done; the next submit starts one
		return nil, false
	}
	return s.join(), true
}

// stopDrain marks draining off after a Prompt error. Items stay queued as rows
// so a failing provider is not hammered, and the next submit delivers them.
func (s *steerQueue) stopDrain() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.draining = false
}

// recall pops the newest item back into the editor and refreshes rows and
// accounting. It reports whether anything was recalled.
func (s *steerQueue) recall() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.items) == 0 {
		return false
	}
	i := len(s.items) - 1 // LIFO: the most recently queued message first
	last := s.items[i]
	s.items = s.items[:i]
	if s.ui != nil && last.label != "" {
		s.ui.PrependInput(last.label)
	}
	s.refreshLocked()
	return true
}

// abort recovers every queued item into the editor, joined by newlines ahead of
// any draft, and clears rows/items/accounting. Called before an interrupt so a
// mid-turn message is never lost.
func (s *steerQueue) abort() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.items) == 0 {
		return
	}
	labels := make([]string, len(s.items))
	for i, it := range s.items {
		labels[i] = it.label
	}
	s.items = nil
	if s.ui != nil && len(labels) > 0 {
		s.ui.PrependInput(strings.Join(labels, "\n"))
	}
	s.refreshLocked()
}

// refreshLocked re-renders the queued rows and keeps the submit bucket in step.
// Caller holds q.mu; UI calls happen under it (queue→UI lock order).
func (s *steerQueue) refreshLocked() {
	if s.ui != nil {
		labels := make([]string, len(s.items))
		for i, it := range s.items {
			labels[i] = it.label
		}
		s.ui.SetQueued(labels)
	}
	var sum int
	for _, it := range s.items {
		sum += it.est
	}
	if sum > 0 && s.submit != nil {
		s.submit(sum) // SetSubmit replaces, so re-pushing the running total is safe
	} else if s.clear != nil {
		s.clear() // nothing pending: clear the submit bucket
	}
}
