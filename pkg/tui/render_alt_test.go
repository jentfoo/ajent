package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestAlt returns an alt renderer painting into v.
func newTestAlt(v *vt) *altRenderer {
	return &altRenderer{
		t:     &termState{out: v, fd: -1, width: v.w, height: v.h},
		theme: NewTheme(ColorNone, DefaultPalette()),
	}
}

// commitText is a helper for committing plain prose lines.
func commitText(r *altRenderer, lines ...string) {
	hl := make([]histLine, len(lines))
	for i, l := range lines {
		hl[i] = histLine{text: l}
	}
	r.commit(hl)
}

func TestAltRendererRender(t *testing.T) {
	t.Parallel()

	t.Run("history_bottom_aligned_above_the_block", func(t *testing.T) {
		v := newVT(20, 6)
		r := newTestAlt(v)
		r.setLive([]string{"❯ ", "ctx"}, 0, 2)
		commitText(r, "one", "two")

		assert.Equal(t, "one", v.Line(2))
		assert.Equal(t, "two", v.Line(3))
		assert.Equal(t, "❯", v.Line(4), "block sits at the bottom")
		assert.Equal(t, "ctx", v.Line(5))
	})
	t.Run("older_output_falls_off_the_top", func(t *testing.T) {
		v := newVT(20, 5)
		r := newTestAlt(v)
		r.setLive([]string{"❯", "ctx"}, 0, 1)
		commitText(r, "a", "b", "c", "d", "e")

		assert.Equal(t, "c", v.Line(0))
		assert.Equal(t, "e", v.Line(2))
	})
}

func TestAltRendererResizeReflows(t *testing.T) {
	t.Parallel()

	const long = "the retry helper loops a fixed number of times"

	v := newVT(50, 6)
	r := newTestAlt(v)
	r.setLive([]string{"❯", "ctx"}, 0, 1)
	commitText(r, long)
	require.Equal(t, long, v.Line(3))

	narrow := newVT(24, 6)
	r.t.out, r.t.width, r.t.height = narrow, narrow.w, narrow.h
	r.resize()

	assert.Equal(t, "the retry helper loops a", narrow.Line(2))
	assert.Equal(t, "fixed number of times", narrow.Line(3), "the whole session re-wraps")

	t.Run("and_back_again", func(t *testing.T) {
		wide := newVT(60, 6)
		r.t.out, r.t.width, r.t.height = wide, wide.w, wide.h
		r.resize()
		assert.Equal(t, long, wide.Line(3), "widening restores the single row")
	})
}

func TestAltRendererScroll(t *testing.T) {
	t.Parallel()

	v := newVT(20, 5)
	r := newTestAlt(v)
	r.setLive([]string{"❯", "ctx"}, 0, 1)
	commitText(r, "a", "b", "c", "d", "e", "f")

	require.Equal(t, "f", v.Line(2), "following the tail")

	t.Run("scrolls_back", func(t *testing.T) {
		require.True(t, r.scroll(2))
		assert.Equal(t, "b", v.Line(0))
		assert.Equal(t, "d", v.Line(2))
	})
	t.Run("marks_the_status_line", func(t *testing.T) {
		assert.Contains(t, v.Line(4), "[scrolled]")
	})
	t.Run("clamped_at_the_oldest_line", func(t *testing.T) {
		r.scroll(100)
		assert.Equal(t, "a", v.Line(0))
	})
	t.Run("new_output_holds_the_position", func(t *testing.T) {
		commitText(r, "g")
		assert.Equal(t, "a", v.Line(0), "reader is not yanked to the tail")
	})
	t.Run("scrolls_forward_to_the_tail", func(t *testing.T) {
		r.scroll(-100)
		assert.Equal(t, "g", v.Line(2))
		assert.NotContains(t, v.Line(4), "[scrolled]")
	})
}

func TestAltRendererScrollHoldsWrappedOutput(t *testing.T) {
	t.Parallel()

	v := newVT(10, 6)
	r := newTestAlt(v)
	r.setLive([]string{"❯", "ctx"}, 0, 1)
	commitText(r, "a", "b", "c", "d", "e", "f")
	require.True(t, r.scroll(3))
	require.Equal(t, "a", v.Line(0))

	// one logical line, three rows: the offset moves by rows, not by lines
	commitText(r, "gg hh ii jj kk ll")
	assert.Equal(t, "a", v.Line(0), "reader stays on the same row")
}

func TestAltRendererLineFlow(t *testing.T) {
	t.Parallel()

	t.Run("wrapped_lines_keep_alignment", func(t *testing.T) {
		v := newVT(12, 5)
		r := newTestAlt(v)
		r.setLive([]string{"❯", "ctx"}, 0, 1)
		r.commit([]histLine{{text: "• alpha beta gamma", flow: flowWrap}})

		// history is bottom aligned above the two row block
		assert.Equal(t, "• alpha beta", v.Line(1))
		assert.Equal(t, "  gamma", v.Line(2))
	})
}

// TestAltRendererTableReflows guards resize behaviour for a committed table: it must
// re-lay out at the new width instead of staying truncated at its old one.
func TestAltRendererTableReflows(t *testing.T) {
	t.Parallel()

	plain := NewTheme(ColorNone, DefaultPalette())
	src := "| A | B |\n|---|---|\n| 1 | a long description that should wrap when the terminal narrows |"
	hl := renderMarkdown(plain, 60, src)[0]
	require.NotNil(t, hl.table)

	v := newVT(40, 10)
	r := newTestAlt(v)
	r.setLive([]string{"❯", "ctx"}, 1, 2)
	r.commit([]histLine{hl})
	wideRows := layoutTable(hl.table, r.t.width)

	narrow := newVT(20, 10)
	r.t.out, r.t.width, r.t.height = narrow, narrow.w, narrow.h
	r.resize()

	narrowRows := layoutTable(hl.table, r.t.width)
	require.Greater(t, len(narrowRows), len(wideRows), "narrowing should wrap the long cell")
	// content is not truncated: the table bottom border and the last wrapped cell
	// line are both on screen with their left borders intact.
	assert.Equal(t, narrowRows[len(narrowRows)-1], narrow.Line(v.h-3), "bottom border re-laid")
	var visible strings.Builder
	for i := 0; i < v.h; i++ {
		visible.WriteString(narrow.Line(i))
	}
	assert.Contains(t, visible.String(), "narrows", "long cell text survives the narrower width")
}

func TestAltRendererClose(t *testing.T) {
	t.Parallel()

	v := newVT(20, 6)
	r := newTestAlt(v)
	r.setLive([]string{"❯", "ctx"}, 0, 1)
	commitText(r, "kept one", "kept two")

	main := newVT(20, 6)
	r.t.out = main
	r.close(-1)

	assert.Equal(t, "kept one", main.Line(0), "transcript replayed onto the main screen")
	assert.Equal(t, "kept two", main.Line(1))
}

func TestAltRendererSuspend(t *testing.T) {
	t.Parallel()

	v := newVT(20, 6)
	r := newTestAlt(v)
	r.setLive([]string{"❯", "ctx"}, 0, 1)
	commitText(r, "before")
	require.Equal(t, "before", v.Line(3))

	var seq strings.Builder
	r.t.out = &seq
	r.suspend(-1)
	assert.Contains(t, seq.String(), altScreenOff)
	assert.Contains(t, seq.String(), bracketedPasteOff)

	t.Run("resume_retakes_and_repaints", func(t *testing.T) {
		seq.Reset()
		require.NoError(t, r.resume(-1))
		assert.Contains(t, seq.String(), altScreenOn)

		back := newVT(20, 6)
		r.t.out = back
		r.setLive([]string{"❯", "ctx"}, 0, 1)
		assert.Equal(t, "before", back.Line(3), "history survives the round trip")
	})
}
