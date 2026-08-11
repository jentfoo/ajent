package tui

import "strings"

// inlineRenderer writes history straight to the main screen and redraws the live
// block immediately below it. It never positions the cursor into history, so the
// terminal keeps full ownership of wrapping, reflow and scrollback. All cursor
// motion is relative to the caret, which makes it immune to scrolling.
type inlineRenderer struct {
	t *termState

	live     []string // the block as last drawn
	caretRow int      // caret row offset within the block
	caretCol int
	drawn    bool
}

func (r *inlineRenderer) start(inFd int) error { return r.resume(inFd) }

func (r *inlineRenderer) resume(inFd int) error {
	if err := r.t.makeRaw(inFd); err != nil {
		return err
	}
	r.t.write(bracketedPasteOn)
	return nil
}

func (r *inlineRenderer) size() (int, int) { return r.t.width, r.t.height }

// eraseLive returns to the first row of the live block and clears it along with
// everything below, leaving the cursor there.
func (r *inlineRenderer) eraseLive() string {
	if !r.drawn {
		return ""
	}
	return cursorUp(r.caretRow) + "\r" + eraseBelow
}

// drawLive writes the block from the current row and leaves the cursor on the caret.
func (r *inlineRenderer) drawLive() string {
	if len(r.live) == 0 {
		return ""
	}
	var b strings.Builder
	for i, row := range r.live {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(truncateDisplay(row, r.t.width))
	}
	b.WriteString(cursorUp(len(r.live) - 1 - r.caretRow))
	b.WriteString("\r")
	b.WriteString(cursorRight(r.caretCol))
	r.drawn = true
	return b.String()
}

// commit writes history above the live block, laying each line out according to
// its flow.
func (r *inlineRenderer) commit(lines []histLine) {
	if len(lines) == 0 {
		return
	}
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
	r.live = nil
	r.drawn = false
}

func (r *inlineRenderer) setLive(rows []string, caretRow, caretCol int) {
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

// resize recomputes the caret position after a terminal width change.
func (r *inlineRenderer) resize() {
	old := r.t.width
	r.t.refreshSize()
	if !r.drawn || old == r.t.width || r.t.width <= 0 {
		return
	}
	var above int
	for i := 0; i < r.caretRow && i < len(r.live); i++ {
		above += rowsFor(r.live[i], r.t.width)
	}
	r.caretRow = above + r.caretCol/r.t.width
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
	if width <= 0 {
		return 1
	}
	w := displayWidth(line)
	if w == 0 {
		return 1
	}
	return (w + width - 1) / width
}
