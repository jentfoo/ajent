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
