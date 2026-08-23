package tui

import (
	"bytes"
	"io"
	"strings"
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
		{"device_attributes", "\x1b[?1;2c", key{typ: keyDeviceAttrs}, 7},
		{"arrow_up", "\x1b[A", key{typ: keyUp}, 3},
		{"alt_arrow_up", "\x1b[1;3A", key{typ: keyAltUp}, 6},
		{"shift_arrow_moves", "\x1b[1;2C", key{typ: keyRight}, 6},
		{"ctrl_shift_arrow_word", "\x1b[1;6C", key{typ: keyWordRight}, 6},
		{"alt_ctrl_arrow_word", "\x1b[1;7D", key{typ: keyWordLeft}, 6},
		{"subparam_modifier", "\x1b[1;5:3C", key{typ: keyWordRight}, 8},
		{"arrow_down", "\x1b[B", key{typ: keyDown}, 3},
		{"arrow_right", "\x1b[C", key{typ: keyRight}, 3},
		{"alt_arrow_right_word", "\x1b[1;3C", key{typ: keyWordRight}, 6},
		{"arrow_left", "\x1b[D", key{typ: keyLeft}, 3},
		{"application_arrow", "\x1bOC", key{typ: keyRight}, 3},
		{"ss3_home", "\x1bOH", key{typ: keyHome}, 3},
		{"ss3_end", "\x1bOF", key{typ: keyEnd}, 3},
		{"ss3_keypad_enter", "\x1bOM", key{typ: keyEnter}, 3},
		{"ss3_f1_ignored", "\x1bOP", key{typ: keyIgnore}, 3},
		{"ss3_keypad_digit_ignored", "\x1bOs", key{typ: keyIgnore}, 3},
		{"ctrl_arrow_word", "\x1b[1;5C", key{typ: keyWordRight}, 6},
		{"alt_arrow_word", "\x1b[1;3D", key{typ: keyWordLeft}, 6},
		{"home_csi", "\x1b[H", key{typ: keyHome}, 3},
		{"end_csi", "\x1b[F", key{typ: keyEnd}, 3},
		{"home_tilde", "\x1b[1~", key{typ: keyHome}, 4},
		{"end_tilde", "\x1b[4~", key{typ: keyEnd}, 4},
		{"delete", "\x1b[3~", key{typ: keyDelete}, 4},
		{"shift_tab_modified", "\x1b[1;2Z", key{typ: keyBackTab}, 6},
		{"bare_csi_r_ignored", "\x1b[R", key{typ: keyIgnore}, 3},
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

	t.Run("unterminated_paste_at_cap", func(t *testing.T) {
		b := append([]byte(pasteStart), bytes.Repeat([]byte{'a'}, maxPasteLen)...)
		k, n, ok := decodeKey(b)
		assert.True(t, ok)
		assert.Equal(t, keyPaste, k.typ)
		assert.Len(t, []byte(k.text), maxPasteLen)
		assert.Equal(t, len(b), n)
	})
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
		_, ok := <-r.keys // stream end emits no editing keystroke, just a channel close
		assert.False(t, ok)
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
	t.Run("color_report_separate_channel", func(t *testing.T) {
		pr, pw := io.Pipe()
		r := newInputReader(pr)
		go r.run()

		_, err := io.WriteString(pw, "\x1b]11;rgb:ffff/ffff/ffff\x07z")
		require.NoError(t, err)

		select {
		case spec := <-r.colors:
			assert.Equal(t, "rgb:ffff/ffff/ffff", spec)
		case <-time.After(time.Second):
			t.Fatal("no color report")
		}
		// the whole reply is consumed: its body never reaches the editor
		assert.Equal(t, key{typ: keyRune, text: "z"}, <-r.keys)
		require.NoError(t, pw.Close())
	})
	t.Run("device_attrs_separate_channel", func(t *testing.T) {
		pr, pw := io.Pipe()
		r := newInputReader(pr)
		go r.run()

		_, err := io.WriteString(pw, "\x1b[?62;c z")
		require.NoError(t, err)

		select {
		case <-r.attrs:
		case <-time.After(time.Second):
			t.Fatal("no device attributes")
		}
		assert.Equal(t, key{typ: keyRune, text: " "}, <-r.keys)
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

func TestCSIModifier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		params string
		want   int
	}{
		{"no_modifier", "", 0},
		{"plain_cursor_row", "1", 0}, // single field: no modifier column
		{"ctrl_only", "1;5", modCtrl},
		{"shift_only", "1;2", modShift},
		{"alt_ctrl_subparam", "1;7:3", modAlt | modCtrl},
		{"extra_field_ignored", "1;5;9", modCtrl},
		{"bare_modifier_field", ";3", modAlt}, // params not starting with a row
		{"garbage_value", "1;x", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, csiModifier(tc.params))
		})
	}
}

func TestDecodeKeyResync(t *testing.T) {
	t.Parallel()

	// decodeAll feeds b and returns every key decoded until an incomplete one.
	decodeAll := func(b []byte) (out []keyType) {
		for len(b) > 0 {
			k, n, ok := decodeKey(b)
			if !ok {
				break
			}
			out = append(out, k.typ)
			b = b[n:]
		}
		return out
	}

	tests := []struct {
		name  string
		input string
		want  []keyType
	}{
		{"esc_aborts_partial_csi", "\x1b[1;5\x1b[A", []keyType{keyIgnore, keyUp}},
		{"esc_aborts_partial_ss3", "\x1bO\x1b[C", []keyType{keyIgnore, keyRight}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, decodeAll([]byte(tc.input)))
		})
	}

	t.Run("overlong_csi_dropped", func(t *testing.T) {
		// an unterminated CSI (no final byte for > maxControlLen params) must not
		// swallow the Enter behind it: cap and resync let decoding reach it.
		b := append([]byte("\x1b["), bytes.Repeat([]byte{0x01}, maxControlLen+64)...)
		b = append(b, '\r')
		var enter bool
		for len(b) > 0 {
			k, n, ok := decodeKey(b)
			if !ok {
				break // would stall forever: the cap failed to resync
			}
			if k.typ == keyEnter {
				enter = true
			}
			b = b[n:]
		}
		assert.True(t, enter, "an unterminated CSI must not swallow Enter")
	})
	t.Run("short_csi_still_waits", func(t *testing.T) {
		// the cap must not turn a split read into a drop.
		_, _, ok := decodeKey([]byte("\x1b[1;5"))
		assert.False(t, ok)
	})
}

func TestDecodeKeyPasteFrom(t *testing.T) {
	t.Parallel()

	t.Run("hint_finds_same_terminator", func(t *testing.T) {
		b := []byte(pasteStart + "hello" + pasteEnd)
		k0, n0, ok0 := decodeKey(b)
		require.True(t, ok0)
		k1, n1, ok1 := decodeKeyFrom(b, 3) // any non-zero hint
		require.True(t, ok1)
		assert.Equal(t, k0, k1)
		assert.Equal(t, n0, n1)
	})
	t.Run("terminator_straddles_boundary", func(t *testing.T) {
		// run keeps the last len(pasteEnd)-1 body bytes re-scannable; a terminator
		// split across reads must still be found from that resume offset.
		first := []byte(pasteStart + "abc" + pasteEnd[:len(pasteEnd)-1]) // partial terminator
		scanned := max(len(first)-len(pasteStart)-len(pasteEnd)+1, 0)
		buf := append(append([]byte{}, first...), pasteEnd[len(pasteEnd)-1:]+"\r"...)
		k, _, ok := decodeKeyFrom(buf, scanned)
		require.True(t, ok)
		assert.Equal(t, key{typ: keyPaste, text: "abc"}, k)
	})
	t.Run("out_of_range_hint_does_not_panic", func(t *testing.T) {
		b := []byte(pasteStart + "hi" + pasteEnd)
		_, _, ok := decodeKeyFrom(b, 1000) // hint beyond the body: clamped, no panic
		assert.False(t, ok)
	})
}

func TestInputReaderPasteStall(t *testing.T) {
	t.Parallel()

	t.Run("stalled_paste_never_arms_timer", func(t *testing.T) {
		pr, pw := io.Pipe()
		r, esc := newManualReader(pr)
		go r.run()

		_, err := io.WriteString(pw, pasteStart+"partia")
		require.NoError(t, err)
		_, err = io.WriteString(pw, "l"+pasteEnd)
		require.NoError(t, err)

		assert.Equal(t, key{typ: keyPaste, text: "partial"}, <-r.keys) // blocks until decoded
		assert.Zero(t, esc.resetCount(), "a confirmed paste must never arm the escape timer")
	})
}

func TestInputReaderStalledSequenceDropped(t *testing.T) {
	t.Parallel()

	t.Run("truncated_csi_drops_no_runes", func(t *testing.T) {
		pr, pw := io.Pipe()
		r, esc := newManualReader(pr)
		go r.run()

		_, err := io.WriteString(pw, "\x1b[1;5")
		require.NoError(t, err)
		require.Eventually(t, func() bool { return esc.resetCount() == 1 }, time.Second, testPoll,
			"a truncated CSI must arm the escape timer")

		esc.fire(t) // the stall elapses: nothing may be emitted for it

		_, err = io.WriteString(pw, "z")
		require.NoError(t, err)
		assert.Equal(t, key{typ: keyRune, text: "z"}, <-r.keys,
			"no spurious Escape or [ 1 ; 5 runes may leak before the next keystroke")
	})
}

func TestInputReaderBounded(t *testing.T) {
	t.Parallel()

	t.Run("keystroke_behind_wedged_sequence", func(t *testing.T) {
		pr, pw := io.Pipe()
		r := newInputReader(pr)
		go r.run()

		// an unterminated CSI must not eat the Enter behind it: cap and resync.
		filler := strings.Repeat("\x1c", maxControlLen+64) //  is an unbound control byte
		_, err := io.WriteString(pw, "\x1b["+filler+"\r")
		require.NoError(t, err)

		assert.Equal(t, key{typ: keyEnter}, <-r.keys) // only Enter lands; filler is ignored
	})
}

func TestDecodeOSC(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected key
		n        int
		ok       bool
	}{
		{"bel_terminated", "\x1b]11;rgb:1234/5678/9abc\x07",
			key{typ: keyColorReport, text: "rgb:1234/5678/9abc"}, 24, true},
		{"st_terminated", "\x1b]11;#ffffff\x1b\\",
			key{typ: keyColorReport, text: "#ffffff"}, 14, true},
		{"other_osc_ignored", "\x1b]0;window title\x07", key{typ: keyIgnore}, 17, true},
		{"incomplete_waits", "\x1b]11;rgb:12", key{}, 0, false},
		{"trailing_esc_waits", "\x1b]11;rgb:12\x1b", key{}, 0, false},
		{"esc_aborts", "\x1b]11;\x1b[A", key{typ: keyIgnore}, 5, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k, n, ok := decodeKeyFrom([]byte(tc.input), 0)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.expected, k)
			assert.Equal(t, tc.n, n)
		})
	}
	t.Run("over_long_resyncs", func(t *testing.T) {
		b := []byte("\x1b]11;" + strings.Repeat("x", maxControlLen) + "\x07")
		k, n, ok := decodeKeyFrom(b, 0)
		assert.True(t, ok)
		assert.Equal(t, key{typ: keyIgnore}, k)
		assert.Equal(t, maxControlLen, n)
	})
}
