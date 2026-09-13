package tui

import (
	"context"
	"strconv"
	"sync"
)

// A modal replaces the composer, so it claims a larger share of the live block than
// the input's half: interactionShareNum of every interactionFree rows still free,
// floored at minInteractionRows and one row over the editor's cap.
const (
	interactionShareNum = 2 // numerator of the free-row share
	interactionFree     = 3 // denominator of the free-row share
	minInteractionRows  = 3 // prompt row, one list row, overflow footer
)

// interactor renders into the live block and consumes keys. Implementations
// hold their own result, which the calling method reads once done closes.
type interactor interface {
	// rows renders the interaction, capped at maxRows, with the caret position.
	rows(t Theme, width, maxRows int) (rows []string, caretRow, caretCol int)
	// key applies one key, reporting whether the interaction resolved and with
	// what error.
	key(k key) (done bool, err error)
	// summary is the one line committed to history once resolved.
	summary(t Theme) string
}

// pending is one queued or active interaction.
type pending struct {
	it   interactor
	done chan struct{}
	err  error
	once sync.Once
}

func newPending(it interactor) *pending {
	return &pending{it: it, done: make(chan struct{})}
}

// resolve completes the interaction exactly once, so a cancellation racing a
// keystroke settles on whichever arrived first. It reports whether this call won.
func (p *pending) resolve(err error) bool {
	var won bool
	p.once.Do(func() { p.err = err; won = true; close(p.done) })
	return won
}

// run registers an interaction and blocks until it resolves, ctx ends or the UI
// closes. Interactions queue in arrival order rather than being refused, since
// parallel tool calls will each want to ask something.
func (u *UI) run(ctx context.Context, it interactor) error {
	if u.mode == ModePlain {
		return u.runPlain(ctx, it)
	}
	p := newPending(it)
	if err := u.enqueue(p); err != nil {
		return err
	}
	return u.wait(ctx, p)
}

// enqueue registers a pending interaction and promotes it if nothing is active.
func (u *UI) enqueue(p *pending) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return ErrCancelled
	}
	u.queue = append(u.queue, p)
	if u.act == nil {
		u.promote()
	}
	u.repaint()
	return nil
}

// wait blocks until p resolves, ctx ends or the UI closes.
func (u *UI) wait(ctx context.Context, p *pending) error {
	select {
	case <-p.done:
		return p.err
	case <-ctx.Done():
		u.abandon(p)
		return p.err // resolve keeps the first result, so an answer given in the same instant survives
	case <-u.done:
		p.resolve(ErrCancelled)
		return p.err
	}
}

// promote makes the head of the queue active. Caller holds the lock.
func (u *UI) promote() {
	if len(u.queue) == 0 {
		u.act = nil
		return
	}
	u.act = u.queue[0]
}

// abandon removes an interaction the caller stopped waiting for.
func (u *UI) abandon(p *pending) {
	u.mu.Lock()
	defer u.mu.Unlock()

	p.resolve(ErrCancelled)
	u.dequeue(p)
	u.repaint()
}

// dequeue drops p from the queue and re-promotes. Caller holds the lock.
func (u *UI) dequeue(p *pending) {
	for i, q := range u.queue {
		if q == p {
			u.queue = append(u.queue[:i], u.queue[i+1:]...)
			break
		}
	}
	if u.act == p {
		u.promote()
	}
}

// routeKey feeds a key to the active interaction. Caller holds the lock.
func (u *UI) routeKey(k key) {
	p := u.act
	done, err := p.it.key(k)
	if !done {
		u.repaint()
		return
	}
	// resolve before committing so an external resolver that won the race is not
	// double-committed or dequeued; only the winner writes history.
	won := p.resolve(err)
	if won {
		// a cancelled interaction records nothing; the never-made choice is not history
		if s := p.it.summary(u.theme); err == nil && s != "" {
			u.gap()
			u.commit(s, flowWrap)
		}
		u.dequeue(p)
		u.repaint()
	}
}

// interactionRows renders the active interaction within free rows, plus one naming
// any prompts queued behind it. Caller holds the lock.
func (u *UI) interactionRows(width, free int) (rows []string, caretRow, caretCol int) {
	waiting := len(u.queue) - 1 // prompts queued behind the active one
	maxRows := interactionMaxRows(free, u.inputRows(), waiting > 0)
	rows, caretRow, caretCol = u.act.it.rows(u.theme, width, maxRows)
	if waiting > 0 {
		// sits below the interaction so the caret position is unchanged
		rows = append(rows, u.theme.Dim.Wrap("+"+strconv.Itoa(waiting)+" waiting"))
	}
	return rows, caretRow, caretCol
}

// interactionMaxRows returns an active interaction's cap from free terminal rows,
// given the editor's own cap and whether prompts queue below it. At least one row.
func interactionMaxRows(free, editorCap int, waiting bool) int {
	want := max(free*interactionShareNum/interactionFree, minInteractionRows, editorCap+1)
	if waiting {
		free-- // the queued-prompt row shares these rows
	}
	return max(min(want, free), 1)
}

// cancelInteractions resolves everything pending, so no caller is left blocked
// after the UI goes away. Caller holds the lock.
func (u *UI) cancelInteractions() {
	for _, p := range u.queue {
		p.resolve(ErrCancelled)
	}
	u.queue = nil
	u.act = nil
}
