package tui

import (
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

	assert.Nil(t, graphemesOf(""))
	assert.Equal(t, []string{"a", "b"}, graphemesOf("ab"))
	assert.Equal(t, []string{"é"}, graphemesOf("é"))
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
