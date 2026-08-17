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
	t.Run("caret_left_in_the_input", func(t *testing.T) {
		v := newVT(20, 8)
		r := newTestInline(v)
		r.setLive([]string{"❯ ab", "ctx"}, 0, 4)
		r.commit([]histLine{{text: "x"}})

		v.WriteString("Z")
		assert.Equal(t, "❯ abZ", v.Line(1))
	})
	t.Run("multi_row_block", func(t *testing.T) {
		v := newVT(20, 8)
		r := newTestInline(v)
		r.setLive([]string{"⠋ tool", "❯ a", "  b", "ctx"}, 2, 3)
		r.commit([]histLine{{text: "hist"}})

		assert.Equal(t, "hist", v.Line(0))
		assert.Equal(t, "⠋ tool", v.Line(1))
		assert.Equal(t, "ctx", v.Line(4))
		v.WriteString("Z")
		assert.Equal(t, "  bZ", v.Line(3), "caret sat on the second input row")
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

	t.Run("caret_offset_follows_the_reflow", func(t *testing.T) {
		v := newVT(20, 8)
		r := newTestInline(v)
		// two input rows, caret on the second; the first is 12 columns wide
		r.setLive([]string{"❯ 0123456789", "  x", "ctx"}, 1, 3)
		assert.Equal(t, 1, r.caretOffset())

		r.t.width = 6 // what the emulator's reflow would leave the rows at

		assert.Equal(t, 1, r.caretRow, "caretRow stays an index into the block")
		assert.Equal(t, 2, r.caretOffset(), "the 12 column row now occupies two rows")
	})
	t.Run("width_change_repaints_the_viewport", func(t *testing.T) {
		v := newVT(20, 8)
		r := newTestInline(v)
		r.commit([]histLine{{text: "one two three four five", flow: flowReflow}})
		r.setLive([]string{strings.Repeat(ruleChar, 19), "❯ x", "ctx"}, 1, 2)

		r.t.sizeFn = func() (int, int, error) { return 10, 8, nil }
		r.resize()

		screen := v.Screen()
		assert.Contains(t, screen, "one two", "history re-wraps at the new width")
		var rules int
		for i := 0; i < v.h; i++ {
			if l := v.Line(i); l != "" && strings.Trim(l, ruleChar) == "" {
				rules++
				assert.Equal(t, 9, displayWidth(l), "the divider is redrawn at the new live width")
			}
		}
		assert.Equal(t, 1, rules, "the repaint rewrites the old divider row, never duplicates it")
	})
	t.Run("repaint_full_clears_leftovers_below", func(t *testing.T) {
		v := newVT(20, 8)
		r := newTestInline(v)
		r.setLive([]string{"❯ x", "ctx"}, 0, 2)
		// junk the previous frame left below the content (a racing draw, a
		// resize that shrank the block): the repaint must clear it
		v.WriteString(cursorTo(5, 1) + "junk" + cursorTo(6, 1) + "junk")

		r.t.sizeFn = func() (int, int, error) { return 10, 8, nil }
		r.resize()

		assert.NotContains(t, v.Screen(), "junk")
	})
	t.Run("resizing_twice_is_idempotent", func(t *testing.T) {
		v := newVT(20, 8)
		r := newTestInline(v)
		r.setLive([]string{"❯ 0123456789", "  x", "ctx"}, 1, 3)

		r.t.sizeFn = func() (int, int, error) { return 6, 8, nil }
		r.resize()
		first := v.Screen()
		r.resize() // a second burst at the same size is a no-op
		assert.Equal(t, first, v.Screen())
	})
	t.Run("height_only_change_draws_nothing", func(t *testing.T) {
		v := newVT(20, 8)
		r := newTestInline(v)
		r.commit([]histLine{{text: "kept"}})
		r.setLive([]string{"❯ x", "ctx"}, 0, 2)
		before := v.Screen()

		r.t.sizeFn = func() (int, int, error) { return 20, 6, nil }
		r.resize()

		assert.Equal(t, before, v.Screen(), "no reflow means no repaint")
	})
	t.Run("caret_offset_never_erases_above_the_screen", func(t *testing.T) {
		v := newVT(20, 4)
		r := newTestInline(v)
		// a live block taller than the screen: the repaint cuts into it, and the
		// erase must count only the rows actually visible
		r.setLive([]string{"a", "b", "c", "d", "e", "f"}, 5, 1)
		r.t.sizeFn = func() (int, int, error) { return 10, 4, nil }
		r.resize()

		assert.Equal(t, 3, r.caretOffset(), "capped at the caret's screen row")
	})
	t.Run("measures_what_was_drawn_not_the_full_row", func(t *testing.T) {
		v := newVT(20, 8)
		r := newTestInline(v)
		// far wider than the terminal: only what fits was ever emitted, so only
		// that can reflow. Measuring the full string would erase history above.
		r.setLive([]string{strings.Repeat("x", 200), "ctx"}, 1, 0)

		r.t.width = 10

		assert.Equal(t, 2, r.caretOffset(), "19 drawn columns reflow to two rows at width 10")
	})
	t.Run("erase_uses_the_live_width_not_the_last_signal", func(t *testing.T) {
		v := newVT(40, 8)
		r := newTestInline(v)
		r.setLive([]string{strings.Repeat(ruleChar, 40), "❯ x", "ctx"}, 1, 2)

		// the terminal narrowed but the debounced SIGWINCH has not fired yet, so
		// resize() has not run. A repaint landing now (spinner tick, progress row)
		// must still erase against the width the block is actually reflowed at,
		// or it under-shoots and strands the old divider on screen.
		r.t.sizeFn = func() (int, int, error) { return 20, 8, nil }
		r.setLive([]string{strings.Repeat(ruleChar, 20), "❯ x", "ctx"}, 1, 2)

		assert.Equal(t, 20, r.t.width, "the draw picked up the new width itself")
		var rules int
		for i := 0; i < v.h; i++ {
			if l := v.Line(i); l != "" && strings.Trim(l, ruleChar) == "" {
				rules++
			}
		}
		assert.Equal(t, 1, rules, "exactly one divider; the old block was fully erased")
	})
	t.Run("no_change_when_width_is_stable", func(t *testing.T) {
		v := newVT(20, 8)
		r := newTestInline(v)
		r.setLive([]string{"❯ a", "ctx"}, 0, 3)
		r.resize()
		assert.Equal(t, 0, r.caretOffset())
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

func TestRowsFor(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 1, rowsFor("", 10))
	assert.Equal(t, 1, rowsFor("0123456789", 10))
	assert.Equal(t, 2, rowsFor("0123456789a", 10))
	assert.Equal(t, 1, rowsFor("abc", 0))
	assert.Equal(t, 1, rowsFor("\x1b[1mabc\x1b[0m", 10))
}

// TestInlineRendererResizeHealsStranding reproduces the reported corruption: a
// progress redraw computed at the old width is consumed after the emulator has
// reflowed to a narrower one, the erase under-shoots, and the old divider is
// stranded above the new block. The settled resize must heal it, because
// repaintFull rebuilds the viewport instead of locating the stale frame.
func TestInlineRendererResizeHealsStranding(t *testing.T) {
	t.Parallel()

	v := newVT(40, 10)
	r := newTestInline(v)
	r.setLive([]string{strings.Repeat(ruleChar, 39), "❯ x", "ctx"}, 1, 2)

	// the terminal narrows and reflows while our next draw is still computed at
	// the old width (queued output, a signal not yet delivered)
	v.setSize(20, 10)
	r.t.sizeFn = func() (int, int, error) { return 40, 10, nil } // stale read
	r.setLive([]string{strings.Repeat(ruleChar, 39), "❯ x", "ctx"}, 1, 2)

	var stranded int
	for i := 0; i < v.h; i++ {
		if l := v.Line(i); l != "" && strings.Trim(l, ruleChar) == "" {
			stranded++
		}
	}
	// one stranded row of the old divider (the erase stopped one row short),
	// two from the new divider wrapping on the narrower grid
	require.Equal(t, 3, stranded, "the stale erase strands the reflowed divider")

	r.t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }
	r.resize() // the settled SIGWINCH

	var dividers int
	for i := 0; i < v.h; i++ {
		if l := v.Line(i); l != "" && strings.Trim(l, ruleChar) == "" {
			dividers++
			assert.Equal(t, 19, displayWidth(l), "redrawn at the settled live width")
		}
	}
	assert.Equal(t, 1, dividers, "the viewport repaint heals the stranding")
	assert.Equal(t, "❯ x", v.Line(1))
	assert.Equal(t, "ctx", v.Line(2))
}
