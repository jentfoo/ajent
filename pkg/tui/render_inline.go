package tui

import "strings"

// inlineRenderer writes history straight to the main screen and redraws the live
// block immediately below it. It never positions the cursor into history, so the
// terminal keeps full ownership of wrapping, reflow and scrollback. All cursor
// motion is relative to the caret, which makes it immune to scrolling.
type inlineRenderer struct {
	t *termState

	hist []histLine // everything committed; a width change repaints the viewport from it

	live     []string // the block as last drawn
	caretRow int      // caret's index within live, not a terminal row offset
	caretCol int
	drawn    bool

	// Geometry of the last actual draw, so the block can be found again after the
	// terminal reflows it. Widths are what was emitted (already truncated), which
	// is not the same as the width of the live strings. blockTop is the screen
	// row the block starts at when a repaintFull placed it (negative when the
	// block is taller than the screen and its top scrolled off); a plain draw
	// leaves it zero, meaning no cap. frameW is the width the frame was drawn
	// at: size() re-reads the terminal size on every call, so a width change
	// can only be detected against this, never against t.width.
	drawnW   []int
	blockTop int
	frameW   int
}

func (r *inlineRenderer) start(inFd int) error { return r.resume(inFd) }

func (r *inlineRenderer) resume(inFd int) error {
	if err := r.t.makeRaw(inFd); err != nil {
		return err
	}
	r.t.write(bracketedPasteOn)
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

// caretOffset is how many terminal rows sit above the caret right now, derived
// from the geometry actually drawn and the current width. After the terminal
// reflows a resized block those rows have multiplied, and this finds the top of
// the block again without the renderer having to track the reflow itself.
func (r *inlineRenderer) caretOffset() int {
	w := r.t.width
	if w <= 0 {
		return r.caretRow
	}
	var above int
	for i := 0; i < r.caretRow && i < len(r.drawnW); i++ {
		above += rowsForWidth(r.drawnW[i], w)
	}
	off := above + r.caretCol/w
	if r.blockTop < 0 {
		off = max(off+r.blockTop, 0) // rows above the screen cannot be erased
	}
	return off
}

// eraseLive returns to the first row of the live block and clears it along with
// everything below, leaving the cursor there.
func (r *inlineRenderer) eraseLive() string {
	if !r.drawn {
		return ""
	}
	return cursorUp(r.caretOffset()) + "\r" + eraseBelow
}

// drawLive writes the block from the current row and leaves the cursor on the caret.
func (r *inlineRenderer) drawLive() string {
	r.frameW = r.t.width
	if len(r.live) == 0 {
		r.drawnW = nil
		return ""
	}
	var b strings.Builder
	r.drawnW = make([]int, len(r.live))
	for i, row := range r.live {
		if i > 0 {
			b.WriteString("\r\n")
		}
		row = truncateDisplay(row, r.liveWidth())
		r.drawnW[i] = displayWidth(row)
		b.WriteString(row)
	}
	b.WriteString(cursorUp(len(r.live) - 1 - r.caretRow))
	b.WriteString("\r")
	b.WriteString(cursorRight(r.caretCol))
	r.drawn = true
	r.blockTop = 0 // the block's absolute position is unknown; no erase cap
	return b.String()
}

// commit writes history above the live block, laying each line out according to
// its flow.
func (r *inlineRenderer) commit(lines []histLine) {
	if len(lines) == 0 {
		return
	}
	r.hist = append(r.hist, lines...) // retained so a width change can repaint
	r.t.refreshSize()                 // the erase below must use the width the block is reflowed at
	var b strings.Builder
	b.WriteString(beginSync)
	b.WriteString(hideCursor)
	b.WriteString(r.eraseLive())
	for _, l := range lines {
		switch {
		case l.table != nil:
			for _, row := range layoutTable(l.table, r.t.width) {
				b.WriteString(row)
				b.WriteString("\r\n")
			}
		case l.flow == flowWrap:
			for _, row := range wrapLine(l.text, r.t.width) {
				b.WriteString(row)
				b.WriteString("\r\n")
			}
		case l.flow == flowClip:
			b.WriteString(truncateDisplay(l.text, r.t.width))
			b.WriteString("\r\n")
		default:
			b.WriteString(l.text)
			b.WriteString("\r\n")
		}
	}
	b.WriteString(r.drawLive())
	b.WriteString(showCursor)
	b.WriteString(endSync)
	r.t.write(b.String())
}

// clearHistory clears the live block; committed scrollback belongs to the
// terminal in this mode and cannot be erased. The UI resets its own buffers.
func (r *inlineRenderer) clearHistory() {
	r.t.write(r.eraseLive())
	r.hist = nil
	r.live, r.drawnW = nil, nil
	r.drawn = false
}

func (r *inlineRenderer) setLive(rows []string, caretRow, caretCol int) {
	r.t.refreshSize() // same reason as commit: never erase against a stale width
	var b strings.Builder
	b.WriteString(beginSync)
	b.WriteString(hideCursor)
	b.WriteString(r.eraseLive())
	r.live, r.caretRow, r.caretCol = rows, caretRow, caretCol
	b.WriteString(r.drawLive())
	b.WriteString(showCursor)
	b.WriteString(endSync)
	r.t.write(b.String())
}

// resize picks up the new terminal size. A width change means the emulator
// reflowed the grid — possibly including bytes drawn before the change but
// consumed after it — so the previous frame can no longer be located by row
// math. Instead of locating it, the viewport is repainted from retained
// history, which also heals whatever a racing draw stranded. A height-only
// change needs nothing: the caret math is height independent.
func (r *inlineRenderer) resize() {
	r.t.refreshSize()
	if r.t.width <= 0 || r.t.width == r.frameW || (len(r.hist) == 0 && !r.drawn) {
		return
	}
	r.repaintFull()
}

// repaintFull rebuilds the whole viewport at the current width: the tail of
// retained history followed by the live block, every row erased and rewritten
// from the top of the screen, so no erase ever has to find the previous frame.
// Terminal scrollback above the screen is untouched; rows that no longer fit
// are simply not painted (the emulator keeps them in scrollback).
func (r *inlineRenderer) repaintFull() {
	w := r.liveWidth()
	var rows []string
	for _, l := range r.hist {
		for _, row := range l.rows(w) {
			rows = append(rows, truncateDisplay(row, w))
		}
	}
	histRows := len(rows)
	r.drawnW = make([]int, len(r.live))
	for i, row := range r.live {
		row = truncateDisplay(row, w)
		r.drawnW[i] = displayWidth(row)
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return
	}

	start := max(len(rows)-r.t.height, 0)
	r.blockTop = histRows - start
	r.drawn = len(r.live) > 0
	r.frameW = r.t.width

	var b strings.Builder
	b.WriteString(beginSync)
	b.WriteString(hideCursor)
	for i, row := range rows[start:] {
		b.WriteString(cursorTo(1+i, 1))
		b.WriteString(eraseLine)
		b.WriteString(row)
	}
	if below := len(rows) - start + 1; below <= r.t.height {
		b.WriteString(cursorTo(below, 1)) // clear whatever the old frame left below
		b.WriteString(eraseBelow)
	}
	caret := min(max(r.blockTop+r.caretRow, 0), r.t.height-1)
	b.WriteString(cursorTo(caret+1, min(r.caretCol, w-1)+1))
	b.WriteString(showCursor)
	b.WriteString(endSync)
	r.t.write(b.String())
}

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

// rowsFor returns how many terminal rows a line occupies at the given width.
func rowsFor(line string, width int) int {
	return rowsForWidth(displayWidth(line), width)
}

// rowsForWidth returns how many terminal rows a run of w columns occupies.
func rowsForWidth(w, width int) int {
	if width <= 0 || w == 0 {
		return 1
	}
	return (w + width - 1) / width
}
