package tui

import (
	"context"
	"strconv"
	"strings"
)

// runPlain asks an interaction without a live block, answering through the same
// message queue the caller reads.
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
