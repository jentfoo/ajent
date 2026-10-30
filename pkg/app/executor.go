package app

import (
	"context"
	"sync"
)

// controlExecutor is the one lane every mutating control verb runs on: submit,
// interrupt, rewind and mode cycle. It serializes them so guards revalidate at
// execution time (never success-after-effect) and a rewind cannot interleave
// with another session mutation. Interrupts jump the lane, so Esc latency never
// waits behind a queued rewind. The prompt pump stays the only turn starter:
// verbs route through the same hub entries the keyboard uses, so one dispatch
// path holds.
type controlExecutor struct {
	root context.Context // app-owned root: cancelled shutdown fails verbs legibly

	mu        sync.Mutex
	queue     []func()
	interrupt []func() // priority lane, popped first
	wake      chan struct{}
}

// newControlExecutor builds the lane over root.
func newControlExecutor(root context.Context) *controlExecutor {
	return &controlExecutor{root: root, wake: make(chan struct{}, 1)}
}

// start runs the lane until the queue drains past root cancellation.
func (e *controlExecutor) start() {
	go e.loop()
}

// run queues fn for execution, reporting false once the lane is shutting down.
func (e *controlExecutor) run(fn func()) bool {
	return e.enqueue(fn, false)
}

// runPriority queues fn ahead of everything already queued.
func (e *controlExecutor) runPriority(fn func()) bool {
	return e.enqueue(fn, true)
}

// enqueue parks fn on one of the lanes and wakes the loop. The lane choice is
// the priority contract; pop order lives in loop.
func (e *controlExecutor) enqueue(fn func(), priority bool) bool {
	e.mu.Lock()
	if e.root.Err() != nil {
		e.mu.Unlock()
		return false
	}
	if priority {
		e.interrupt = append(e.interrupt, fn)
	} else {
		e.queue = append(e.queue, fn)
	}
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
	return true
}

// loop pops interrupt-lane closures first, then the queue, so a verb queued
// behind slow work cannot delay an interrupt. Shutdown stops the waits; queued
// closures still run once, replying their unavailable refusals.
func (e *controlExecutor) loop() {
	for {
		e.mu.Lock()
		var fn func()
		switch {
		case len(e.interrupt) > 0:
			fn = e.interrupt[0]
			e.interrupt = e.interrupt[1:]
		case len(e.queue) > 0:
			fn = e.queue[0]
			e.queue = e.queue[1:]
		}
		drained := fn == nil
		e.mu.Unlock()

		if drained {
			if e.root.Err() != nil {
				return
			}
			select {
			case <-e.wake:
			case <-e.root.Done():
			}
			continue
		}
		fn()
	}
}
