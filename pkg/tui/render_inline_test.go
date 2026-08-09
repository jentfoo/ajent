package tui

import (
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

	t.Run("recomputes_caret_row_after_reflow", func(t *testing.T) {
		v := newVT(20, 8)
		r := newTestInline(v)
		// two input rows, caret on the second; the first is 12 columns wide
		r.setLive([]string{"❯ 0123456789", "  x", "ctx"}, 1, 3)

		r.t.sizeFn = func() (int, int, error) { return 6, 8, nil } // terminal narrowed
		r.resize()

		assert.Equal(t, 2, r.caretRow, "the 12 column row now occupies two rows")
	})
	t.Run("no_change_when_width_is_stable", func(t *testing.T) {
		v := newVT(20, 8)
		r := newTestInline(v)
		r.setLive([]string{"❯ a", "ctx"}, 0, 3)
		r.resize()
		assert.Equal(t, 0, r.caretRow)
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
