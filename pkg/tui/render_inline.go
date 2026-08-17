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
// reflow (widening pulls scrollback back; narrowing wraps), so any attempt to
// rewrite it would land on rows that are not where we expect. Three designs tried
// to repaint the visible screen from retained history — a full viewport redraw, an
// absolute cursorTo pass, and a relative climb bounded by owned rows — and each
// surfaced new corruption on real terminals (scrolled-up or widened). Instead,
// committed lines are emitted so that what can reflow natively does:
//
//   - every text line (`flowReflow` and `flowWrap` alike: prose, code, lists,
//     quotes, diffs, tool output) goes out as one logical line, exactly like
//     cat. The terminal wraps it and reflows it on resize in both directions,
//     and selections carry no fake continuation indents — a hard break from us
//     would freeze the line at commit width and fragment copies.
//   - only genuinely two-dimensional content (tables, rules) is laid out at
//     commit width and keeps it; wrapping a table would garble it outright.
//
// The live block, whose top is the parked cursor the terminal tracks through
// reflow, is always redrawn at the current width. Alt mode exists for full resize
// fidelity: it owns a viewport and re-lays everything from retained lines.
type inlineRenderer struct {
	t *termState

	live     []string // the block as last drawn
	caretRow int      // caret's index within live, never a terminal row offset
	caretCol int
	drawn    bool
}

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
// and — worse — erase against a stale width, under-shooting the top of the
// reflowed block and stranding its rows on screen.
func (r *inlineRenderer) size() (int, int) {
	r.t.refreshSize()
	return r.t.width, r.t.height
}

// liveWidth is how wide a live row may be drawn. One column short of the
// terminal on purpose: a row that fills the last column leaves the cursor in the
// deferred-wrap state, and emulators disagree on whether the line is then marked
// as continued — which decides whether a resize reflows it into the next row or
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
// a resize racing the draw — the failures that used to strand a divider, once
// per miss, compounding. A reflow leaves the cursor on the first cell of its
// logical line, which is the cell we parked on.
func (r *inlineRenderer) eraseLive() string {
	if !r.drawn {
		return ""
	}
	return "\r" + eraseBelow
}

// drawLive writes the block from the current row, then parks the cursor back on
// the block's first row for the next erase. The caret is drawn into its row
// rather than left under the terminal's cursor (paintCaret).
func (r *inlineRenderer) drawLive() string {
	if len(r.live) == 0 {
		return ""
	}
	var b strings.Builder
	widths := make([]int, len(r.live))
	for i, row := range r.live {
		if i > 0 {
			b.WriteString("\r\n")
		}
		// oneLine so a row carrying a newline cannot silently become two rows
		row = truncateDisplay(oneLine(row), r.liveWidth())
		if i == r.caretRow {
			row = paintCaret(row, r.caretCol, r.liveWidth())
		}
		widths[i] = displayWidth(row)
		b.WriteString(row)
	}
	// Park on the block's first row, ready for the next erase. This counts only
	// rows being written right now, at the width in force right now — never how
	// the emulator reflowed something written earlier, which is the thing no
	// model can get right. The size is re-read first so a resize landing
	// mid-frame is accounted for before the count is taken.
	r.t.refreshSize()
	var rows int
	for _, w := range widths {
		rows += rowsForWidth(w, r.t.width)
	}
	b.WriteString(cursorUp(rows - 1))
	b.WriteString("\r")
	r.drawn = true
	return b.String()
}

// commit writes history above the live block, laying each line out according to
// its flow.
func (r *inlineRenderer) commit(lines []histLine) {
	if len(lines) == 0 {
		return
	}
	r.t.refreshSize() // the erase below must use the width the block is reflowed at
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
		case l.table != nil || l.rule:
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
	b.WriteString(r.drawLive())
	b.WriteString(endSync)
	r.t.write(b.String())
}

// clearHistory clears the live block; committed scrollback belongs to the
// terminal in this mode and cannot be erased. The UI resets its own buffers.
func (r *inlineRenderer) clearHistory() {
	r.t.write(r.eraseLive())
	r.live = nil
	r.drawn = false
}

func (r *inlineRenderer) setLive(rows []string, caretRow, caretCol int) {
	r.t.refreshSize() // same reason as commit: never erase against a stale width
	var b strings.Builder
	b.WriteString(beginSync)
	b.WriteString(hideCursor)
	b.WriteString(r.eraseLive())
	// clamp: drawLive paints the caret into live[caretRow], so an out of range
	// caret would panic or silently land on the wrong row
	r.live, r.caretCol = rows, caretCol
	r.caretRow = min(max(caretRow, 0), max(len(rows)-1, 0))
	b.WriteString(r.drawLive())
	b.WriteString(endSync)
	r.t.write(b.String())
}

// resize picks up the new terminal size; nothing needs redrawing here. The next
// ordinary draw erases from the cursor parked on the block's first row, which
// the reflow carried along with its cell, so no size is baked into the erase at
// all. Committed lines are the terminal's, exactly like cat output: they reflow
// however the emulator reflows them and are never re-rendered.
func (r *inlineRenderer) resize() { r.t.refreshSize() }

// probe emits a status query whose reply (decoded as keyStatusReport) proves
// the terminal has processed everything sent before it — including the resize
// reflow a settled burst is about to draw behind. The gate narrows the
// mid-reflow draw window; this closes it.
func (r *inlineRenderer) probe() { r.t.write(statusQuery) }

// scroll is the terminal's job in this mode.
func (r *inlineRenderer) scroll(int) bool { return false }

// suspend clears the live block and leaves the cursor below it, so a shell prompt
// or another program lands cleanly.
func (r *inlineRenderer) suspend(inFd int) {
	r.t.write(r.eraseLive() + bracketedPasteOff + showCursor)
	r.drawn = false
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
