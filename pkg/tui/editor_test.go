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
		e.LineStart()
		assert.Equal(t, 4, e.pos)
		e.LineEnd()
		assert.Equal(t, 7, e.pos)
	})
}

func TestEditorKill(t *testing.T) {
	t.Parallel()

	t.Run("to_line_end", func(t *testing.T) {
		e := newEditorAt("abc\ndef", 1)
		e.KillToLineEnd()
		assert.Equal(t, "a\ndef", e.Value())
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

	t.Run("up_keeps_column", func(t *testing.T) {
		e := newEditorAt("abcd\nefgh", 7)
		require.True(t, e.Up())
		assert.Equal(t, 2, e.pos)
	})
	t.Run("up_clamps_to_shorter_line", func(t *testing.T) {
		e := newEditorAt("ab\nefgh", 6)
		require.True(t, e.Up())
		assert.Equal(t, 2, e.pos)
	})
	t.Run("up_on_first_line_declines", func(t *testing.T) {
		e := newEditorAt("abc", 1)
		assert.False(t, e.Up())
	})
	t.Run("down_keeps_column", func(t *testing.T) {
		e := newEditorAt("abcd\nefgh", 2)
		require.True(t, e.Down())
		assert.Equal(t, 7, e.pos)
	})
	t.Run("down_on_last_line_declines", func(t *testing.T) {
		e := newEditorAt("abc", 1)
		assert.False(t, e.Down())
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
		assert.Equal(t, "older", e.Value(), "stops at the oldest entry")

		e.HistoryNext()
		assert.Equal(t, "newer", e.Value())
		e.HistoryNext()
		assert.Equal(t, "draft", e.Value(), "live buffer comes back")
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

	th := NewTheme(ColorNone)

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
