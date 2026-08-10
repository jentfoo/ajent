package tui

import (
	"context"
	"strconv"
	"strings"
)

// runPlain asks an interaction without a live block. Plain mode has no raw keys
// and readLines already owns stdin, so the prompt is written to history and the
// answer is taken from the message queue.
//
// Reading the same queue the caller does is what makes this race free: a plain
// mode prompt is only ever opened from the message loop, so nothing else is
// reading while it waits.
func (u *UI) runPlain(ctx context.Context, it interactor) error {
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return ErrCancelled
	}
	u.commit(plainPrompt(u.theme, it), flowWrap)
	u.mu.Unlock()

	select {
	case line, ok := <-u.msgs:
		if !ok {
			return ErrCancelled
		}
		return applyPlainAnswer(it, line)
	case <-ctx.Done():
		return ErrCancelled
	case <-u.done:
		return ErrCancelled
	}
}

// plainPrompt renders an interaction as static numbered text.
func plainPrompt(t Theme, it interactor) string {
	rows, _, _ := it.rows(t, defaultWidth, 100)
	return strings.Join(rows, "\n")
}

// applyPlainAnswer feeds a typed line to an interaction. A number selects by
// position, anything else is typed in.
func applyPlainAnswer(it interactor, line string) error {
	line = strings.TrimSpace(line)
	switch s := it.(type) {
	case *selectState:
		n, err := strconv.Atoi(line)
		if err != nil || n < 1 || n > len(s.options) {
			return ErrCancelled
		}
		s.cursor = n - 1
		return nil
	case *inputState:
		s.value = line
		return nil
	case *pickState:
		s.filter = line
		s.refilter()
		if len(s.matches) == 0 {
			return ErrCancelled
		}
		s.chosen = s.matches[0]
		return nil
	default:
		return ErrNoUI
	}
}
