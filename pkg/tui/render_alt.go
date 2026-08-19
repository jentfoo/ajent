package tui

import "strings"

const (
	altScreenOn  = csi + "?1049h"
	altScreenOff = csi + "?1049l"
	clearScreen  = csi + "2J"
	// altLeave hands the main screen back, shared by suspend and close
	altLeave      = bracketedPasteOff + altScreenOff + showCursor
	scrollNoteTxt = "[scrolled] "
)

// altRenderer owns an alternate screen. It keeps every committed line and re-wraps
// them on demand, so a resize is a pure re-render and the whole session stays
// scrollable at any width, regardless of what the emulator does.
type altRenderer struct {
	t     *termState
	theme Theme

	lines   []histLine // the whole session, unwrapped
	wrapped []string   // lines rendered at wrapWidth
	wrapAt  int        // width wrapped is valid for

	live     []string
	caretRow int
	caretCol int

	offset int // rows scrolled back from the tail, zero follows new output
}

func (r *altRenderer) start(inFd int) error { return r.resume(inFd) }

func (r *altRenderer) resume(inFd int) error {
	if err := r.t.makeRaw(inFd); err != nil {
		return err
	}
	// same as inline: the caret is painted into its row, the terminal's cursor
	// stays hidden for the session
	r.t.write(altScreenOn + clearScreen + bracketedPasteOn + hideCursor)
	return nil
}

func (r *altRenderer) size() (int, int) { return r.t.width, r.t.height }

// viewHeight is the number of history rows visible above the live block.
func (r *altRenderer) viewHeight() int {
	return max(r.t.height-len(r.live), 1)
}

// rows returns the whole session laid out at the current width, rebuilding the
// cache when the width changed.
func (r *altRenderer) rows() []string {
	if r.wrapAt != r.t.width {
		r.wrapAt = r.t.width
		r.wrapped = r.wrapped[:0]
		for _, l := range r.lines {
			r.wrapped = append(r.wrapped, l.rows(r.t.width)...)
		}
	}
	return r.wrapped
}

func (r *altRenderer) commit(lines []histLine) {
	before := len(r.rows()) // also brings the cache up to the current width
	r.lines = append(r.lines, lines...)
	for _, l := range lines {
		r.wrapped = append(r.wrapped, l.rows(r.t.width)...)
	}
	if r.offset > 0 {
		// hold the reader's position instead of yanking them to the tail
		r.offset += len(r.wrapped) - before
		r.clampOffset()
	}
	r.render()
}

// clearHistory drops every retained line so a rewind can redraw just the
// current session state; alt owns its scrollback and repaints from scratch.
func (r *altRenderer) clearHistory() {
	r.lines = nil
	r.wrapped = nil
	r.wrapAt = 0
	r.offset = 0
}

func (r *altRenderer) setLive(rows []string, caretRow, caretCol int) {
	r.live, r.caretRow, r.caretCol = rows, caretRow, caretCol
	r.render()
}

func (r *altRenderer) resize() {
	r.t.refreshSize()
	r.wrapAt = 0 // force a re-wrap at the new width
	r.clampOffset()
	r.render()
}

// probe is a no-op: alt re-paints every cell it owns on each frame, so a draw
// that raced a reflow is fully repaired by the next one — no barrier needed.
func (r *altRenderer) probe() {}

// scroll moves the viewport, positive scrolls back toward older output.
func (r *altRenderer) scroll(lines int) bool {
	before := r.offset
	r.offset += lines
	r.clampOffset()
	if r.offset != before {
		r.render()
	}
	return true
}

func (r *altRenderer) clampOffset() {
	r.offset = min(max(r.offset, 0), max(len(r.rows())-r.viewHeight(), 0))
}

// render paints a whole frame: the visible slice of history, then the live block.
func (r *altRenderer) render() {
	all := r.rows()
	view := r.viewHeight()
	end := max(len(all)-r.offset, 0)
	start := max(end-view, 0)
	visible := all[start:end]

	var b strings.Builder
	b.WriteString(beginSync)
	b.WriteString(hideCursor)
	// history is bottom aligned so new output appears just above the input
	blank := view - len(visible)
	for i := range view {
		b.WriteString(cursorTo(1+i, 1) + eraseLine)
		if i >= blank {
			b.WriteString(truncateDisplay(visible[i-blank], r.t.width))
		}
	}
	for i, row := range r.live {
		row := row
		if r.offset > 0 && i == len(r.live)-1 {
			row = r.scrollNote() + row
		}
		row = truncateDisplay(sanitizeRow(row), r.t.width)
		if i == r.caretRow {
			row = paintCaret(row, r.caretCol, r.t.width)
		}
		b.WriteString(cursorTo(view+1+i, 1) + eraseLine + row)
	}
	b.WriteString(endSync)
	r.t.write(b.String())
}

// scrollNote marks the status line while the viewport is held back.
func (r *altRenderer) scrollNote() string {
	return r.theme.Dim.Wrap(scrollNoteTxt)
}

// suspend hands the main screen back, keeping the session so resume can repaint it.
func (r *altRenderer) suspend(inFd int) {
	r.t.write(altLeave)
	r.t.restore(inFd)
}

// close leaves the alternate screen and replays the session onto the main screen,
// so the transcript lands in the terminal's scrollback instead of vanishing.
func (r *altRenderer) close(inFd int) {
	r.t.write(altLeave)
	var b strings.Builder
	for _, l := range r.lines {
		for _, row := range l.rows(r.t.width) {
			b.WriteString(truncateDisplay(row, r.t.width))
			b.WriteString("\r\n")
		}
	}
	r.t.write(b.String())
	r.t.restore(inFd)
}
