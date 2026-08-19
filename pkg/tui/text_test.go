package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDisplayWidth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected int
	}{
		{"empty", "", 0},
		{"ascii", "hello", 5},
		{"styled_ignored", "\x1b[1mhello\x1b[0m", 5},
		{"wide_runes", "ＡＢＣ", 6},
		{"emoji_cluster", "👍", 2},
		{"osc_ignored", "\x1b]0;title\x07abc", 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, displayWidth(tc.input))
		})
	}
}

func TestTruncateDisplay(t *testing.T) {
	t.Parallel()

	t.Run("under_limit_unchanged", func(t *testing.T) {
		assert.Equal(t, "hello", truncateDisplay("hello", 10))
	})
	t.Run("zero_width_empty", func(t *testing.T) {
		assert.Empty(t, truncateDisplay("hello", 0))
	})
	t.Run("cuts_to_width", func(t *testing.T) {
		assert.Equal(t, "hel", truncateDisplay("hello", 3))
	})
	t.Run("keeps_escapes_and_resets", func(t *testing.T) {
		assert.Equal(t, "\x1b[1mhel\x1b[0m", truncateDisplay("\x1b[1mhello\x1b[0m", 3))
	})
	t.Run("does_not_split_wide_rune", func(t *testing.T) {
		assert.Equal(t, "Ａ", truncateDisplay("ＡＢＣ", 3))
	})
}

func TestGraphemesOf(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, in string
		want     []string
	}{
		{name: "empty", in: ""},
		{name: "plain_runes", in: "ab", want: []string{"a", "b"}},
		{name: "combining_mark_kept_together", in: "é", want: []string{"é"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, graphemesOf(tc.in))
		})
	}
}

func TestSplitANSI(t *testing.T) {
	t.Parallel()

	t.Run("plain_text", func(t *testing.T) {
		assert.Equal(t, []ansiSegment{{text: "abc"}}, splitANSI("abc"))
	})
	t.Run("csi_sequence", func(t *testing.T) {
		expected := []ansiSegment{
			{text: "\x1b[1m", escape: true},
			{text: "a"},
			{text: "\x1b[0m", escape: true},
		}
		assert.Equal(t, expected, splitANSI("\x1b[1ma\x1b[0m"))
	})
	t.Run("osc_string_terminator", func(t *testing.T) {
		expected := []ansiSegment{
			{text: "\x1b]8;;http://x\x1b\\", escape: true},
			{text: "link"},
		}
		assert.Equal(t, expected, splitANSI("\x1b]8;;http://x\x1b\\link"))
	})
	t.Run("two_byte_escape", func(t *testing.T) {
		expected := []ansiSegment{{text: "\x1b7", escape: true}, {text: "x"}}
		assert.Equal(t, expected, splitANSI("\x1b7x"))
	})
	t.Run("truncated_escape", func(t *testing.T) {
		assert.Equal(t, []ansiSegment{{text: "\x1b[1", escape: true}}, splitANSI("\x1b[1"))
	})
}

func TestOneLine(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, in, want string
	}{
		{name: "already_plain", in: "plain", want: "plain"},
		{name: "newline_folds_to_space", in: "a\nb", want: "a b"},
		// CRLF folds to one blank, not two
		{name: "crlf_single_blank", in: "a\r\nb", want: "a b"},
		{name: "tabs_and_vtab_fold", in: "a\tb\vc", want: "a b c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, oneLine(tc.in))
		})
	}
}

func TestSanitizeRow(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, in, want string }{
		{name: "plain_unchanged", in: "plain text", want: "plain text"},
		{name: "sgr_kept", in: "\x1b[1mbold\x1b[0m tail", want: "\x1b[1mbold\x1b[0m tail"},
		{name: "sgr_colon_params_kept", in: "\x1b[38:5:196mred\x1b[0m", want: "\x1b[38:5:196mred\x1b[0m"},
		{name: "empty_sgr_kept", in: "\x1b[m", want: "\x1b[m"},
		{name: "cursor_motion_dropped", in: "sub-1 \x1b[2B boom", want: "sub-1  boom"},
		{name: "screen_escapes_dropped", in: "a\x1b[2Jb\x1b[5Ac", want: "abc"},
		{name: "private_mode_dropped", in: "\x1b[?25lx", want: "x"},
		{name: "private_sgr_params_dropped", in: "\x1b[>4;2mx", want: "x"},
		{name: "vpa_cha_dropped", in: "a\x1b[3db\x1b[4Gc", want: "abc"},
		{name: "osc_bel_dropped", in: "\x1b]0;title\x07x", want: "x"},
		{name: "osc_st_dropped", in: "\x1b]8;;url\x1b\\link", want: "link"},
		{name: "decsc_decrc_dropped", in: "\x1b7\x1b8x", want: "x"},
		{name: "ind_nel_ri_dropped", in: "\x1bD\x1bE\x1bMx", want: "x"},
		// splitANSI models two-byte escapes, so a charset designator's final
		// byte survives as text; harmless, no escape reaches the terminal
		{name: "charset_dropped", in: "\x1b(Bx", want: "Bx"},
		{name: "truncated_csi_dropped", in: "sub \x1b[12", want: "sub "},
		{name: "truncated_osc_dropped", in: "sub \x1b]0;tit", want: "sub "},
		{name: "lone_esc_dropped", in: "sub \x1b", want: "sub "},
		{name: "crlf_folds_one", in: "a\r\nb", want: "a b"},
		{name: "tab_folds", in: "a\tb", want: "a b"},
		{name: "c0_dropped", in: "a\x00\x01\x02b", want: "ab"},
		{name: "del_dropped", in: "a\x7fb", want: "ab"},
		{name: "c1_dropped", in: "a\u009bb\u0090c", want: "abc"},
		{name: "wide_kept", in: "\u4e16\u754c \x1b[2B", want: "\u4e16\u754c "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := sanitizeRow(tc.in)
			assert.Equal(t, tc.want, out)
			assert.Equal(t, out, sanitizeRow(out), "idempotent")
		})
	}
}

func TestIsSGR(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{name: "empty_params", in: "\x1b[m", want: true},
		{name: "reset", in: "\x1b[0m", want: true},
		{name: "semicolons", in: "\x1b[1;32m", want: true},
		{name: "colons", in: "\x1b[38:2:0;0;255m", want: true},
		{name: "private_marker", in: "\x1b[>4;2m", want: false},
		{name: "question_marker", in: "\x1b[?25m", want: false},
		{name: "cursor_up", in: "\x1b[2A", want: false},
		{name: "erase_display", in: "\x1b[2J", want: false},
		{name: "truncated", in: "\x1b[1", want: false},
		{name: "not_csi", in: "\x1b7", want: false},
		{name: "osc", in: "\x1b]0;t\x07", want: false},
		{name: "intermediate_space", in: "\x1b[0 m", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isSGR(tc.in))
		})
	}
}

// FuzzSanitizeRow checks the strong property: a sanitized row truncated to
// the live width never moves the cursor to another row, whatever the input
// contained. That one property covers every stranding mechanism at once.
func FuzzSanitizeRow(f *testing.F) {
	f.Add("plain text")
	f.Add("sub \x1b[2B down")
	f.Add("\x1b[1mbold\x1b[0m")
	f.Add("tab\there\r\nnow")
	f.Add("\x1b]0;title\x07osc")
	f.Add("trunc\x1b[12")
	f.Add("\u009b2Bc1\u0090")
	f.Add("\x1bD\x1bE\x1bM\x7f\x00")
	f.Fuzz(func(t *testing.T, s string) {
		san := sanitizeRow(s)
		assert.Equal(t, san, sanitizeRow(san), "idempotent")
		for w := 1; w <= 24; w++ {
			v := newVT(w, 8)
			v.WriteString("x\r\ny\r\nz") // park mid-screen
			row := v.row
			v.WriteString(truncateDisplay(san, w-1))
			assert.Equal(t, row, v.row, "width %d: the row stays on one terminal row", w)
		}
	})
}

func TestPaintCaret(t *testing.T) {
	t.Parallel()

	// the caret reverses a cell rather than adding one, so widths never move
	t.Run("reverses_in_place_without_growing_width", func(t *testing.T) {
		assert.Equal(t, 3, displayWidth(paintCaret("abc", 1, 10)))
		assert.Contains(t, paintCaret("abc", 1, 10), caretReverse)
	})

	t.Run("past_the_text_pads", func(t *testing.T) {
		out := paintCaret("ab", 4, 10)
		assert.Equal(t, 5, displayWidth(out))
		assert.Contains(t, out, caretReverse)
	})
	t.Run("never_past_the_bound", func(t *testing.T) {
		assert.LessOrEqual(t, displayWidth(paintCaret("ab", 9, 5)), 5)
	})
	t.Run("keeps_the_row_readable", func(t *testing.T) {
		out := paintCaret("\x1b[2mdim\x1b[0m", 0, 10)
		assert.Equal(t, 3, displayWidth(out))
		var b strings.Builder
		for _, seg := range splitANSI(out) {
			if !seg.escape {
				b.WriteString(seg.text)
			}
		}
		text := b.String()
		// styling is re-cut around the caret, text is not
		assert.Equal(t, "dim", text)
	})
	t.Run("empty_row", func(t *testing.T) {
		assert.Equal(t, 1, displayWidth(paintCaret("", 0, 10)))
	})
}
