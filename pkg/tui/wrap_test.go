package tui

import (
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWrapLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		width    int
		expected []string
	}{
		{"fits", "hello", 10, []string{"hello"}},
		{"exact_width", "0123456789", 10, []string{"0123456789"}},
		{"breaks_on_space", "hello world", 8, []string{"hello", "world"}},
		{"drops_the_break_space", "aaa bbb ccc", 7, []string{"aaa bbb", "ccc"}},
		{"hard_break_without_space", "aaaaaaaaaaaa", 5, []string{"aaaaa", "aaaaa", "aa"}},
		{"invalid_width", "hello", 0, []string{"hello"}},
		{"empty", "", 10, []string{""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, wrapLine(tc.input, tc.width))
		})
	}

	t.Run("no_row_exceeds_width", func(t *testing.T) {
		long := "the retry helper loops a fixed number of times with no backoff at all"
		for _, w := range []int{5, 12, 40, 68} {
			for _, row := range wrapLine(long, w) {
				assert.LessOrEqual(t, displayWidth(row), w, "width %d", w)
			}
		}
	})
	t.Run("wide_runes_never_stall", func(t *testing.T) {
		rows := wrapLine("ＡＢＣ", 1)
		require.Len(t, rows, 3, "a rune wider than the row still advances")
	})
	t.Run("zero_width_never_stalls", func(t *testing.T) {
		// a lone combining mark is its own cluster measuring nothing; the loop has
		// to advance on cell count, not on columns consumed
		rows := wrapLine("\u0301\u0301\u0301abc", 1)
		require.NotEmpty(t, rows)
		assert.Equal(t, "\u0301\u0301\u0301abc", strings.Join(rows, ""))
	})
	t.Run("graphemes_never_split", func(t *testing.T) {
		// a family emoji is one cluster of many runes, splitting it would corrupt it
		line := "ok 👨‍👩‍👧‍👦 done"
		for _, w := range []int{3, 4, 5, 9} {
			for _, row := range wrapLine(line, w) {
				assert.LessOrEqual(t, displayWidth(row), w, "width %d", w)
				if strings.Contains(row, "👨") {
					assert.Contains(t, row, "👦", "the cluster stayed whole at width %d", w)
				}
			}
		}
	})
	t.Run("combining_marks_measured_as_one", func(t *testing.T) {
		rows := wrapLine("éé ab", 2) // precomposed then combining form
		assert.Equal(t, []string{"éé", "ab"}, rows)
	})
}

func TestWrapLineIndent(t *testing.T) {
	t.Parallel()

	t.Run("bullet_hangs_under_text", func(t *testing.T) {
		rows := wrapLine("• alpha beta gamma", 10)
		assert.Equal(t, []string{"• alpha", "  beta", "  gamma"}, rows)
	})
	t.Run("ordered_marker_hangs", func(t *testing.T) {
		rows := wrapLine("12. alpha beta", 10)
		assert.Equal(t, []string{"12. alpha", "    beta"}, rows)
	})
	t.Run("quote_hangs", func(t *testing.T) {
		rows := wrapLine(quotePrefix+"alpha beta gamma", 12)
		assert.Equal(t, []string{quotePrefix + "alpha beta", "  gamma"}, rows)
	})
	t.Run("existing_indent_preserved", func(t *testing.T) {
		rows := wrapLine("    alpha beta", 10)
		assert.Equal(t, []string{"    alpha", "    beta"}, rows)
	})
	t.Run("hang_capped_for_narrow_width", func(t *testing.T) {
		rows := wrapLine("          alpha beta", 8)
		for _, r := range rows {
			assert.LessOrEqual(t, displayWidth(r), 8)
		}
	})
}

func TestWrapLineStyled(t *testing.T) {
	t.Parallel()

	t.Run("style_reopened_on_each_row", func(t *testing.T) {
		style := Style{open: sgr(attrDim)}
		rows := wrapLine(style.Wrap("hello world"), 8)
		require.Len(t, rows, 2)
		assert.Equal(t, "\x1b[2mhello\x1b[0m", rows[0])
		assert.Equal(t, "\x1b[2mworld\x1b[0m", rows[1], "continuation carries the style, prose has no hang")
	})
	t.Run("escapes_not_counted_as_width", func(t *testing.T) {
		styled := Style{open: sgr(attrBold)}.Wrap("0123456789")
		assert.Equal(t, []string{styled}, wrapLine(styled, 10))
	})
	t.Run("mixed_styles_preserved", func(t *testing.T) {
		bold := Style{open: sgr(attrBold)}
		line := "plain " + bold.Wrap("strong") + " tail"
		rows := wrapLine(line, 12)
		var joined strings.Builder
		for _, r := range rows {
			joined.WriteString(strutil.StripANSI(r))
		}
		assert.Contains(t, joined.String(), "strong")
		assert.Contains(t, rows[0]+rows[1], bold.Open())
	})
}

func TestHardWrap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		width    int
		expected []string
	}{
		{"fits", "hello", 10, []string{"hello"}},
		{"exact_width", "0123456789", 10, []string{"0123456789"}},
		{"breaks_mid_word", "hello world", 8, []string{"hello wo", "rld"}},
		{"drops_boundary_space", "aaaaa bbbbb", 5, []string{"aaaaa", "bbbbb"}},
		{"keeps_inner_spaces", "aa bb cc dd", 6, []string{"aa bb ", "cc dd"}},
		{"no_hanging_indent", "• alpha beta gamma", 10, []string{"• alpha be", "ta gamma"}},
		{"invalid_width", "hello", 0, []string{"hello"}},
		{"empty", "", 10, []string{""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, hardWrap(tc.input, tc.width))
		})
	}

	t.Run("no_row_exceeds_width", func(t *testing.T) {
		long := "the retry helper loops a fixed number of times with no backoff at all"
		for _, w := range []int{5, 12, 40, 68} {
			for _, row := range hardWrap(long, w) {
				assert.LessOrEqual(t, displayWidth(row), w, "width %d", w)
			}
		}
	})
	t.Run("wide_runes_never_stall", func(t *testing.T) {
		assert.Len(t, hardWrap("ＡＢＣ", 1), 3, "a rune wider than the row still advances")
	})
	t.Run("zero_width_never_stalls", func(t *testing.T) {
		rows := hardWrap("́́́abc", 1)
		require.NotEmpty(t, rows)
		assert.Equal(t, "́́́abc", strings.Join(rows, ""))
	})
	t.Run("graphemes_never_split", func(t *testing.T) {
		line := "ok 👨‍👩‍👧‍👦 done"
		for _, w := range []int{3, 4, 5, 9} {
			for _, row := range hardWrap(line, w) {
				assert.LessOrEqual(t, displayWidth(row), w, "width %d", w)
				if strings.Contains(row, "👨") {
					assert.Contains(t, row, "👦", "the cluster stayed whole at width %d", w)
				}
			}
		}
	})
	t.Run("style_reopened_on_each_row", func(t *testing.T) {
		style := Style{open: sgr(attrDim)}
		rows := hardWrap(style.Wrap("hello world"), 8)
		require.Len(t, rows, 2)
		assert.Equal(t, "\x1b[2mhello wo\x1b[0m", rows[0])
		assert.Equal(t, "\x1b[2mrld\x1b[0m", rows[1])
	})
}

func TestHangWidth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected int
	}{
		{"none", "plain text", 0},
		{"spaces", "    text", 4},
		{"bullet", "• text", 2},
		{"dash", "- text", 2},
		{"star", "* text", 2},
		{"quote", quotePrefix + "text", 2},
		{"ordered", "1. text", 3},
		{"ordered_two_digit", "12. text", 4},
		{"ordered_paren", "3) text", 3},
		{"indented_bullet", "  • text", 4},
		{"styled_bullet", Style{open: sgr(attrDim)}.Wrap("• ") + "text", 2},
		{"digits_without_marker", "2024 was a year", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, hangWidth(tc.input))
		})
	}
}

func TestCells(t *testing.T) {
	t.Parallel()

	t.Run("carries_active_style", func(t *testing.T) {
		cs := cells("a\x1b[1mb\x1b[0mc")
		require.Len(t, cs, 3)
		assert.Empty(t, cs[0].style)
		assert.Equal(t, "\x1b[1m", cs[1].style)
		assert.Empty(t, cs[2].style, "reset clears the active style")
	})
	t.Run("measures_width", func(t *testing.T) {
		cs := cells("ＡB")
		require.Len(t, cs, 2)
		assert.Equal(t, 2, cs[0].width)
		assert.Equal(t, 1, cs[1].width)
	})
}

func TestRenderCells(t *testing.T) {
	t.Parallel()

	t.Run("round_trips_plain", func(t *testing.T) {
		assert.Equal(t, "abc", renderCells(cells("abc"), ""))
	})
	t.Run("emits_style_once", func(t *testing.T) {
		assert.Equal(t, "\x1b[1mab\x1b[0m", renderCells(cells("\x1b[1mab"), ""))
	})
	t.Run("applies_prefix", func(t *testing.T) {
		assert.Equal(t, "  ab", renderCells(cells("ab"), "  "))
	})
}
