package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestInline returns an inline renderer painting into v.
func newTestInline(v *vt) *inlineRenderer {
	return &inlineRenderer{t: &termState{out: v, fd: -1, width: v.w, height: v.h}}
}

func TestInlineRendererCommit(t *testing.T) {
	t.Parallel()

	t.Run("history_then_live_block", func(t *testing.T) {
		v := newVT(20, 8)
		r := newTestInline(v)
		r.setLive([]string{"❯ ", "ctx 0%"}, 0, 2)
		r.commit([]histLine{{text: "one"}, {text: "two"}})

		assert.Equal(t, "one", v.Line(0))
		assert.Equal(t, "two", v.Line(1))
		assert.Equal(t, "❯", v.Line(2), "the block follows the output")
		assert.Equal(t, "ctx 0%", v.Line(3))
	})
	t.Run("block_is_erased_not_duplicated", func(t *testing.T) {
		v := newVT(20, 8)
		r := newTestInline(v)
		r.setLive([]string{"❯ hi", "ctx 0%"}, 0, 4)
		r.commit([]histLine{{text: "out"}})

		assert.Equal(t, "out", v.Line(0))
		assert.Equal(t, "❯ hi", v.Line(1))
		assert.Equal(t, "ctx 0%", v.Line(2))
		assert.Empty(t, v.Line(3), "no leftover copy of the old block")
	})
	t.Run("prose_left_for_the_terminal_to_wrap", func(t *testing.T) {
		v := newVT(10, 8)
		r := newTestInline(v)
		r.commit([]histLine{{text: "aaaa bbbb cccc"}})

		// the terminal wrapped it, so it can reflow it again on resize
		assert.Equal(t, "aaaa bbbb", v.Line(0))
		assert.Equal(t, "cccc", v.Line(1))
	})
	t.Run("wrapped_lines_wrapped_by_us", func(t *testing.T) {
		v := newVT(10, 8)
		r := newTestInline(v)
		r.commit([]histLine{{text: "• aaaa bbbb", flow: flowWrap}})

		assert.Equal(t, "• aaaa", v.Line(0))
		assert.Equal(t, "  bbbb", v.Line(1), "continuation aligned under the text")
	})
	t.Run("clipped_lines_lose_their_tail", func(t *testing.T) {
		v := newVT(10, 8)
		r := newTestInline(v)
		r.commit([]histLine{{text: "│ aaaa │ bbbb │", flow: flowClip}})

		assert.Equal(t, "│ aaaa │ b", v.Line(0))
		assert.Empty(t, v.Line(1), "structural lines never wrap")
	})
	t.Run("cursor_parks_on_the_block_not_the_caret", func(t *testing.T) {
		v := newVT(20, 8)
		r := newTestInline(v)
		r.setLive([]string{"❯ ab", "ctx"}, 0, 4)
		r.commit([]histLine{{text: "x"}})

		// the caret is painted into its row (paintCaret), which frees the
		// terminal's cursor to sit where the next erase needs to start
		assert.Equal(t, "❯ ab", v.Line(1), "the caret does not alter the text")
		assert.Equal(t, 1, v.row, "parked on the block's first row")
		assert.Equal(t, 0, v.col)
	})
	t.Run("multi_row_block", func(t *testing.T) {
		v := newVT(20, 8)
		r := newTestInline(v)
		r.setLive([]string{"⠋ tool", "❯ a", "  b", "ctx"}, 2, 3)
		r.commit([]histLine{{text: "hist"}})

		assert.Equal(t, "hist", v.Line(0))
		assert.Equal(t, "⠋ tool", v.Line(1))
		assert.Equal(t, "  b", v.Line(3), "the caret row keeps its text")
		assert.Equal(t, "ctx", v.Line(4))
		assert.Equal(t, 1, v.row, "the cursor parks on the block's first row")
	})
}

func TestInlineRendererScrollsNaturally(t *testing.T) {
	t.Parallel()

	v := newVT(10, 4)
	r := newTestInline(v)
	r.setLive([]string{"❯", "ctx"}, 0, 1)
	for _, s := range []string{"a", "b", "c", "d"} {
		r.commit([]histLine{{text: s}})
	}

	assert.NotEmpty(t, v.scrollback, "overflow reaches the terminal's own scrollback")
	assert.Equal(t, "d", v.Line(1))
	assert.Equal(t, "❯", v.Line(2))
	assert.Equal(t, "ctx", v.Line(3))
}

func TestInlineRendererResize(t *testing.T) {
	t.Parallel()

	t.Run("cursor_parks_on_the_blocks_first_row", func(t *testing.T) {
		v := newVT(20, 8)
		r := newTestInline(v)
		r.commit([]histLine{{text: "kept"}})
		r.setLive([]string{"❯ 0123456789", "  x", "ctx"}, 1, 3)

		// the erase depends on this and nothing else: not on the width, not on
		// how many rows the block turned out to occupy
		assert.Equal(t, 1, v.row, "parked on the divider row, below the history line")
		assert.Equal(t, 0, v.col)
	})
	t.Run("resize_alone_draws_nothing", func(t *testing.T) {
		v := newVT(20, 8)
		r := newTestInline(v)
		r.commit([]histLine{{text: "kept"}})
		r.setLive([]string{"❯ x", "ctx"}, 0, 2)
		before := v.Screen()

		r.t.sizeFn = func() (int, int, error) { return 10, 8, nil }
		r.resize()

		assert.Equal(t, before, v.Screen(), "resize only picks the size up")
		assert.Equal(t, 10, r.t.width)
	})
	t.Run("erase_lands_on_the_reflowed_block", func(t *testing.T) {
		v := newVT(40, 10)
		r := newTestInline(v)
		r.t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }
		r.commit([]histLine{{text: "committed output", flow: flowReflow}})
		r.setLive([]string{strings.Repeat(ruleChar, 39), "❯ x", "ctx"}, 1, 2)

		// the divider now spans two rows on the narrower grid; the next erase
		// has to climb both or it strands one above the redrawn block
		v.setSize(20, 10)
		r.resize()
		r.setLive([]string{strings.Repeat(ruleChar, 19), "❯ x", "ctx"}, 1, 2)

		assert.Equal(t, 1, countRules(v.Screen()), "exactly one divider")
		assert.Equal(t, "committed output", v.Line(0))
	})
	t.Run("committed_rows_are_never_re_rendered", func(t *testing.T) {
		v := newVT(40, 12)
		r := newTestInline(v)
		r.t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }
		r.commit([]histLine{
			{text: "❯ alpha bravo charlie delta echo foxtrot golf hotel india", flow: flowWrap},
			{text: "a reply that also runs past the edge of this narrow screen", flow: flowReflow},
		})
		r.setLive([]string{strings.Repeat(ruleChar, 39), "❯ x", "ctx"}, 1, 2)

		// whatever the emulator's reflow makes of those rows is the truth; a
		// redraw at the new width must leave every one of them alone
		v.setSize(20, 12)
		reflowed := []string{v.Line(0), v.Line(1), v.Line(2), v.Line(3), v.Line(4), v.Line(5)}

		r.resize()
		r.setLive([]string{strings.Repeat(ruleChar, 19), "❯ x", "ctx"}, 1, 2)

		assert.Equal(t, reflowed, []string{
			v.Line(0), v.Line(1), v.Line(2), v.Line(3), v.Line(4), v.Line(5),
		}, "committed rows belong to the terminal, exactly like cat output")
	})
	t.Run("leaves_content_above_the_session", func(t *testing.T) {
		v := newVT(40, 12)
		r := newTestInline(v)
		r.t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }
		// whatever the terminal held before the session started
		v.WriteString("$ ls\r\nREADME.md  main.go\r\n$ ajent\r\n")
		r.commit([]histLine{{text: "session line one", flow: flowReflow}})
		r.setLive([]string{strings.Repeat(ruleChar, 39), "❯ x", "ctx"}, 1, 2)

		v.setSize(20, 12)
		r.resize()
		r.setLive([]string{strings.Repeat(ruleChar, 19), "❯ x", "ctx"}, 1, 2)

		screen := v.Screen()
		assert.Contains(t, screen, "README.md", "the shell's own output is untouchable")
		assert.Contains(t, screen, "$ ajent")
		assert.Equal(t, 1, strings.Count(screen, "session line one"))
	})
	t.Run("erase_needs_no_row_maths", func(t *testing.T) {
		v := newVT(20, 10)
		r := newTestInline(v)
		r.t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }
		r.commit([]histLine{{text: "kept"}})
		// a block whose rows reflow into several each: nothing counts them
		r.setLive([]string{strings.Repeat(ruleChar, 19), "❯ x", "ctx"}, 1, 2)

		v.setSize(6, 10)
		r.setLive([]string{strings.Repeat(ruleChar, 5), "❯ x", "ctx"}, 1, 2)

		assert.Equal(t, 1, countRules(v.Screen()), "the whole reflowed block went")
		assert.Equal(t, "kept", v.Line(0), "and history was left alone")
	})
	t.Run("repeated_draws_never_accumulate", func(t *testing.T) {
		v := newVT(40, 10)
		r := newTestInline(v)
		r.t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }
		r.commit([]histLine{{text: "kept"}})

		// each miss used to leave another divider behind; drive many draws
		// across many widths and assert nothing ever piles up
		for _, w := range []int{40, 17, 33, 9, 26, 40} {
			v.setSize(w, 10)
			r.setLive([]string{strings.Repeat(ruleChar, max(w-1, 1)), "❯ x", "ctx"}, 1, 2)
			assert.Equal(t, 1, countRules(v.Screen()), "width %d", w)
		}
	})
	t.Run("live_rows_never_fill_the_last_column", func(t *testing.T) {
		v := newVT(20, 8)
		r := newTestInline(v)
		r.setLive([]string{strings.Repeat(ruleChar, 20), "ctx"}, 1, 0)
		// a row ending in the last column leaves the cursor in deferred wrap, which
		// emulators reflow inconsistently
		assert.Equal(t, 19, displayWidth(v.Line(0)))
	})
	t.Run("history_is_never_touched", func(t *testing.T) {
		v := newVT(20, 8)
		r := newTestInline(v)
		r.commit([]histLine{{text: "kept"}})
		r.setLive([]string{"❯", "ctx"}, 0, 1)
		r.resize()
		assert.Equal(t, "kept", v.Line(0), "resize must not rewrite history")
	})
}

func TestInlineRendererSuspend(t *testing.T) {
	t.Parallel()

	v := newVT(20, 8)
	r := newTestInline(v)
	r.commit([]histLine{{text: "kept"}})
	r.setLive([]string{"❯ hi", "ctx"}, 0, 4)
	require.Equal(t, "❯ hi", v.Line(1))

	r.suspend(-1)
	assert.Equal(t, "kept", v.Line(0), "history stays on the main screen")
	assert.Empty(t, v.Line(1), "the live block is cleared for the shell")

	t.Run("resume_redraws_the_block", func(t *testing.T) {
		require.NoError(t, r.resume(-1))
		r.setLive([]string{"❯ hi", "ctx"}, 0, 4)

		assert.Equal(t, "❯ hi", v.Line(1))
		assert.Equal(t, "ctx", v.Line(2))
	})
}

// TestInlineRendererResizeRace models a resize landing in the middle of a frame:
// the rows are composed at the old width, but the terminal has reflowed by the
// time they are written. This is the window the resize gate narrows but cannot
// close, and it is what used to strand a divider on every miss, compounding.
// The park re-reads the size after composing the rows, so it still lands on the
// block's first row and the next erase takes the whole block with it.
func TestInlineRendererResizeRace(t *testing.T) {
	t.Parallel()

	v := newVT(40, 10)
	r := newTestInline(v)
	r.t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }
	r.commit([]histLine{{text: "committed history", flow: flowReflow}})
	r.setLive([]string{strings.Repeat(ruleChar, 39), "\u276f x", "ctx"}, 1, 2)

	// the terminal has already reflowed, but the size read that composes the
	// frame has not caught up; the one the park takes has
	var reads int
	r.t.sizeFn = func() (int, int, error) {
		if reads++; reads == 1 {
			v.setSize(20, 10)  // the emulator reflowed
			return 40, 10, nil // and this read is still stale
		}
		return v.w, v.h, nil
	}
	r.setLive([]string{strings.Repeat(ruleChar, 39), "\u276f x", "ctx"}, 1, 2)

	// the divider was drawn too wide and wrapped, but nothing was stranded: the
	// next frame at the true width erases all of it
	r.t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }
	r.setLive([]string{strings.Repeat(ruleChar, 19), "\u276f x", "ctx"}, 1, 2)

	assert.Equal(t, 1, countRules(v.Screen()), "the whole reflowed block was erased")
	assert.Equal(t, "committed history", v.Line(0), "and history was left alone")
}
