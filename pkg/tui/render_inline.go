package tui

import "strings"

// inlineRenderer writes history straight to the main screen and redraws the live
// block immediately below it. Once a line is committed it belongs to the
// terminal, exactly like output from cat: it is never addressed, re-rendered or
// re-wrapped again, so the terminal keeps full ownership of wrapping, reflow and
// scrollback. All cursor motion is relative to the caret and confined to the
// live block, which makes it immune to scrolling.
//
// This means inline mode never rewrites committed history on a resize: we cannot
// know how many physical terminal rows our content occupies after an emulator
// reflow, so any attempt to rewrite it would land on rows that are not where we
// expect. Instead,
// committed lines are emitted so that what can reflow natively does:
//
//   - every text line (`flowReflow` and `flowWrap` alike: prose, code, lists,
//     quotes, diffs, tool output) goes out as one logical line, exactly like
//     cat. The terminal wraps it and reflows it on resize in both directions;
//     selections carry no fake continuation indents (a hard break from us
//     would freeze the line at commit width and fragment copies.
//   - only genuinely two-dimensional content (tables, rules) is laid out at
//     commit width and keeps it; wrapping a table would garble it outright.
//
// The live block, whose top is the parked cursor the terminal tracks through
// reflow, is always redrawn at the current width. Alt mode exists for full resize
// fidelity: it owns a viewport and re-lays everything from retained lines.
type inlineRenderer struct {
	t *termState

	live     []string // the block as last set
	caretRow int      // caret's index within live, never a terminal row offset
	caretCol int

	// base holds the bookkeeping for diffing against the frame on screen: a
	// skipped row writes only the newline that walks past it, so the cursor
	// walk, and with it the park, stays byte-identical to a full redraw.
	base baseline

	reanchored bool // a reflow lost the screen bottom; the next full draw pads back to it
	anchorRow  int  // reported park row (1-based) the pad is measured from

	// sigGen reads the UI's resize-signal generation, drawGen the generation the
	// live block is settled at. A frame is safe only while they agree. Both nil
	// never aborts.
	sigGen  func() uint64
	drawGen func() uint64
}

// baseline is the bookkeeping for row-diffing against the frame on screen.
// A width change reflows rows we did not write, a count change is what the full
// erase covers, and an invalidation means the block itself is gone; each makes
// the next draw fall back to today's full erase-and-redraw. The periodic full
// redraw bounds how long a stale row can linger in erasable territory.
type baseline struct {
	drawn     bool     // a frame reached the terminal (eraseLive depends on this)
	frames    int      // monotonic paint count; forces a full draw via diffFullEvery
	prev      []string // emitted rows (post-caret) of the last written frame
	prevWidth int      // width that frame was drawn at
	forceFull bool     // commit/suspend/clearHistory: the on-screen block is gone; reanchor: the pad needs the full path
}

// invalidate marks the on-screen block as unknown so the next draw repaints it
// whole. Used when committed history moved or another program owned the screen.
func (b *baseline) invalidate() {
	b.drawn, b.prev = false, nil
	b.forceFull = true
}

// diffFullEvery forces a whole-block redraw every so many frames. A stale row
// inside the live block is erasable territory that the next full frame heals;
// this bounds how long a diff miss can linger.
const diffFullEvery = 64

func (r *inlineRenderer) start(inFd int) error { return r.resume(inFd) }

func (r *inlineRenderer) resume(inFd int) error {
	if err := r.t.makeRaw(inFd); err != nil {
		return err
	}
	// the caret is painted into the block, so the terminal's own cursor stays
	// hidden for the whole session and never enters the row maths
	r.t.write(bracketedPasteOn + hideCursor)
	return nil
}

// size reports the terminal's current size, re-reading it rather than trusting
// the last SIGWINCH. Resize signals are debounced (resizeSettle) but repaints are
// not: a spinner tick or a progress row landing mid-burst would otherwise compose
// and, worse, erase against a stale width, under-shooting the top of the
// reflowed block and stranding its rows on screen.
func (r *inlineRenderer) size() (int, int) {
	r.t.refreshSize()
	return r.t.width, r.t.height
}

// liveWidth is how wide a live row may be drawn. One column short of the
// terminal on purpose: a row that fills the last column leaves the cursor in the
// deferred-wrap state, and emulators disagree on whether the line is then marked
// as continued; that decides whether a resize reflows it into the next row or
// not. Staying off the last column makes reflow predictable everywhere.
func (r *inlineRenderer) liveWidth() int { return max(r.t.width-1, 1) }

// eraseLive clears the live block and everything below it, leaving the cursor
// where the block starts.
//
// There is deliberately no row arithmetic here. Every draw parks the cursor on
// the block's first row, so erasing is only "return to the start of this line
// and clear downward": nothing counts how many rows the block occupies, so
// nothing can miscount them. That is what makes it immune to the emulator
// reflowing the block, to a glyph the terminal measures wider than we do, and to
// a resize racing the draw; those failures used to strand a divider once per miss,
// compounding. A reflow leaves the cursor on the first cell of its
// logical line, which is the cell we parked on.
func (r *inlineRenderer) eraseLive() string {
	if !r.base.drawn {
		return ""
	}
	return "\r" + eraseBelow
}

// setLive stores the block and paints one frame. The caret is drawn into its
// row rather than left under the terminal's cursor (paintCaret).
func (r *inlineRenderer) setLive(rows []string, caretRow, caretCol int) {
	r.t.refreshSize() // same reason as commit: never erase against a stale width
	// clamp: the caret is painted into live[caretRow], so an out of range
	// caret would panic or silently land on the wrong row
	r.live, r.caretCol = rows, caretCol
	r.caretRow = min(max(caretRow, 0), max(len(rows)-1, 0))
	r.paint()
}

// paint composes and writes one frame of the live block: the erase plus every
// row, or only the rows that changed when the previous frame is still
// comparable. The write is abandoned when a resize signal arrived while the
// frame composed, and the burst starting now redraws at its settled size.
func (r *inlineRenderer) paint() {
	gen := r.generation()
	diff := r.canDiff()
	var b strings.Builder
	b.WriteString(beginSync)
	b.WriteString(hideCursor)
	if !diff {
		b.WriteString(r.eraseLive())
		b.WriteString(r.anchorPad())
	}
	emitted := r.composeRows(&b, diff)
	b.WriteString(endSync)
	if r.stale(gen) {
		return
	}
	r.t.write(b.String())
	r.record(emitted, diff)
}

// canDiff reports whether unchanged rows may be skipped: a frame is on screen,
// drawn at this width with this many rows, and nothing invalidated it. A width
// change is the only thing that reflows rows we did not write; a count change
// is what the full erase was covering.
func (r *inlineRenderer) canDiff() bool {
	b := &r.base
	return b.drawn && !b.forceFull && b.prevWidth == r.t.width &&
		len(b.prev) == len(r.live) && b.frames%diffFullEvery != 0
}

// record updates the diff baseline once a frame reached the terminal.
func (r *inlineRenderer) record(emitted []string, diff bool) {
	b := &r.base
	b.drawn = true
	b.frames++
	if diff {
		b.prev = emitted
		return // a diff never follows a reanchor: it sets forceFull, which canDiff rejects
	}
	b.prev, b.prevWidth, b.forceFull = emitted, r.t.width, false
	// the pad landed with this frame. On the commit path none was emitted, but
	// the history that frame wrote moved the block, so the row it was measured
	// from no longer describes where the block sits
	r.reanchored = false
}

// composeRows writes the live rows into b and returns the emitted rows to
// record as the diff baseline. sanitizeRow keeps each row to exactly one
// terminal row whatever a caller passed; truncation and the caret stay as they
// were. The park counts only these rows at the width in force now.
func (r *inlineRenderer) composeRows(b *strings.Builder, diff bool) []string {
	if len(r.live) == 0 {
		return nil
	}
	width := r.liveWidth()
	emitted := make([]string, len(r.live))
	widths := make([]int, len(r.live))
	for i, row := range r.live {
		if i > 0 {
			b.WriteString("\r\n")
		}
		row = truncateDisplay(sanitizeRow(row), width)
		if i == r.caretRow {
			row = paintCaret(row, r.caretCol, width)
		}
		emitted[i], widths[i] = row, displayWidth(row)
		if !diff || i >= len(r.base.prev) || r.base.prev[i] != row {
			b.WriteString(row)
			if diff {
				b.WriteString(eraseTail) // a shorter replacement leaves no tail
			}
		}
	}
	// Park on the block's first row, ready for the next erase. The climb
	// counts only what this frame descended through: every row boundary is one
	// newline, and a written row adds its wrapped continuations (counted at the
	// width in force right now, re-read so a resize landing mid-frame is
	// accounted for before the count is taken). A skipped row never descended
	// anything beyond its boundary, so it counts as one regardless of how the
	// emulator has reflowed it since; the cursor returns exactly to where the
	// previous frame parked, which is the block's top.
	r.t.refreshSize()
	var climb int
	for i, w := range widths {
		if !diff || i >= len(r.base.prev) || r.base.prev[i] != emitted[i] {
			climb += rowsForWidth(w, r.t.width)
		} else {
			climb++ // skipped: only its newline boundary was crossed
		}
	}
	b.WriteString(cursorUp(climb - 1))
	b.WriteString("\r")
	return emitted
}

// generation is the resize-signal generation drawing is settled at, the
// baseline a frame is judged against.
func (r *inlineRenderer) generation() uint64 {
	if r.drawGen == nil {
		return 0
	}
	return r.drawGen()
}

// stale reports whether a signal arrived that gen has not been settled for:
// the baseline is the settled generation, never one captured as the frame starts.
func (r *inlineRenderer) stale(gen uint64) bool {
	return r.sigGen != nil && r.sigGen() != gen
}

// commit writes history above the live block, laying each line out according to
// its flow.
func (r *inlineRenderer) commit(lines []histLine) {
	if len(lines) == 0 {
		return
	}
	r.t.refreshSize() // the erase below must use the width the block is reflowed at
	gen := r.generation()
	var b strings.Builder
	b.WriteString(beginSync)
	b.WriteString(hideCursor)
	b.WriteString(r.eraseLive())
	emit := func(row string) {
		b.WriteString(row)
		b.WriteString("\r\n")
	}
	for _, l := range lines {
		switch {
		case l.structured():
			// structured content arrives as intent (markdown.go), not baked text;
			// rows() lays it out at the width in force, so a later re-lay at any
			// other width (alt mode) reproduces commit exactly. One column short
			// of the edge, the same precaution as live rows: a full-width row
			// enters the deferred-wrap state, which emulators reflow
			// inconsistently (a table border could fuse with the next row).
			for _, row := range l.rows(max(r.t.width-1, 1)) {
				emit(row)
			}
		default:
			// one logical line, left long on purpose: the terminal wraps it and
			// reflows it on resize. Hard-breaking here would freeze the line at
			// commit width and fragment selections with our continuation indents.
			emit(l.text)
		}
	}
	if r.stale(gen) {
		// A resize arrived while the frame composed. History must still land
		// (committed text reflows like any other), but the block half would park
		// by a count taken on the old grid. The erase went out with this frame,
		// so the next paint rebuilds the block from the current row.
		b.WriteString(endSync)
		r.t.write(b.String())
		r.reanchored = false // this frame's history moved the block; the row is stale
		r.base.invalidate()
		return
	}
	// the block moved down the screen behind the new history: the diff cannot
	// carry over
	emitted := r.composeRows(&b, false)
	b.WriteString(endSync)
	r.t.write(b.String())
	r.record(emitted, false)
}

// clearHistory clears the live block; committed scrollback belongs to the
// terminal in this mode and cannot be erased. The UI resets its own buffers.
func (r *inlineRenderer) clearHistory() {
	r.t.write(r.eraseLive())
	r.live = nil
	r.base.invalidate()
	r.reanchored = false // the block it was measured against is gone
}

// resize picks up the new terminal size; nothing needs redrawing here. The next
// ordinary draw erases from the cursor parked on the block's first row, which
// the reflow carried along with its cell, so no size is baked into the erase at
// all. Committed lines are the terminal's, exactly like cat output: they reflow
// however the emulator reflows them and are never re-rendered.
func (r *inlineRenderer) resize() { r.t.refreshSize() }

// probe asks the terminal where the cursor is and for a status reply; the
// replies prove the terminal processed the settled reflow. The status reply
// releases the barrier; the cursor reply measures where the reflow left the park.
func (r *inlineRenderer) probe() { r.t.write(cursorQuery + statusQuery) }

// reanchor takes the CPR-reported cursor row (1-based) and arms the next full
// draw to pad the block back to the screen bottom when the reflow left it
// ending above the last row. A fresh session (started false) owns the top and
// is left alone; a row of zero means no usable evidence. Every path clears the
// pad, since one left pending would be measured against a grid that has moved.
func (r *inlineRenderer) reanchor(row int, started bool) {
	r.reanchored, r.anchorRow = false, 0
	if !started || row <= 0 || len(r.live) == 0 {
		return
	}
	if row-1+len(r.live) >= r.t.height {
		return // the block already reaches the bottom
	}
	r.reanchored, r.anchorRow = true, row
	r.base.forceFull = true // a height-only change would otherwise take the diff path
}

// anchorPad drops the block to the screen bottom, in newlines so nothing is
// addressed. The count is measured against the frame this pad lands on —
// repaint recomposes the block in between — and from the reported park row, so
// it only fills the rows eraseLive just cleared and never scrolls. Pure: paint
// may still abandon the frame.
func (r *inlineRenderer) anchorPad() string {
	if !r.reanchored || len(r.live) == 0 {
		return ""
	}
	pad := r.t.height - len(r.live) - (r.anchorRow - 1)
	return strings.Repeat("\r\n", min(max(pad, 0), r.t.height))
}

func (r *inlineRenderer) setTheme(Theme) {} // inline draws no styled chrome of its own

func (r *inlineRenderer) query(seq string) { r.t.write(seq) }

// scroll is the terminal's job in this mode.
func (r *inlineRenderer) scroll(int) bool { return false }

// suspend clears the live block and leaves the cursor below it, so a shell prompt
// or another program lands cleanly.
func (r *inlineRenderer) suspend(inFd int) {
	r.t.write(r.eraseLive() + bracketedPasteOff + showCursor)
	r.base.invalidate()  // another program owned the screen
	r.reanchored = false // and moved the cursor: the recorded row is stale evidence
	r.t.restore(inFd)
}

func (r *inlineRenderer) close(inFd int) { r.suspend(inFd) }

// rowsForWidth returns how many terminal rows a run of w columns occupies.
func rowsForWidth(w, width int) int {
	if width <= 0 || w == 0 {
		return 1
	}
	return (w + width - 1) / width
}
