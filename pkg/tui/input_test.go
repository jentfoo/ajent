package tui

import (
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected key
		n        int
	}{
		{"ascii_rune", "a", key{typ: keyRune, text: "a"}, 1},
		{"multibyte_rune", "é", key{typ: keyRune, text: "é"}, 2},
		{"enter", "\r", key{typ: keyEnter}, 1},
		{"ctrl_j_newline", "\n", key{typ: keyNewline}, 1},
		{"backspace", "\x7f", key{typ: keyBackspace}, 1},
		{"ctrl_c", "\x03", key{typ: keyInterrupt}, 1},
		{"ctrl_d", "\x04", key{typ: keyEOF}, 1},
		{"ctrl_a_home", "\x01", key{typ: keyHome}, 1},
		{"ctrl_e_end", "\x05", key{typ: keyEnd}, 1},
		{"ctrl_k", "\x0b", key{typ: keyKillToEnd}, 1},
		{"ctrl_r_reverse_search", "\x12", key{typ: keyReverseSearch}, 1},
		{"ctrl_u", "\x15", key{typ: keyKillLine}, 1},
		{"ctrl_w", "\x17", key{typ: keyKillWord}, 1},
		{"ctrl_l_redraw", "\x0c", key{typ: keyRedraw}, 1},
		{"ctrl_z_suspend", "\x1a", key{typ: keySuspend}, 1},
		{"unbound_control", "\x1c", key{typ: keyIgnore}, 1},
		{"mouse_report_ignored", "\x1b[<64;10;5M", key{typ: keyIgnore}, 11},
		{"arrow_up", "\x1b[A", key{typ: keyUp}, 3},
		{"alt_arrow_up", "\x1b[1;3A", key{typ: keyAltUp}, 6},
		{"arrow_down", "\x1b[B", key{typ: keyDown}, 3},
		{"arrow_right", "\x1b[C", key{typ: keyRight}, 3},
		{"alt_arrow_right_word", "\x1b[1;3C", key{typ: keyWordRight}, 6},
		{"arrow_left", "\x1b[D", key{typ: keyLeft}, 3},
		{"application_arrow", "\x1bOC", key{typ: keyRight}, 3},
		{"ctrl_arrow_word", "\x1b[1;5C", key{typ: keyWordRight}, 6},
		{"alt_arrow_word", "\x1b[1;3D", key{typ: keyWordLeft}, 6},
		{"home_csi", "\x1b[H", key{typ: keyHome}, 3},
		{"end_csi", "\x1b[F", key{typ: keyEnd}, 3},
		{"home_tilde", "\x1b[1~", key{typ: keyHome}, 4},
		{"end_tilde", "\x1b[4~", key{typ: keyEnd}, 4},
		{"delete", "\x1b[3~", key{typ: keyDelete}, 4},
		{"alt_enter", "\x1b\r", key{typ: keyNewline}, 2},
		{"alt_b_word_left", "\x1bb", key{typ: keyWordLeft}, 2},
		{"alt_f_word_right", "\x1bf", key{typ: keyWordRight}, 2},
		{"alt_backspace", "\x1b\x7f", key{typ: keyKillWord}, 2},
		{"unknown_escape", "\x1bZ", key{typ: keyIgnore}, 2},
		{"unknown_csi", "\x1b[9Z", key{typ: keyIgnore}, 4},
		{"cursor_report", "\x1b[12;40R", key{typ: keyCursorReport, row: 12}, 8},
		{"malformed_report", "\x1b[;R", key{typ: keyIgnore}, 4},
		{"status_report", "\x1b[0n", key{typ: keyStatusReport}, 4},
		{"other_dsr_ignored", "\x1b[3n", key{typ: keyIgnore}, 4},
		{"paste", "\x1b[200~hi\x1b[201~", key{typ: keyPaste, text: "hi"}, 14},
		{"paste_multiline", "\x1b[200~a\nb\x1b[201~", key{typ: keyPaste, text: "a\nb"}, 15},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k, n, ok := decodeKey([]byte(tc.input))
			require.True(t, ok)
			assert.Equal(t, tc.expected, k)
			assert.Equal(t, tc.n, n)
		})
	}
}

func TestDecodeKeyIncomplete(t *testing.T) {
	t.Parallel()

	partials := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"lone_escape", "\x1b"},
		{"csi_start", "\x1b["},
		{"csi_params", "\x1b[1;5"},
		{"application_start", "\x1bO"},
		{"unterminated_paste", "\x1b[200~partial"},
		{"partial_rune", "\xc3"},
	}
	for _, tc := range partials {
		t.Run(tc.name, func(t *testing.T) {
			_, n, ok := decodeKey([]byte(tc.input))
			assert.False(t, ok)
			assert.Zero(t, n)
		})
	}
}

// manualEsc is an escTimer a test drives directly, so the timeout paths need no
// wall clock. It may be called from the inputReader run goroutine while the
// test polls its state, so it guards those fields with a mutex.
type manualEsc struct {
	ch     chan time.Time
	mu     sync.Mutex
	delay  time.Duration // last Reset duration, for assertion
	resets int
}

func newManualEsc() *manualEsc { return &manualEsc{ch: make(chan time.Time)} }

func (m *manualEsc) Reset(d time.Duration) bool {
	m.mu.Lock()
	m.delay = d
	m.resets++
	m.mu.Unlock()
	return true
}

// resetCount returns how many times the timer has been armed.
func (m *manualEsc) resetCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resets
}

func (m *manualEsc) Stop() bool {
	select {
	case <-m.ch:
	default:
	}
	return true
}

func (m *manualEsc) C() <-chan time.Time { return m.ch }

// fire simulates the escape timeout elapsing.
func (m *manualEsc) fire(tb testing.TB) {
	tb.Helper()
	select {
	case m.ch <- time.Now():
	default:
		tb.Fatal("escape timer not armed")
	}
}

// newManualReader returns an inputReader whose escape timer is a manualEsc.
func newManualReader(src io.Reader) (*inputReader, *manualEsc) {
	r := newInputReader(src)
	e := newManualEsc()
	r.newTimer = func() escTimer { return e }
	return r, e
}

func TestEscapeTimeout(t *testing.T) {
	t.Parallel()

	t.Run("lone_escape_fires", func(t *testing.T) {
		pr, pw := io.Pipe()
		r, esc := newManualReader(pr)
		go r.run()

		_, err := io.WriteString(pw, "\x1b")
		require.NoError(t, err)
		require.Eventually(t, func() bool { return esc.resetCount() == 1 }, time.Second, testPoll,
			"the escape byte must arm the timer")

		esc.fire(t)
		assert.Equal(t, key{typ: keyEscape}, <-r.keys)
	})
	t.Run("sequence_within_window_decodes", func(t *testing.T) {
		pr, pw := io.Pipe()
		r, esc := newManualReader(pr)
		go r.run()

		_, err := io.WriteString(pw, "\x1b")
		require.NoError(t, err)
		require.Eventually(t, func() bool { return esc.resetCount() == 1 }, time.Second, testPoll,
			"the escape byte must arm the timer before [A arrives")

		_, err = io.WriteString(pw, "[A")
		require.NoError(t, err)
		assert.Equal(t, key{typ: keyUp}, <-r.keys)
	})
	t.Run("paste_containing_escape", func(t *testing.T) {
		pr, pw := io.Pipe()
		r, _ := newManualReader(pr)
		go r.run()

		_, err := io.WriteString(pw, "\x1b[200~a\x1bb\x1b[201~")
		require.NoError(t, err)
		assert.Equal(t, key{typ: keyPaste, text: "a\x1bb"}, <-r.keys)
	})
	t.Run("timer_never_fires_without_data", func(t *testing.T) {
		pr, pw := io.Pipe()
		r, esc := newManualReader(pr)
		go r.run()

		_, err := io.WriteString(pw, "z")
		require.NoError(t, err)
		assert.Equal(t, key{typ: keyRune, text: "z"}, <-r.keys)
		assert.Zero(t, esc.resetCount(), "no escape byte means no timer arming")

		_, err = io.WriteString(pw, "\x1b[A")
		require.NoError(t, err)
		assert.Equal(t, key{typ: keyUp}, <-r.keys)
	})
}

func TestInputReaderRun(t *testing.T) {
	t.Parallel()

	t.Run("decodes_stream", func(t *testing.T) {
		pr, pw := io.Pipe()
		r := newInputReader(pr)
		go r.run()

		_, err := io.WriteString(pw, "hi\r")
		require.NoError(t, err)

		assert.Equal(t, key{typ: keyRune, text: "h"}, <-r.keys)
		assert.Equal(t, key{typ: keyRune, text: "i"}, <-r.keys)
		assert.Equal(t, key{typ: keyEnter}, <-r.keys)

		require.NoError(t, pw.Close())
		assert.Equal(t, keyEOF, (<-r.keys).typ)
	})
	t.Run("splits_across_reads", func(t *testing.T) {
		pr, pw := io.Pipe()
		r := newInputReader(pr)
		go r.run()

		go func() {
			_, _ = io.WriteString(pw, "\x1b[1")
			_, _ = io.WriteString(pw, ";5C")
			_ = pw.Close()
		}()
		assert.Equal(t, key{typ: keyWordRight}, <-r.keys)
	})
	t.Run("reports_go_to_separate_channel", func(t *testing.T) {
		pr, pw := io.Pipe()
		r := newInputReader(pr)
		go r.run()

		_, err := io.WriteString(pw, "\x1b[7;3Rz")
		require.NoError(t, err)

		select {
		case row := <-r.reports:
			assert.Equal(t, 7, row)
		case <-time.After(time.Second):
			t.Fatal("no cursor report")
		}
		assert.Equal(t, key{typ: keyRune, text: "z"}, <-r.keys)
		require.NoError(t, pw.Close())
	})
	t.Run("ignored_keys_dropped", func(t *testing.T) {
		pr, pw := io.Pipe()
		r := newInputReader(pr)
		go r.run()

		_, err := io.WriteString(pw, "\x1cx")
		require.NoError(t, err)
		assert.Equal(t, key{typ: keyRune, text: "x"}, <-r.keys)
		require.NoError(t, pw.Close())
	})
}
