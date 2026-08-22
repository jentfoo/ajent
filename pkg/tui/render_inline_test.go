package tui

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestInline returns an inline renderer painting into v.
func newTestInline(v *vt) *inlineRenderer {
	return &inlineRenderer{t: &termState{out: v, fd: -1, width: v.w, height: v.h}}
}

// recWriter captures raw escape bytes so a test can assert on what a renderer
// actually emits rather than only on the screen it produces.
type recWriter struct {
	b *strings.Builder
}

func (w recWriter) Write(p []byte) (int, error) {
	n, _ := w.b.Write(p)
	return n, nil
}

// hasCursorTo reports whether s contains an absolute cursor address. The only
// finals that position absolutely are 'H' and 'f', with or without parameters;
// relative motion is 'A' (up) plus \r, and inline's other sequences end in 'h',
// 'l', 'J', 'K' or 'm'. Inline mode must never emit an absolute address.
func hasCursorTo(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] != esc[0] || s[i+1] != '[' {
			continue
		}
		j := i + 2
		for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == ';' || s[j] == '?') {
			j++
		}
		if j < len(s) && (s[j] == 'H' || s[j] == 'f') {
			return true
		}
	}
	return false
}

// TestInlineNeverAbsolute pins invariant 1 as an emitted-stream property: inline
// mode never issues an absolute cursor address, even across a resize (both narrow
// and wide). Every prior repaint design — attempt 3's absolute cursorTo pass and
// the relative climb that followed it — corrupted scrollback on real terminals
// because we cannot know where our committed rows sit after emulator reflow. The
// only anchor is the parked cursor, which the terminal tracks for us; everything
// else must be relative.
func TestInlineNeverAbsolute(t *testing.T) {
	t.Parallel()

	for _, width := range []int{25, 80} { // narrow and wide both stay relative
		var buf strings.Builder
		r := &inlineRenderer{t: &termState{out: recWriter{&buf}, fd: -1, width: 40, height: 12}}
		for range 15 {
			r.commit([]histLine{{text: "line", flow: flowReflow}})
		}
		r.setLive([]string{"❯ x", "ctx"}, 0, 2)

		buf.Reset()
		r.t.sizeFn = func() (int, int, error) { return width, 12, nil }
		r.resize()
		r.setLive([]string{"❯ x", "ctx"}, 0, 2)

		assert.False(t, hasCursorTo(buf.String()),
			"width %d: a resize must never address an absolute row; the parked cursor is the only anchor", width)
	}
}

// assertParked asserts the cursor sits on the live block's first row: every
// draw parks there so the next erase needs no row maths.
func assertParked(t *testing.T, v *vt, want int) {
	t.Helper()
	assert.Equal(t, want, v.row)
	assert.Equal(t, 0, v.col)
}

// TestInlineContaminatedRowParks drives live rows carrying the escapes caller
// text may hold. Unsanitized, each moves the cursor in ways no row count
// predicted, so the park lands inside the block and the next erase strands
// its top row.
func TestInlineContaminatedRowParks(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, row string }{
		{name: "escape_row_parks_on_the_top_row", row: "sub-1 \x1b[2B boom"},
		{name: "index_escape_row_parks_on_the_top_row", row: "sub-1\x1bD boom"},
		{name: "c1_csi_row_parks_on_the_top_row", row: "sub-1 \u009b2B boom"},
		{name: "truncated_escape_does_not_eat_the_park", row: "sub-1 \x1b[12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newVT(40, 8)
			r := newTestInline(v)
			r.commit([]histLine{{text: "hist"}})
			top := v.row
			// caret off the contaminated row: paintCaret's cell rebuild would
			// strip the escape itself, hiding the bug from the raw emission
			r.setLive([]string{tc.row, "ctx"}, 1, 1)
			assertParked(t, v, top)
		})
	}
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
	t.Run("wrapped_lines_left_for_the_terminal", func(t *testing.T) {
		v := newVT(10, 8)
		r := newTestInline(v)
		r.commit([]histLine{{text: "• aaaa bbbb", flow: flowWrap}})

		// no hanging indent from us: the terminal wraps flush-left, so it can
		// reflow the line on resize and copy it as one logical line
		assert.Equal(t, "• aaaa bbb", v.Line(0))
		assert.Equal(t, "b", v.Line(1))
	})
	t.Run("structural_rows_never_fill_the_last_column", func(t *testing.T) {
		v := newVT(20, 8)
		r := newTestInline(v)
		r.commit([]histLine{{rule: true}})

		// a committed row that fills the last column enters the deferred-wrap
		// state, which emulators reflow inconsistently — same precaution as
		// live_rows_never_fill_the_last_column
		assert.Equal(t, 19, displayWidth(v.Line(0)), "a committed rule stays off the last column")
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

// TestInlineUnalignedFlowReflowsOnWiden pins that flowWrap content with nothing to
// align (hangWidth == 0: diffs, tool output) is emitted as a single logical line so
// the emulator reflows it on resize. This is what makes committed structural rows
// come back in full form when the window widens — without our ever rewriting them.
func TestInlineUnalignedFlowReflowsOnWiden(t *testing.T) {
	t.Parallel()

	line := "146 + Level 148: Drifting mist curled around bridge arches where moss grew thick and deep green now"
	require.Zero(t, hangWidth(line), "a diff line has nothing to align")

	v := newVT(40, 10)
	r := newTestInline(v)
	r.t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }
	r.commit([]histLine{{text: line, flow: flowWrap}})

	// narrow: the emulator wrapped it into more than one row; nothing was hard-broken by us
	assert.True(t, strings.HasPrefix(v.Line(0), "146 + Level 148:"), v.Line(0))
	require.NotEmpty(t, strings.TrimSpace(v.Line(1)),
		"the long line must wrap into several terminal rows at w=40")

	// widen: the emulator rejoins that one logical line into its full form. We only
	// resize — never redraw committed history — so whatever is on screen is exactly
	// what the emulator's own reflow made of our single emitted line.
	v.setSize(120, 4) // wide enough that the whole line fits on one row
	r.resize()

	var rejoined []string // collect non-empty rows (no live block was drawn)
	for i := range v.h {
		if s := strings.TrimRight(v.Line(i), " "); s != "" {
			rejoined = append(rejoined, s)
		}
	}
	assert.Equal(t, []string{line}, rejoined,
		"widening restores the committed line in full form on one row")
}

// TestInlineAlignedFlowReflowsOnWiden pins that even flowWrap content whose
// continuation we used to indent (hangWidth > 0: code, lists, quotes) goes out as
// a single logical line: the terminal wraps it flush-left, reflows it on resize,
// and copies it whole. Hard-breaking it kept the alignment but froze the line at
// commit width — the corruption-seam the user sees when widening.
func TestInlineAlignedFlowReflowsOnWiden(t *testing.T) {
	t.Parallel()

	line := "    return fmt.Sprintf(\"a fairly long code line that overflows\", x)"
	require.Positive(t, hangWidth(line), "a code line leads with spaces")

	v := newVT(30, 10)
	r := newTestInline(v)
	r.t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }
	r.commit([]histLine{{text: line, flow: flowWrap}})

	// narrow: the emulator wrapped it flush-left; nothing was hard-broken by us
	assert.True(t, strings.HasPrefix(v.Line(0), "    return fmt.Sprintf"), v.Line(0))

	// widen: the emulator rejoins the one logical line into its full form
	v.setSize(100, 4)
	r.resize()

	var rejoined []string
	for i := range v.h {
		if s := strings.TrimRight(v.Line(i), " "); s != "" {
			rejoined = append(rejoined, s)
		}
	}
	assert.Equal(t, []string{line}, rejoined,
		"widening restores an indented line in full form, indent intact")
}

// TestInlineDiffSkipsUnchangedRows pins the row-level diff: a frame where
// only one row changed (a spinner tick, a caret move) rewrites just that row,
// so the wire bytes for the untouched rows vanish. The cursor walk and the
// park stay byte-identical to a full redraw.
func TestInlineDiffSkipsUnchangedRows(t *testing.T) {
	t.Parallel()

	var buf strings.Builder
	r := &inlineRenderer{t: &termState{out: recWriter{&buf}, fd: -1, width: 40, height: 12}}
	rows := []string{"draft text", "mid row", "third row", "status"}
	r.setLive(rows, 1, 2)
	require.Contains(t, buf.String(), "draft text")

	buf.Reset()
	tick := []string{"draft text", "mid row", "third row", "status tick"}
	r.setLive(tick, 1, 2)
	out := buf.String()
	assert.Contains(t, out, "status tick")
	assert.Contains(t, out, cursorUp(3), "the park still climbs the whole block")
	assert.NotContains(t, out, "draft", "the first row emits no bytes")
	assert.NotContains(t, out, "third row", "nor any unchanged row")
	assert.NotContains(t, out, eraseBelow, "no full erase when nothing moved rows")
	assert.Contains(t, out, eraseTail, "a rewritten row clears its own tail")
}

// TestInlineDiffFallsBackOnWidthChange: only the width can reflow rows the
// diff did not write, so a width change redraws the whole block.
func TestInlineDiffFallsBackOnWidthChange(t *testing.T) {
	t.Parallel()

	var buf strings.Builder
	r := &inlineRenderer{t: &termState{out: recWriter{&buf}, fd: -1, width: 40, height: 12}}
	r.setLive([]string{"draft text", "ctx"}, 1, 1)

	buf.Reset()
	r.t.width = 30
	r.setLive([]string{"draft text", "ctx"}, 1, 1)
	out := buf.String()
	assert.Contains(t, out, eraseBelow, "the whole block is erased and redrawn")
	assert.Contains(t, out, "draft text")
}

// TestInlineDiffFallsBackOnRowCountChange: the single erase-below used to
// cover a block that grew or shrank; the diff cannot, so it falls back.
func TestInlineDiffFallsBackOnRowCountChange(t *testing.T) {
	t.Parallel()

	var buf strings.Builder
	r := &inlineRenderer{t: &termState{out: recWriter{&buf}, fd: -1, width: 40, height: 12}}
	r.setLive([]string{"draft text", "ctx"}, 0, 2)

	buf.Reset()
	r.setLive([]string{"draft text", "extra row", "ctx"}, 0, 2)
	assert.Contains(t, buf.String(), eraseBelow)

	buf.Reset()
	r.setLive([]string{"draft text", "ctx"}, 0, 2)
	assert.Contains(t, buf.String(), eraseBelow)
}

// TestInlineAbortsFrameOnResizeSignal pins the pre-write gate: a resize
// signal arriving while a frame composed means the emulator is reflowing onto
// a different grid, and the park's row count was taken on the old one. The
// frame is abandoned; the burst's settled redraw repaints at its own size.
func TestInlineAbortsFrameOnResizeSignal(t *testing.T) {
	t.Parallel()

	v := newVT(40, 8)
	r := newTestInline(v)
	r.commit([]histLine{{text: "hist"}})
	r.setLive([]string{"old row", "ctx"}, 0, 1)
	before := v.Screen()
	top := v.row

	var gen atomic.Uint64
	r.sigGen = func() uint64 { return gen.Add(1) } // moves between capture and check
	r.setLive([]string{"fresh row", "ctx"}, 0, 1)
	assert.Equal(t, before, v.Screen(), "the frame never reached the terminal")
	assertParked(t, v, top)
	assert.NotContains(t, v.Screen(), "fresh row")

	r.sigGen = gen.Load // stable now
	r.setLive([]string{"fresh row", "ctx"}, 0, 1)
	assert.Contains(t, v.Screen(), "fresh row")
	assertParked(t, v, top)
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

		assert.Equal(t, []string{
			v.Line(0), v.Line(1), v.Line(2), v.Line(3), v.Line(4), v.Line(5),
		}, reflowed, "committed rows belong to the terminal, exactly like cat output")
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

// relayout lays histLine values out at width w via their own rows() method — the
// path alt mode uses to rebuild its viewport on resize. It lets a test compare
// "committed at A, re-laid at B" against "freshly rendered at B". Inline mode does
// not use it (it never re-renders committed history), but the retained-intent rule
// that makes alt's re-layout exact is shared and must hold for every producer.
func relayout(lines []histLine, w int) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.rows(w)...)
	}
	return out
}

// TestInlineRelayoutBakesNoWidth pins the retained-intent guarantee behind
// histLine.table and histLine.rule: nothing a producer commits may carry the
// width it was rendered at. Re-laying lines committed at one width must
// reproduce rendering them fresh at another, for every flow and kind.
func TestInlineRelayoutBakesNoWidth(t *testing.T) {
	t.Parallel()

	plain := NewTheme(ColorNone, DefaultPalette())
	srcs := []string{
		"prose that runs long enough to wrap across several columns",
		"- list item one\n- second list item with more text than fits at a narrow width",
		"> quoted line that is fairly long and should rewrap cleanly when narrowed",
		"```\nfunc f() int { return 1 }\n```",
		"| A | B |\n|---|---|\n| 1 | two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen |",
		"---",
	}
	for _, src := range srcs {
		t.Run(strings.Split(src, "\n")[0], func(t *testing.T) {
			const (
				a = 60
				b = 24
			)
			committedWide := renderMarkdown(plain, a, src)
			want := relayout(renderMarkdown(plain, b, src), b)
			assert.Equal(t, want, relayout(committedWide, b),
				"re-laying lines committed at %d must match a fresh render at %d", a, b)
		})
	}
}
