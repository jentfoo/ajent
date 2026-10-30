package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tui"
	tuisink "github.com/jentfoo/ajent/pkg/tui/sink"
)

// Recall refusals: the newest queued prompt's payload cannot cross the wire,
// so a remote surface recalls it at the session's own terminal instead.
var (
	errSecretQueued = errors.New("the newest queued prompt carries a secret prefix")
	errImageQueued  = errors.New("the newest queued prompt carries images")
)

// newQueueID mints a short random correlation token naming one prompt
// submission on queue rows, drop events and the committed entry.
func newQueueID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("q%x", time.Now().UnixNano()) // still unique within the process
	}
	return hex.EncodeToString(b[:])
}

// queueUI is the narrow TUI surface the steer queue needs, *tui.UI satisfies it.
type queueUI interface {
	SetQueued(rows []ctl.QueueRow)
	PrependInput(text string)
	SetInput(text string)
	UserEcho(text string)
	UserImages(imgs []tui.Image)
	// Interactive reports whether a local editor exists to recover queued
	// prompts into; serve mode has none and an interrupt drops them instead.
	Interactive() bool
}

// steerQueue holds prompts submitted while a turn runs: they render as dimmed
// rows above the prompt, hand over as newline-joined messages (one per
// provenance run) at the next step boundary or behind the next turn's prompt,
// and are recoverable to the editor until then. Lock order: q.mu before any
// tui.UI lock.
type steerQueue struct {
	ui        queueUI
	submit    func(est int)                           // SetSubmit(sum) while anything is pending
	clear     func()                                  // SetSubmit(0) once the batch and its reads have landed
	waitReady func(context.Context)                   // stager.WaitReady: block until staged commands finish
	takeReady func() []agent.MessageInfo              // stager.TakeReady: consume finished staged output
	dropped   func(cause string, rows []ctl.QueueRow) // names queued prompts a removal cleared, for pending rows

	mu       sync.Mutex
	items    []steerItem // oldest first
	draining bool        // a goroutine is running turns and will drain us
	gen      uint64      // bumped per dispatch, so a late drain exit knows it was superseded
}

// steerItem is one queued prompt: its expanded input (carrying the row's
// correlation id), the typed line shown in rows / recalled / echoed, and the
// token estimate carried until it lands.
type steerItem struct {
	input agent.Input // expanded text + Before blocks, ready to send
	label string      // the typed line, shown in rows / recalled / echoed
	est   int         // token estimate carried in the submit bucket until landed
}

// rowID is the item's queue-row correlation. Offer and hold stamp any input
// that arrived without one, so every wire event about this row names it.
func (it steerItem) rowID() string {
	if len(it.input.QueueIDs) == 0 {
		return ""
	}
	return it.input.QueueIDs[0]
}

// newSteerItem stamps a missing queue-row id: pump-submitted prompts carry one
// from submit time, so the row correlates with its committed entry.
func newSteerItem(in agent.Input, label string, est int) steerItem {
	if len(in.QueueIDs) == 0 {
		in.QueueIDs = []string{newQueueID()}
	}
	return steerItem{input: in, label: label, est: est}
}

func newSteerQueue(ui queueUI, submit func(int), clear func(), waitReady func(context.Context), takeReady func() []agent.MessageInfo, dropped func(cause string, rows []ctl.QueueRow)) *steerQueue {
	return &steerQueue{ui: ui, submit: submit, clear: clear, waitReady: waitReady, takeReady: takeReady, dropped: dropped}
}

// offer is the pump entry. When a drain goroutine runs it queues the item and
// returns true, otherwise it marks draining and returns false so the caller
// spawns the single drain goroutine with this input.
func (s *steerQueue) offer(in agent.Input, label string, est int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.draining {
		s.draining = true // a turn is about to run, later offers queue
		return false
	}
	s.items = append(s.items, newSteerItem(in, label, est))
	s.refreshLocked()
	return true
}

// nextGen stamps a new dispatch with a generation. The drain exit carries it
// back to settleIdle, which restores idle only while no newer stamp exists.
func (s *steerQueue) nextGen() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.gen++
	return s.gen
}

// settleIdle runs set(true) unless a newer dispatch took over since gen was
// stamped. The compare and the write share q.mu with nextGen, so a stale exit
// either restores before the next dispatch reads the queue or is skipped.
func (s *steerQueue) settleIdle(gen uint64, set func(bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if gen == s.gen {
		set(true)
	}
}

// hold queues an item while no drain runs: a parked-gate input the gate did not
// capture. The pump offered it first, so reset that drain mark or the gated
// submit would queue behind it and never run. Delivery waits for whatever turn
// the gated submit starts next.
func (s *steerQueue) hold(in agent.Input, label string, est int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.draining = false
	s.items = append(s.items, newSteerItem(in, label, est))
	s.refreshLocked()
}

// join splits the queue into contiguous provenance runs (caller holds the
// lock): items sharing an Injected value merge into one input, texts joined by
// newline, Before blocks and After resolvers chained in submit order. A
// provenance change starts a new input, so a mixed user + system batch keeps
// per-item attribution on the transcript rows instead of one OR-ed flag. The
// final run clears accounting once everything behind it has landed. Each run
// echoes its labels and images as its message lands, so Input.Echo is never
// carried: a loop-side echo too would draw twice. Blocks concatenate in queue order
// across a run's items, so the joined message need not match any one label.
func (s *steerQueue) join() []agent.Input {
	type runAcc struct {
		in     agent.Input
		afters []func(context.Context) []llm.Message
		labels []string
	}
	runs := make([]runAcc, 0, len(s.items))

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
		r.in.Blocks = append(r.in.Blocks, it.input.Blocks...)
		r.in.Before = append(r.in.Before, it.input.Before...)
		// every member's correlation rides the run, so one joined entry resolves
		// each row that contributed to it
		r.in.QueueIDs = append(r.in.QueueIDs, it.input.QueueIDs...)
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
		label := strings.Join(r.labels, "\n")
		blocks := r.in.Blocks
		if label != "" || llm.HasImageBlocks(blocks) { // image-only runs land visibly too
			r.in.Delivered = func() { s.landed(label, blocks) }
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

// landed echoes the delivered labels and images on the loop goroutine. It must
// not take q.mu (Delivered can fire inside drainSteer).
func (s *steerQueue) landed(labels string, blocks llm.BlockList) {
	if labels != "" {
		s.ui.UserEcho(labels)
	}
	if imgs := tuisink.Images(blocks); len(imgs) > 0 {
		s.ui.UserImages(imgs)
	}
}

// settled releases the submit reserve once the batch and everything behind it has
// been accounted. Same locking rule as landed.
func (s *steerQueue) settled() {
	s.clear() // SetSubmit(0): pending owns the text and its reads now
}

// pending reports how many prompts are queued for the next boundary.
func (s *steerQueue) pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.items)
}

// drainLive reports whether a drain goroutine owns or is settling turns, so
// console state writes must queue behind it
func (s *steerQueue) drainLive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.draining
}

// pull is the OnBoundary callback: hand over every queued item joined into
// provenance runs at this step boundary. Staged shell output that already
// finished rides ahead of the batch. Returns nil when empty.
func (s *steerQueue) pull() []agent.Input {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.items) == 0 {
		return nil
	}
	s.attachFlushedLocked()
	return s.join()
}

// take returns the next batch of joined runs to deliver behind the next turn's
// prompt, or false when empty (which also clears draining so a later offer
// starts a fresh drain). It first waits out any still-running staged command,
// outside the lock, so its output rides the batch it was meant for.
func (s *steerQueue) take(ctx context.Context) ([]agent.Input, bool) {
	s.mu.Lock()
	if len(s.items) == 0 {
		s.draining = false // the drain goroutine is done, the next submit starts one
		s.mu.Unlock()
		return nil, false
	}
	s.mu.Unlock()

	// no q.mu: a long staged command must not block Esc or submits
	s.waitReady(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.items) == 0 { // aborted while waiting: nothing consumes the output
		s.draining = false
		return nil, false
	}
	s.attachFlushedLocked()
	return s.join(), true
}

// attachFlushedLocked folds finished staged shell output ahead of the first
// queued item. Deferring the consume here is what keeps it safe: output only
// leaves the stager at the moment its batch delivers, so an aborted or recalled
// prompt cannot lose it. Caller holds q.mu; items must be non-empty.
func (s *steerQueue) attachFlushedLocked() {
	before := s.takeReady()
	if len(before) == 0 {
		return
	}
	first := &s.items[0]
	first.input.Before = append(before, first.input.Before...)
	first.est += tokens.EstimateMessages(agent.BeforeMessages(before))
}

// stopDrain marks draining off while items stay queued as rows: a failing provider
// is not hammered and parked-gate steering holds; the next submit delivers them.
func (s *steerQueue) stopDrain() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.draining = false
}

// recall pops the newest item back into the editor and refreshes rows,
// accounting and the removal notice, so attached surfaces resolve its pending
// row like any other queue exit. It reports whether anything was recalled.
func (s *steerQueue) recall() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.items) == 0 {
		return false
	}
	item := s.items[len(s.items)-1]
	s.items = s.items[:len(s.items)-1]
	if label := restoredLabel(item); label != "" {
		s.ui.PrependInput(label)
	}
	s.announceRecallLocked(item)
	return true
}

// recallInto pops the newest item for a remote surface: rows, accounting and
// the removal notice update exactly like the keyboard path, but no editor is
// touched — the returned text fills the caller's own draft instead. An empty
// queue answers "" with no error; an item whose payload cannot cross the wire
// refuses in place and stays queued.
func (s *steerQueue) recallInto() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.items) == 0 {
		return "", nil
	}
	item := s.items[len(s.items)-1]
	label := queuedLabel(item)
	switch {
	case session.HasSecretPrefix(secretPrefix, label):
		return "", errSecretQueued
	case llm.HasImageBlocks(item.input.Blocks):
		return "", errImageQueued
	}
	s.items = s.items[:len(s.items)-1]
	s.announceRecallLocked(item)
	return restoredLabel(item), nil
}

// announceRecallLocked publishes the removal notice naming the row and
// refreshes rows and accounting after any recall. Caller holds q.mu.
func (s *steerQueue) announceRecallLocked(it steerItem) {
	if text := queuedLabel(it); text != "" && s.dropped != nil {
		s.dropped("recalled", []ctl.QueueRow{{ID: it.rowID(), Text: text}})
	}
	s.refreshLocked()
}

// restoredLabel refiles an item's image blocks under fresh slots, their
// submission slots having been consumed, and prefixes the token onto the typed
// label so a recalled or aborted message keeps its images.
func restoredLabel(it steerItem) string {
	label := it.label
	if tok := pendingImages.refile(it.input.Blocks); tok != "" {
		label = strings.TrimSpace(tok + " " + label)
	}
	return label
}

// queuedLabel is what a queue row shows: an image-only item has no typed line,
// so its blocks stand in via the shared placeholder instead of an empty label.
func queuedLabel(it steerItem) string {
	if it.label != "" {
		return it.label
	}
	var parts []string
	for _, ib := range llm.ImageBlocks(it.input.Blocks) {
		parts = append(parts, llm.ImagePlaceholder(ib.Data, ""))
	}
	return strings.Join(parts, ", ")
}

// abort recovers every queued item into the editor, joined by newlines ahead of
// any draft, and clears rows/items/accounting. Called before an interrupt so a
// mid-turn message is never lost. A run with no editor (serve mode) drops the
// queued items instead: there is nothing to recover into, and pretending a
// recall happened would hide the loss. The refresh runs on an empty queue too:
// a directly dispatched prompt parked in the typing hold still holds its submit
// reserve, and the interrupt that follows drops it without ever landing.
func (s *steerQueue) abort() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.items) > 0 {
		rows := s.queuedRows()
		if s.ui.Interactive() {
			recovered := make([]string, len(s.items))
			for i, it := range s.items {
				recovered[i] = restoredLabel(it)
			}
			s.ui.PrependInput(strings.Join(recovered, "\n"))
		}
		s.items = nil
		if s.dropped != nil {
			s.dropped("interrupt", rows) // pending rows resolve instead of vanishing
		}
	}
	s.refreshLocked()
}

// clearForRewind drops every queued item, publishing the removal notice and
// the emptied rows inside this one critical section: a prompt re-offered after
// the rewind publishes fresh rows no late drop can remove. Returns the cleared
// labels for the recorded notice entry; a rewind re-targets nothing, so the
// prompts stay recallable from editor history.
func (s *steerQueue) clearForRewind() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.items) == 0 {
		return nil
	}
	rows := s.queuedRows()
	s.items = nil
	if s.dropped != nil {
		s.dropped("rewind", rows)
	}
	s.refreshLocked()
	return queueRowTexts(rows)
}

// queuedRows renders every queued item's wire row, oldest first.
func (s *steerQueue) queuedRows() []ctl.QueueRow {
	rows := make([]ctl.QueueRow, len(s.items))
	for i, it := range s.items {
		rows[i] = ctl.QueueRow{ID: it.rowID(), Text: queuedLabel(it)}
	}
	return rows
}

// refreshLocked re-renders the queued rows and keeps the submit bucket in step.
// Caller holds q.mu, UI calls happen under it (queue→UI lock order).
func (s *steerQueue) refreshLocked() {
	s.ui.SetQueued(s.queuedRows())
	var sum int
	for _, it := range s.items {
		sum += it.est
	}
	if sum > 0 {
		s.submit(sum) // SetSubmit replaces, so re-pushing the running total is safe
	} else {
		s.clear() // nothing pending: clear the submit bucket
	}
}
