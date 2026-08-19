package main

import (
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
// rows above the prompt, hand over as one newline-joined message at the next
// step boundary (or as the next turn's prompt), and are recoverable to the
// editor until then. Lock order: q.mu before any tui.UI lock.
type steerQueue struct {
	ui     queueUI
	submit func(est int) // SetSubmit(sum) while anything is pending; nil-safe in main
	clear  func()        // SetSubmit(0) once the batch lands in state

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

// join builds one agent.Input from every queued item (caller holds the lock):
// texts joined by newline, Before blocks concatenated in submit order, Injected
// OR-ed, and a Delivered hook that clears accounting and echoes the labels once
// the batch actually lands.
func (s *steerQueue) join() agent.Input {
	texts := make([]string, 0, len(s.items))
	before := make([]llm.Message, 0, len(s.items))
	labels := make([]string, len(s.items))
	injected := false
	for i, it := range s.items {
		texts = append(texts, it.input.Text)
		before = append(before, it.input.Before...)
		if it.input.Injected {
			injected = true
		}
		labels[i] = it.label
	}
	s.items = nil

	out := agent.Input{Text: strings.Join(texts, "\n"), Before: before, Injected: injected}
	if labelJoin := strings.Join(labels, "\n"); labelJoin != "" {
		joined := labelJoin // captured for the closure; landed takes no lock
		out.Delivered = func() { s.landed(joined) }
	}
	s.refreshLocked()
	return out
}

// landed clears accounting and echoes the delivered labels on the loop goroutine.
// It must not take q.mu (Delivered can fire inside drainSteer).
func (s *steerQueue) landed(labels string) {
	if s.clear != nil {
		s.clear() // SetSubmit(0): pending owns the text once it lands
	}
	if s.ui != nil && labels != "" {
		s.ui.UserEcho(labels)
	}
}

// pull is the OnBoundary callback: hand over every queued item joined into one
// input at this step boundary. Returns nil when empty.
func (s *steerQueue) pull() []agent.Input {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) == 0 {
		return nil
	}
	return []agent.Input{s.join()}
}

// take returns the next joined batch to run as a follow-up turn, or false when
// empty (which also clears draining so a later offer starts a fresh drain).
func (s *steerQueue) take() (agent.Input, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) == 0 {
		s.draining = false // the drain goroutine is done; the next submit starts one
		return agent.Input{}, false
	}
	return s.join(), true
}

// stopDrain marks draining off after a Prompt error. Items stay queued as rows;
// a failing provider must not be hammered — the next submit delivers them.
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
	sum := 0
	for _, it := range s.items {
		sum += it.est
	}
	if sum > 0 && s.submit != nil {
		s.submit(sum) // SetSubmit replaces, so re-pushing the running total is safe
	} else if s.clear != nil {
		s.clear() // nothing pending: clear the submit bucket
	}
}
