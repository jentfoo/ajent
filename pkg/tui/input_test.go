package tui

import (
	"bytes"
	"io"
	"strconv"
	"strings"
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
		{"ctrl_u_kill_line", "\x15", key{typ: keyKillLine}, 1},
		{"ctrl_w", "\x17", key{typ: keyKillWord}, 1},
		{"ctrl_x_picker_copy", "\x18", key{typ: keyPickerCopy}, 1},
		{"ctrl_y_undo", "\x19", key{typ: keyUndo}, 1},
		{"ctrl_l_redraw", "\x0c", key{typ: keyRedraw}, 1},
		{"ctrl_z_suspend", "\x1a", key{typ: keySuspend}, 1},
		{"unbound_control", "\x1c", key{typ: keyIgnore}, 1},
		{"mouse_report_ignored", "\x1b[<64;10;5M", key{typ: keyIgnore}, 11},
		{"device_attributes", "\x1b[?1;2c", key{typ: keyDeviceAttrs}, 7},
		{"arrow_up", "\x1b[A", key{typ: keyUp}, 3},
		{"alt_arrow_up", "\x1b[1;3A", key{typ: keyAltUp}, 6},
		{"shift_arrow_up_moves", "\x1b[1;2A", key{typ: keyUp}, 6},
		{"shift_arrow_right_mode", "\x1b[1;2C", key{typ: keyShiftRight}, 6},
		{"shift_arrow_left_mode", "\x1b[1;2D", key{typ: keyShiftLeft}, 6},
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
		{"apc_reply_ignored", "\x1b_G0;Invalid control range\x1b\\", key{typ: keyIgnore}, 28},
		{"alt_enter", "\x1b\r", key{typ: keyNewline}, 2},
		{"alt_b_word_left", "\x1bb", key{typ: keyWordLeft}, 2},
		{"alt_f_word_right", "\x1bf", key{typ: keyWordRight}, 2},
		{"alt_backspace", "\x1b\x7f", key{typ: keyKillWord}, 2},
		{"unknown_escape_consumes_esc_only", "\x1bZ", key{typ: keyIgnore}, 1},
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
		assert.True(t, k.partial)
		assert.Len(t, []byte(k.text), maxPasteLen-len(pasteEnd)+1)
		assert.Equal(t, len(b)-len(pasteEnd)+1, n)
		// exactly a terminator head's worth stays unconsumed for the overflow scan
		assert.Equal(t, bytes.Repeat([]byte{'a'}, len(pasteEnd)-1), b[n:])
	})
}

func TestPendingPasteStart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"empty", "", false},
		{"lone_escape_keeps_timeout", "\x1b", false},
		{"csi_open_bracket", "\x1b[", true},
		{"partial_two", "\x1b[2", true},
		{"partial_three", "\x1b[20", true},
		{"complete_header", pasteStart, true},
		{"header_with_body", pasteStart + "hi", true},
		{"wrong_digit", "\x1b[0", false},
		{"paste_end_not_start", pasteEnd, false},
		{"diverged_params", "\x1b[21", false},
		{"plain_runes", "abc", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, pendingPasteStart([]byte(tc.input)))
		})
	}
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

	t.Run("newest_cursor_report_wins", func(t *testing.T) {
		pr, pw := io.Pipe()
		r := newInputReader(pr)
		go r.run()

		// more reports than the channel holds, none of them drained: a report is
		// a position, so the oldest must give way rather than the newest be lost
		go func() {
			for row := 1; row <= 6; row++ {
				_, _ = io.WriteString(pw, "\x1b["+strconv.Itoa(row)+";1R")
			}
			_, _ = io.WriteString(pw, "z")
		}()
		assert.Equal(t, key{typ: keyRune, text: "z"}, <-r.keys) // every report emitted

		var last int
		for drained := true; drained; {
			select {
			case last = <-r.reports:
			default:
				drained = false
			}
		}
		assert.Equal(t, 6, last)
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

	t.Run("alt_printable_lands_as_rune", func(t *testing.T) {
		pr, pw := io.Pipe()
		r := newInputReader(pr)
		go r.run()

		// Meta held while typing: the Esc is dropped and the letter still arrives
		_, err := io.WriteString(pw, "\x1bj")
		require.NoError(t, err)
		assert.Equal(t, key{typ: keyRune, text: "j"}, <-r.keys)
		require.NoError(t, pw.Close())
	})

	t.Run("paste_body_keeps_escapes", func(t *testing.T) {
		pr, pw := io.Pipe()
		r := newInputReader(pr)
		go r.run()

		_, err := io.WriteString(pw, "\x1b[200~a\x1bb\x1b[201~")
		require.NoError(t, err)
		assert.Equal(t, key{typ: keyPaste, text: "a\x1bb"}, <-r.keys)
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

	// decodeAll feeds b and returns every key decoded until an incomplete one
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
		{"alt_letter_falls_through", "\x1bzx", []keyType{keyIgnore, keyRune, keyRune}},
		{"dcs_consumed_whole", "\x1bP0;1|abc\x1b\\x", []keyType{keyIgnore, keyRune}},
		{"sos_consumed_whole", "\x1bXhi\x1b\\z", []keyType{keyIgnore, keyRune}},
		{"pm_consumed_whole", "\x1b^note\x1b\\y", []keyType{keyIgnore, keyRune}},
		{"esc_aborts_dcs", "\x1bP\x1b[A", []keyType{keyIgnore, keyUp}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, decodeAll([]byte(tc.input)))
		})
	}

	t.Run("overlong_csi_dropped", func(t *testing.T) {
		// an unterminated CSI (no final byte for > maxControlLen params) must not
		// swallow the Enter behind it: cap and resync let decoding reach it
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
		assert.True(t, enter)
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
		// run keeps the last len(pasteEnd)-1 body bytes re-scannable. A terminator
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

	t.Run("split_body_delivers", func(t *testing.T) {
		pr, pw := io.Pipe()
		r := newInputReader(pr)
		go r.run()

		_, err := io.WriteString(pw, pasteStart+"partia")
		require.NoError(t, err)
		_, err = io.WriteString(pw, "l"+pasteEnd)
		require.NoError(t, err)

		assert.Equal(t, key{typ: keyPaste, text: "partial"}, <-r.keys) // blocks until decoded
		require.NoError(t, pw.Close())
	})

	t.Run("split_header_delivers", func(t *testing.T) {
		// a header fragmented across reads is mid-delivery like any paste body:
		// it holds without arming a timer, then completes whole
		for _, n := range []int{2, 3, 4, 5} {
			pr, pw := io.Pipe()
			r := newInputReader(pr)
			go r.run()

			_, err := io.WriteString(pw, pasteStart[:n])
			require.NoError(t, err)
			_, err = io.WriteString(pw, pasteStart[n:]+"split"+pasteEnd)
			require.NoError(t, err)

			assert.Equal(t, key{typ: keyPaste, text: "split"}, <-r.keys) // blocks until decoded
			require.NoError(t, pw.Close())
		}
	})

	t.Run("broken_prefix_resumes_decoding", func(t *testing.T) {
		pr, pw := io.Pipe()
		r := newInputReader(pr)
		go r.run()

		// the held head completes as an ordinary CSI once a byte breaks the match:
		// no runes leak from either fragment and later keys still arrive
		_, err := io.WriteString(pw, "\x1b[2")
		require.NoError(t, err)
		_, err = io.WriteString(pw, "~z")
		require.NoError(t, err)

		assert.Equal(t, key{typ: keyRune, text: "z"}, <-r.keys)
		require.NoError(t, pw.Close())
	})
}

func TestInputReaderBulkCoalesce(t *testing.T) {
	t.Parallel()

	// bulk fills exactly one read chunk at the coalescing floor: more text than
	// typing puts in one read, and any larger body would race the stall window
	bulk := strings.Repeat("a", bulkRun)

	t.Run("typing_replays_individually", func(t *testing.T) {
		pr, pw := io.Pipe()
		r := newInputReader(pr)
		go r.run()

		_, err := io.WriteString(pw, "hi\r\nthere")
		require.NoError(t, err)
		for _, k := range []key{
			{typ: keyRune, text: "h"}, {typ: keyRune, text: "i"}, {typ: keyEnter},
			{typ: keyNewline}, {typ: keyRune, text: "t"}, {typ: keyRune, text: "h"},
			{typ: keyRune, text: "e"}, {typ: keyRune, text: "r"}, {typ: keyRune, text: "e"},
		} {
			assert.Equal(t, k, <-r.keys)
		}
		require.NoError(t, pw.Close())
	})

	t.Run("eof_flushes_pending", func(t *testing.T) {
		pr, pw := io.Pipe()
		r := newInputReader(pr)
		go r.run()

		_, err := io.WriteString(pw, bulk)
		require.NoError(t, err)
		require.NoError(t, pw.Close())

		assert.Equal(t, key{typ: keyPaste, text: bulk}, <-r.keys) // one whole paste, stall or EOF
		_, ok := <-r.keys                                         // stream end emits no keystroke beyond the flush
		assert.False(t, ok)
	})

	t.Run("interrupt_discards_pending", func(t *testing.T) {
		pr, pw := io.Pipe()
		r := newInputReader(pr)
		go r.run()

		// the abort lands in the same read as the text it discards: no paste precedes it
		_, err := io.WriteString(pw, "hi\x03")
		require.NoError(t, err)
		assert.Equal(t, key{typ: keyInterrupt}, <-r.keys)
		require.NoError(t, pw.Close())
		_, ok := <-r.keys
		assert.False(t, ok)
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

		assert.Equal(t, key{typ: keyEnter}, <-r.keys) // only Enter lands, filler is ignored
	})
}

func TestDropPasteTail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
		done bool
	}{
		{"empty", "", "", false},
		{"terminator_only", pasteEnd, "", true},
		{"terminator_drops_prefix", "x" + pasteEnd + "ab", "ab", true},
		{"short_tail_kept_whole", "ab", "ab", false},
		{"long_tail_keeps_head_window", "abcdefgh", "defgh", false},
		{"split_head_kept", "\x1b[201", "\x1b[201", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, done := dropPasteTail([]byte(tc.in))
			assert.Equal(t, tc.done, done)
			assert.Equal(t, tc.want, string(out))
		})
	}
}

func TestInputReaderPasteOverflow(t *testing.T) {
	t.Parallel()

	t.Run("tail_dropped_to_terminator", func(t *testing.T) {
		pr, pw := io.Pipe()
		r := newInputReader(pr)
		go r.run()

		go func() {
			_, _ = io.WriteString(pw, pasteStart+strings.Repeat("a", maxPasteLen))
			_, _ = io.WriteString(pw, "tail\rmore"+pasteEnd+"zy")
		}()

		capped := <-r.keys
		require.Equal(t, keyPaste, capped.typ)
		assert.True(t, capped.partial)
		assert.Len(t, capped.text, maxPasteLen-len(pasteEnd)+1)

		// the next keys follow the terminator, never the tail's Enter
		assert.Equal(t, key{typ: keyRune, text: "z"}, <-r.keys)
		assert.Equal(t, key{typ: keyRune, text: "y"}, <-r.keys)
	})

	t.Run("split_terminator_head_completes", func(t *testing.T) {
		// the delivery boundary can cut the terminator's head from its tail: the
		// held-back bytes must complete it, or every later keystroke is dropped
		for n := 0; n < len(pasteEnd); n++ {
			pr, pw := io.Pipe()
			r := newInputReader(pr)
			go r.run()

			head := pasteStart + strings.Repeat("a", maxPasteLen-n) + pasteEnd[:n]
			tail := pasteEnd[n:] + "zy"
			go func() {
				_, _ = io.WriteString(pw, head)
				_, _ = io.WriteString(pw, tail)
			}()

			capped := <-r.keys
			require.Equal(t, keyPaste, capped.typ)
			assert.True(t, capped.partial)
			assert.Equal(t, strings.Repeat("a", maxPasteLen-len(pasteEnd)+1), capped.text)

			assert.Equal(t, key{typ: keyRune, text: "z"}, <-r.keys)
			assert.Equal(t, key{typ: keyRune, text: "y"}, <-r.keys)
		}
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
