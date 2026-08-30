package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newEditorAt returns an editor holding value with the cursor at pos.
func newEditorAt(value string, pos int) *editor {
	e := &editor{}
	e.SetValue(value)
	e.pos = pos
	return e
}

func TestEditorSetValueAt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, value string
		off, want   int
	}{
		{"offset_at_start", "hello", 0, 0},
		{"offset_mid_text", "fix the retry loop", 8, 8},
		{"negative_lands_at_end", "hello", -1, 5},
		{"offset_past_end", "hello", 99, 5},
		{"offset_inside_cluster", "a\U0001F44D\U0001F3FDb", 5, 1}, // byte 5 sits inside the emoji
		{"multibyte_prefix", "h\u00e9llo", 3, 2},                  // é is two bytes, one cell
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &editor{}
			e.SetValueAt(tc.value, tc.off)
			assert.Equal(t, tc.value, e.Value())
			assert.Equal(t, tc.want, e.pos)
		})
	}
}

func TestEditorInsert(t *testing.T) {
	t.Parallel()

	t.Run("at_cursor", func(t *testing.T) {
		e := newEditorAt("ac", 1)
		e.Insert("b")
		assert.Equal(t, "abc", e.Value())
		assert.Equal(t, 2, e.pos)
	})
	t.Run("multi_cell_text", func(t *testing.T) {
		e := &editor{}
		e.Insert("hi\nthere")
		assert.Equal(t, "hi\nthere", e.Value())
		assert.Equal(t, 8, e.pos)
	})
	t.Run("empty_is_noop", func(t *testing.T) {
		e := newEditorAt("ab", 1)
		e.Insert("")
		assert.Equal(t, "ab", e.Value())
		assert.Equal(t, 1, e.pos)
	})
	t.Run("grapheme_counts_as_one", func(t *testing.T) {
		e := &editor{}
		e.Insert("👍")
		assert.Equal(t, 1, e.pos)
	})
}

func TestEditorDelete(t *testing.T) {
	t.Parallel()

	t.Run("backspace", func(t *testing.T) {
		e := newEditorAt("abc", 2)
		e.Backspace()
		assert.Equal(t, "ac", e.Value())
		assert.Equal(t, 1, e.pos)
	})
	t.Run("backspace_at_start", func(t *testing.T) {
		e := newEditorAt("abc", 0)
		e.Backspace()
		assert.Equal(t, "abc", e.Value())
	})
	t.Run("delete_forward", func(t *testing.T) {
		e := newEditorAt("abc", 1)
		e.DeleteForward()
		assert.Equal(t, "ac", e.Value())
		assert.Equal(t, 1, e.pos)
	})
	t.Run("delete_at_end", func(t *testing.T) {
		e := newEditorAt("abc", 3)
		e.DeleteForward()
		assert.Equal(t, "abc", e.Value())
	})
}

func TestEditorMovement(t *testing.T) {
	t.Parallel()

	t.Run("left_right_clamped", func(t *testing.T) {
		e := newEditorAt("ab", 0)
		e.Left()
		assert.Equal(t, 0, e.pos)
		e.Right()
		e.Right()
		e.Right()
		assert.Equal(t, 2, e.pos)
	})
	t.Run("word_left", func(t *testing.T) {
		e := newEditorAt("one two three", 13)
		e.WordLeft()
		assert.Equal(t, 8, e.pos)
		e.WordLeft()
		assert.Equal(t, 4, e.pos)
	})
	t.Run("word_right", func(t *testing.T) {
		e := newEditorAt("one two three", 0)
		e.WordRight()
		assert.Equal(t, 3, e.pos)
		e.WordRight()
		assert.Equal(t, 7, e.pos)
	})
	t.Run("line_start_and_end", func(t *testing.T) {
		e := newEditorAt("one\ntwo", 5)
		e.LineStart(60)
		assert.Equal(t, 4, e.pos)
		e.LineEnd(60)
		assert.Equal(t, 7, e.pos)
	})
	t.Run("line_start_and_end_wrapped", func(t *testing.T) {
		// "hello world foo" at width 9 wraps to [hello][world][foo]: home and end
		// bound the visual row, not the whole buffer
		e := newEditorAt("hello world foo", 8)
		e.LineStart(9)
		assert.Equal(t, 6, e.pos)
		e.LineEnd(9)
		assert.Equal(t, 11, e.pos)
	})
	t.Run("line_end_stops_at_newline", func(t *testing.T) {
		// a wrapped row ending on an explicit newline stops before it
		e := newEditorAt("hello world\nfoo", 2)
		e.LineEnd(9)
		assert.Equal(t, 5, e.pos)
		e.LineStart(9)
		assert.Equal(t, 0, e.pos)
	})
}

func TestEditorKill(t *testing.T) {
	t.Parallel()

	const wide = 60 // wide enough that short fixtures never wrap

	t.Run("to_line_end", func(t *testing.T) {
		e := newEditorAt("abc\ndef", 1)
		e.KillToLineEnd(wide)
		assert.Equal(t, "a\ndef", e.Value())
		assert.Equal(t, 1, e.pos)
	})
	t.Run("mid_line_caret_stays_put", func(t *testing.T) {
		e := newEditorAt("abcdef", 3)
		e.KillToLineEnd(wide)
		assert.Equal(t, "abc", e.Value())
		assert.Equal(t, 3, e.pos)
	})
	t.Run("wrapped_row_clears_only_to_row_end", func(t *testing.T) {
		// "hello world" at width 9 wraps to [hello][world]; killing from the first
		// row clears only that row, never the wrapped remainder below
		e := newEditorAt("hello world", 2)
		e.KillToLineEnd(9)
		assert.Equal(t, "he world", e.Value())
		assert.Equal(t, 2, e.pos)
	})
	t.Run("wrapped_second_row_clears_only_that_row", func(t *testing.T) {
		e := newEditorAt("hello world", 6)
		e.KillToLineEnd(9)
		assert.Equal(t, "hello ", e.Value())
		assert.Equal(t, 6, e.pos)
	})
	t.Run("wrapped_far_left_stays_column_zero", func(t *testing.T) {
		// far-left of a wrapped continuation row with content below: the caret must
		// land on column zero of where content resumes, not drift up onto the line
		// above. "hello world foo" at width 9 wraps to [hello][world][foo].
		e := newEditorAt("hello world foo", 6)
		e.KillToLineEnd(9)
		assert.Equal(t, "hello foo", e.Value())
		assert.Equal(t, 6, e.pos)
		starts, _ := e.layout(9)
		assert.Contains(t, starts, e.pos, "caret on a visual row start")
	})
	t.Run("wrapped_far_left_follows_content_up", func(t *testing.T) {
		// "ab cdefg hi" at width 9 wraps to [ab][cdefg][hi]; clearing the middle row
		// lets "hi" fit on the first, and the caret follows it there
		e := newEditorAt("ab cdefg hi", 3)
		e.KillToLineEnd(9)
		assert.Equal(t, "ab hi", e.Value())
		assert.Equal(t, 3, e.pos, "caret before the content that moved up")
	})
	t.Run("wrapped_far_left_hard_split_keeps_space", func(t *testing.T) {
		// the row below is joined by a hard split rather than a dropped space, so the
		// space before the cleared row must survive
		e := newEditorAt("ab cdefghijklmno", 3)
		e.KillToLineEnd(9)
		assert.Equal(t, "ab jklmno", e.Value())
		assert.Equal(t, 3, e.pos)
	})
	t.Run("wrapped_far_left_last_row_keeps_end", func(t *testing.T) {
		// far-left of the final wrapped row has nothing below to pull up; caret stays
		// at the end like a mid-line kill.
		e := newEditorAt("hello world foo", 12)
		e.KillToLineEnd(9)
		assert.Equal(t, "hello world ", e.Value())
		assert.Equal(t, 12, e.pos)
	})
	t.Run("empty_trailing_line_deletes_join", func(t *testing.T) {
		e := newEditorAt("abc\n", 4)
		e.KillToLineEnd(wide)
		assert.Equal(t, "abc", e.Value())
		assert.Equal(t, 0, e.pos)
	})
	t.Run("empty_middle_line_joins_below", func(t *testing.T) {
		e := newEditorAt("ab\n\ncd", 3)
		e.KillToLineEnd(wide)
		assert.Equal(t, "ab\ncd", e.Value())
		assert.Equal(t, 3, e.pos, "cursor at the start of the joined line below")
	})
	t.Run("empty_first_line_joins_below", func(t *testing.T) {
		e := newEditorAt("\nabc", 0)
		e.KillToLineEnd(wide)
		assert.Equal(t, "abc", e.Value())
		assert.Equal(t, 0, e.pos)
	})
	t.Run("repeat_presses_consume_one_unit_each", func(t *testing.T) {
		// Delete semantics: each press removes exactly one row (join, clear,
		// join, clear), never more than one per press
		e := newEditorAt("ab\n\ncd", 3)
		e.KillToLineEnd(wide)
		assert.Equal(t, "ab\ncd", e.Value())
		e.KillToLineEnd(wide)
		assert.Equal(t, "ab\n", e.Value())
		e.KillToLineEnd(wide)
		assert.Equal(t, "ab", e.Value())
		e.KillToLineEnd(wide)
		assert.Empty(t, e.Value())
		e.KillToLineEnd(wide)
		assert.Empty(t, e.Value(), "nothing left to remove")
	})
	t.Run("nonempty_line_tail_is_noop", func(t *testing.T) {
		e := newEditorAt("hello\nworld", 5)
		e.KillToLineEnd(wide)
		assert.Equal(t, "hello\nworld", e.Value())
		assert.Equal(t, 5, e.pos)
	})
	t.Run("line_before_cursor", func(t *testing.T) {
		e := newEditorAt("abc\ndef", 6)
		e.KillLine()
		assert.Equal(t, "abc\nf", e.Value())
		assert.Equal(t, 4, e.pos)
	})
	t.Run("word_back", func(t *testing.T) {
		e := newEditorAt("one two ", 8)
		e.KillWordBack()
		assert.Equal(t, "one ", e.Value())
		assert.Equal(t, 4, e.pos)
	})
}

func TestEditorLineNavigation(t *testing.T) {
	t.Parallel()

	// a wide layout keeps each logical line on its own visual row.
	const wide = 60

	t.Run("up_keeps_column", func(t *testing.T) {
		e := newEditorAt("abcd\nefgh", 7)
		require.True(t, e.Up(wide))
		assert.Equal(t, 2, e.pos)
	})
	t.Run("up_clamps_to_shorter_line", func(t *testing.T) {
		e := newEditorAt("ab\nefgh", 6)
		require.True(t, e.Up(wide))
		assert.Equal(t, 2, e.pos)
	})
	t.Run("up_on_first_line_declines", func(t *testing.T) {
		e := newEditorAt("abc", 1)
		assert.False(t, e.Up(wide))
	})
	t.Run("down_keeps_column", func(t *testing.T) {
		e := newEditorAt("abcd\nefgh", 2)
		require.True(t, e.Down(wide))
		assert.Equal(t, 7, e.pos)
	})
	t.Run("down_on_last_line_declines", func(t *testing.T) {
		e := newEditorAt("abc", 1)
		assert.False(t, e.Down(wide))
	})
	t.Run("up_walks_wrapped_rows_same_column", func(t *testing.T) {
		// "one two three" wraps to rows [one][two][three] at width 7; the caret on
		// row one ('o' of "two") moves up to row zero keeping its within-row column.
		e := newEditorAt("one two three", 6)
		require.True(t, e.Up(7))
		assert.Equal(t, 2, e.pos) // moves from 'two' onto the end of 'one'
	})
	t.Run("down_walks_wrapped_rows_same_column", func(t *testing.T) {
		// the caret on row zero ('n' of "one") moves down to row one at the same column.
		e := newEditorAt("one two three", 1)
		require.True(t, e.Down(7))
		assert.Equal(t, 5, e.pos) // moves from 'one' onto 'two' at the same column
	})
	t.Run("down_clamps_to_shorter_visual_row", func(t *testing.T) {
		// a row below that ends before the target column clamps to its end.
		e := newEditorAt("abcdef\nx", 3)
		require.True(t, e.Down(wide))
		assert.Equal(t, 8, e.pos) // row below is shorter than the same column
	})
}

func TestEditorHistory(t *testing.T) {
	t.Parallel()

	t.Run("submit_records_and_clears", func(t *testing.T) {
		e := &editor{}
		e.SetValue("first")
		assert.Equal(t, "first", e.Submit())
		assert.Empty(t, e.Value())
		assert.Equal(t, []string{"first"}, e.history)
	})
	t.Run("blank_not_recorded", func(t *testing.T) {
		e := &editor{}
		e.SetValue("  ")
		e.Submit()
		assert.Empty(t, e.history)
	})
	t.Run("duplicate_not_repeated", func(t *testing.T) {
		e := &editor{}
		for range 2 {
			e.SetValue("same")
			e.Submit()
		}
		assert.Equal(t, []string{"same"}, e.history)
	})
	t.Run("browse_and_restore_draft", func(t *testing.T) {
		e := &editor{}
		e.SetValue("older")
		e.Submit()
		e.SetValue("newer")
		e.Submit()
		e.SetValue("draft")

		e.HistoryPrev()
		assert.Equal(t, "newer", e.Value())
		e.HistoryPrev()
		assert.Equal(t, "older", e.Value())
		e.HistoryPrev()
		assert.Equal(t, "older", e.Value())

		e.HistoryNext()
		assert.Equal(t, "newer", e.Value())
		e.HistoryNext()
		assert.Equal(t, "draft", e.Value())
		e.HistoryNext()
		assert.Equal(t, "draft", e.Value())
	})
	t.Run("empty_history_is_noop", func(t *testing.T) {
		e := &editor{}
		e.SetValue("draft")
		e.HistoryPrev()
		assert.Equal(t, "draft", e.Value())
	})
}

func TestEditorInputView(t *testing.T) {
	t.Parallel()

	th := NewTheme(ColorNone, DefaultPalette())

	t.Run("empty_shows_hint", func(t *testing.T) {
		e := &editor{}
		rows, curRow, curCol := e.inputView(th, 60, 5)
		require.Len(t, rows, 1)
		assert.Contains(t, rows[0], promptFirst)
		assert.Contains(t, rows[0], inputHint)
		assert.Equal(t, 0, curRow)
		assert.Equal(t, 2, curCol, "cursor sits at the start of the hint")
	})
	t.Run("single_line", func(t *testing.T) {
		e := newEditorAt("hi", 2)
		rows, curRow, curCol := e.inputView(th, 60, 5)
		assert.Equal(t, []string{promptFirst + "hi"}, rows)
		assert.Equal(t, 0, curRow)
		assert.Equal(t, 4, curCol)
	})
	t.Run("explicit_newline_indents", func(t *testing.T) {
		e := newEditorAt("a\nb", 3)
		rows, curRow, curCol := e.inputView(th, 60, 5)
		assert.Equal(t, []string{promptFirst + "a", promptCont + "b"}, rows)
		assert.Equal(t, 1, curRow)
		assert.Equal(t, 3, curCol)
	})
	t.Run("wraps_at_width", func(t *testing.T) {
		e := newEditorAt("abcdef", 6)
		rows, curRow, curCol := e.inputView(th, 5, 5)
		assert.Equal(t, []string{promptFirst + "abc", promptCont + "def"}, rows)
		assert.Equal(t, 1, curRow)
		assert.Equal(t, 5, curCol)
	})
	t.Run("wraps_words_whole", func(t *testing.T) {
		// an unbroken token still splits, but a word that overflows moves to the
		// next line intact rather than being cut across lines
		e := newEditorAt("one two three", 13)
		rows, curRow, curCol := e.inputView(th, 7, 5)
		assert.Equal(t, []string{promptFirst + "one", promptCont + "two", promptCont + "three"}, rows)
		assert.Equal(t, 2, curRow)
		assert.Equal(t, 7, curCol)
	})
	t.Run("wraps_word_when_partial_room", func(t *testing.T) {
		// a row with room for only part of the next word still moves it whole
		e := newEditorAt("hello world", 5)
		rows, _, _ := e.inputView(th, 9, 5)
		assert.Equal(t, []string{promptFirst + "hello", promptCont + "world"}, rows)
	})
	t.Run("windows_to_max_rows", func(t *testing.T) {
		e := newEditorAt("a\nb\nc\nd", 7)
		rows, curRow, _ := e.inputView(th, 60, 2)
		assert.Equal(t, []string{promptCont + "c", promptCont + "d"}, rows)
		assert.Equal(t, 1, curRow, "the cursor row stays visible")
	})
	t.Run("wide_runes_measured", func(t *testing.T) {
		e := newEditorAt("ＡＢ", 2)
		rows, _, curCol := e.inputView(th, 60, 5)
		assert.Equal(t, []string{promptFirst + "ＡＢ"}, rows)
		assert.Equal(t, 6, curCol)
	})
	t.Run("leading_bang_swaps_marker_no_duplicate", func(t *testing.T) {
		e := newEditorAt("!ls -la", 2)
		rows, _, curCol := e.inputView(th, 60, 5)
		// the literal `!` replaces ❯ as the marker: exactly one bang on screen
		assert.Equal(t, []string{"!ls -la"}, rows)
		assert.NotContains(t, rows[0], promptFirst)
		assert.Equal(t, 1, strings.Count(rows[0], "!"), "no duplicate ! marker")
		// cursor is after the `l`, so col = width of `!` + one cell
		assert.Equal(t, 2, curCol)
	})
	t.Run("bare_bang_is_the_marker", func(t *testing.T) {
		e := newEditorAt("!", 1)
		rows, _, curCol := e.inputView(th, 60, 5)
		assert.Equal(t, []string{"!"}, rows)
		assert.Equal(t, 1, curCol)
	})
	t.Run("bang_mid_buffer_keeps_prompt", func(t *testing.T) {
		e := newEditorAt("a!b", 2)
		rows, _, _ := e.inputView(th, 60, 5)
		assert.Equal(t, []string{promptFirst + "a!b"}, rows)
	})
	t.Run("continuation_lines_keep_indent", func(t *testing.T) {
		e := newEditorAt("!echo a\necho b", 10)
		rows, _, _ := e.inputView(th, 60, 5)
		assert.Equal(t, []string{"!echo a", promptCont + "echo b"}, rows)
	})
}

// TestPrefixWidths pins the prompt glyphs the editor budgets against: layout
// subtracts these widths from every row, so a glyph the terminal measures wider
// than uniseg does costs the row a column it already spent.
func TestPrefixWidths(t *testing.T) {
	t.Parallel()

	t.Run("prompt_glyph_is_two_columns", func(t *testing.T) {
		assert.Equal(t, 2, displayWidth(promptFirst))
		assert.Equal(t, displayWidth(promptFirst), displayWidth(promptCont))
	})

	t.Run("prompt_and_continuation_align", func(t *testing.T) {
		var e editor
		e.SetValue("hello")
		first, cont := e.prefixWidths()
		assert.Equal(t, 2, first)
		assert.Equal(t, 2, cont)
	})

	t.Run("shell_marker_drops_the_glyph", func(t *testing.T) {
		var e editor
		e.SetValue("!ls")
		first, cont := e.prefixWidths()
		assert.Equal(t, 0, first)
		assert.Equal(t, 2, cont)
	})
}
