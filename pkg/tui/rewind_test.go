package tui

import (
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rewindSetup returns a UI with the double-Esc gesture enabled and idle, plus
// its input writer. The window is set wide so two lone Esc presses are always
// within it; tests that need an elapsing window use a short one explicitly.
func rewindSetup(t *testing.T) (*UI, io.Writer) {
	t.Helper()
	v := newVT(40, 10)
	pr, pw := io.Pipe()
	u := newTestUI(t, v, pr)

	// the UI built by newTestUI never ran New, so wire rewind fields directly
	// before any key is decoded (the reader holds a lone Esc for escTimeout).
	u.mu.Lock()
	u.idle = true
	u.doubleEscWindow = time.Hour
	u.onRewind = func() {}
	u.mu.Unlock()
	return u, pw
}

// setOnRewind swaps the rewind callback under lock.
func (u *UI) setOnRewind(cb func()) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.onRewind = cb
}

// editorValue reads the editor buffer under lock.
func (u *UI) editorValue() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.editor.Value()
}

// editorPos reads the editor caret cell index under lock.
func (u *UI) editorPos() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.editor.pos
}

// feedEscape writes one lone escape byte and waits until it has been decoded as
// keyEscape (the input reader holds a bare Esc for escTimeout before reporting).
func feedEscape(t *testing.T, w io.Writer) {
	t.Helper()
	_, err := io.WriteString(w, "\x1b")
	require.NoError(t, err)
}

func TestDoubleEsc(t *testing.T) {
	t.Parallel()

	// two lone Esc presses while idle and within the window invoke OnRewind.
	t.Run("rewinds_while_idle", func(t *testing.T) {
		u, pw := rewindSetup(t)
		rewound := make(chan struct{})
		u.setOnRewind(func() { close(rewound) })

		feedEscape(t, pw)
		// wait for the first lone Esc to be decoded and arm the rewind gesture
		require.Eventually(t, func() bool { return u.isEscPending() }, time.Second, testPoll)
		select {
		case c := <-u.Controls():
			t.Fatalf("first idle Esc must not emit a control immediately, got %v", c)
		default:
		}

		// escPending above already proves the first lone Esc was fully decoded and
		// armed the gesture; a fresh press now lands as an independent keyEscape.
		feedEscape(t, pw) // second Esc -> rewind
		select {
		case <-rewound:
		case <-time.After(time.Second):
			t.Fatal("double-Esc did not invoke OnRewind")
		}
	})

	// after the window elapses a lone Esc emits its deferred single control.
	t.Run("single_esc_emits_control_after_window_elapses", func(t *testing.T) {
		v := newVT(40, 10)
		pr, pw := io.Pipe()
		u := newTestUI(t, v, pr)

		// short window so the lone-Esc flush fires quickly after one press. A callback
		// is wired (so rewind would be possible) but only one Esc arrives, and the
		// deferred single control must still emit once the window elapses.
		rewound := make(chan struct{})
		u.mu.Lock()
		u.idle = true
		u.doubleEscWindow = 30 * time.Millisecond
		u.onRewind = func() { close(rewound) }
		u.mu.Unlock()

		feedEscape(t, pw)
		assert.Equal(t, ControlEscape, <-u.Controls())
		select {
		case <-rewound:
			t.Fatal("a single Esc must not rewind")
		default:
		}
	})

	// mid-turn (idle false) a lone Esc keeps its interrupt role immediately.
	t.Run("single_esc_not_idle_emits_immediately", func(t *testing.T) {
		v := newVT(40, 10)
		pr, pw := io.Pipe()
		u := newTestUI(t, v, pr)

		// mid-turn: idle is false, so a lone Esc keeps its interrupt role
		feedEscape(t, pw)
		assert.Equal(t, ControlEscape, <-u.Controls())
	})

	// with no OnRewind wired, an idle lone Esc still emits a control immediately.
	t.Run("no_callback_keeps_plain_single_esc", func(t *testing.T) {
		v := newVT(40, 10)
		pr, pw := io.Pipe()
		u := newTestUI(t, v, pr)

		// idle but no OnRewind wired: Esc still emits a control immediately
		u.mu.Lock()
		u.idle = true
		u.onRewind = nil
		u.mu.Unlock()

		feedEscape(t, pw)
		assert.Equal(t, ControlEscape, <-u.Controls())
	})

	// Esc with a non-empty buffer clears it rather than rewinding.
	t.Run("esc_with_text_clears_buffer_not_rewinds", func(t *testing.T) {
		v := newVT(40, 10)
		pr, pw := io.Pipe()
		u := newTestUI(t, v, pr)

		rewound := make(chan struct{})
		u.mu.Lock()
		u.idle = true
		u.doubleEscWindow = time.Hour
		u.onRewind = func() { close(rewound) }
		u.mu.Unlock()

		_, err := io.WriteString(pw, "typed")
		require.NoError(t, err)
		require.Eventually(t, func() bool { return u.editorValue() == "typed" },
			time.Second, testPoll)

		feedEscape(t, pw) // clears the buffer (after escTimeout decodes it)
		require.Eventually(t, func() bool { return u.editorValue() == "" },
			time.Second, testPoll)

		select {
		case <-rewound:
			t.Fatal("Esc with a non-empty buffer must clear it, not rewind")
		default:
		}
	})

	// starting a turn before the second Esc cancels the pending gesture.
	t.Run("set_idle_false_cancels_pending_rewind", func(t *testing.T) {
		v := newVT(40, 10)
		pr, pw := io.Pipe()
		u := newTestUI(t, v, pr)

		rewound := make(chan struct{})
		u.mu.Lock()
		u.idle = true
		u.doubleEscWindow = time.Hour
		u.onRewind = func() { close(rewound) }
		u.mu.Unlock()

		feedEscape(t, pw)
		// a turn starts before the second Esc: the pending gesture is cancelled
		require.Eventually(t, func() bool { return u.isEscPending() }, time.Second, testPoll)

		u.SetIdle(false)
		assert.False(t, u.isEscPending())

		feedEscape(t, pw) // now mid-turn: plain interrupt control
		assert.Equal(t, ControlEscape, <-u.Controls())
	})
}

// isEscPending reads the rewind arm state under lock.
func (u *UI) isEscPending() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.escPending
}
