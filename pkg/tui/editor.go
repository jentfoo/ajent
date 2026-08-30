package tui

import (
	"slices"
	"strings"
)

const (
	promptFirst = "❯ "
	promptCont  = "  "
	inputHint   = "type a message"
)

// editor is the multi line input buffer. Positions are grapheme cluster indexes
// so the cursor moves over emoji and combining marks as a unit.
type editor struct {
	cells   []string
	pos     int
	history []string
	histIdx int    // len(history) while editing the live buffer
	stash   string // live buffer held aside during history browsing
}

// Value returns the current buffer contents.
func (e *editor) Value() string { return strings.Join(e.cells, "") }

// SetValue replaces the buffer and puts the cursor at the end.
func (e *editor) SetValue(s string) {
	e.cells = graphemesOf(s)
	e.pos = len(e.cells)
}

// SetValueAt replaces the buffer and puts the cursor on the cell holding byte
// offset off; a negative or past-the-end offset lands at the end.
func (e *editor) SetValueAt(s string, off int) {
	e.SetValue(s)
	if off < 0 {
		return
	}
	var b int
	for i, c := range e.cells {
		if off < b+len(c) { // an offset inside a cluster snaps to its start
			e.pos = i
			return
		}
		b += len(c)
	}
}

// Insert adds text at the cursor.
func (e *editor) Insert(s string) {
	add := graphemesOf(s)
	if len(add) == 0 {
		return
	}
	e.cells = slices.Insert(e.cells, e.pos, add...)
	e.pos += len(add)
}

// Submit records the buffer in history, clears it, and returns what was entered.
func (e *editor) Submit() string {
	v := e.Value()
	if trimmed := strings.TrimSpace(v); trimmed != "" {
		if len(e.history) == 0 || e.history[len(e.history)-1] != v {
			e.history = append(e.history, v)
		}
	}
	e.Clear()
	return v
}

func (e *editor) Clear() {
	e.cells = nil
	e.pos = 0
	e.histIdx = len(e.history)
	e.stash = ""
}

func (e *editor) Backspace() {
	if e.pos > 0 {
		e.cells = slices.Delete(e.cells, e.pos-1, e.pos)
		e.pos--
	}
}

func (e *editor) DeleteForward() {
	if e.pos < len(e.cells) {
		e.cells = slices.Delete(e.cells, e.pos, e.pos+1)
	}
}

func (e *editor) Left() {
	if e.pos > 0 {
		e.pos--
	}
}

func (e *editor) Right() {
	if e.pos < len(e.cells) {
		e.pos++
	}
}

func (e *editor) WordLeft() {
	e.pos = e.wordStart()
}

func (e *editor) WordRight() {
	i := e.pos
	for i < len(e.cells) && isSpaceCell(e.cells[i]) {
		i++
	}
	for i < len(e.cells) && !isSpaceCell(e.cells[i]) {
		i++
	}
	e.pos = i
}

// LineStart moves the caret to the first column of the current visual row (the
// wrapped line on screen), not the start of the whole buffer.
func (e *editor) LineStart(width int) {
	starts, ends := e.layout(width)
	e.pos = starts[e.displayRow(starts, ends)]
}

// LineEnd moves the caret to the last column of the current visual row.
func (e *editor) LineEnd(width int) {
	starts, ends := e.layout(width)
	e.pos = ends[e.displayRow(starts, ends)]
}

// KillToLineEnd clears from the cursor to the end of the current visual row
// (the wrapped line on screen), leaving the caret where it was. An already-
// empty line is removed like Delete: the newline in front of the caret goes,
// the line below joins at the caret, and the caret stays put. A trailing empty
// line joins the one above instead; a non-empty row tail is a no-op.
func (e *editor) KillToLineEnd(width int) {
	starts, ends := e.layout(width)
	r := e.displayRow(starts, ends)
	switch {
	case ends[r] > e.pos: // content after the cursor: clear to the row's end
		from := e.pos
		// Wrapping drops the space each row breaks on. Clearing a whole row leaves the
		// breaks on both sides of it, so consume the leading one to avoid a double
		// space where the rows join.
		if from == starts[r] && from > 0 && ends[r] < len(e.cells) &&
			e.cells[from-1] == " " && e.cells[ends[r]] == " " {
			from--
		}
		e.cells = slices.Delete(e.cells, from, ends[r]) // caret unmoved: content below joins at it
	case e.lineStart(e.pos) != e.pos: // text before the cursor: nothing to clear
	case e.pos < len(e.cells): // empty line: delete joins the line below at the caret
		e.cells = slices.Delete(e.cells, e.pos, e.pos+1) // cells[pos] is the newline
	case e.pos > 0: // trailing empty line: join the line above
		e.cells = slices.Delete(e.cells, e.pos-1, e.pos)
		e.pos = e.lineStart(e.pos - 1)
	}
}

// KillLine removes from the start of the current line to the cursor.
func (e *editor) KillLine() {
	start := e.lineStart(e.pos)
	e.cells = slices.Delete(e.cells, start, e.pos)
	e.pos = start
}

// KillWordBack removes the word before the cursor.
func (e *editor) KillWordBack() {
	start := e.wordStart()
	e.cells = slices.Delete(e.cells, start, e.pos)
	e.pos = start
}

// Up moves the caret to the visual row above at roughly the same column,
// reporting false when already on the first display row.
func (e *editor) Up(width int) bool {
	starts, ends := e.layout(width)
	r := e.displayRow(starts, ends)
	if r == 0 {
		return false
	}
	e.pos = min(starts[r-1]+(e.pos-starts[r]), ends[r-1])
	return true
}

// Down moves the caret to the visual row below at roughly the same column,
// reporting false when already on the last display row.
func (e *editor) Down(width int) bool {
	starts, ends := e.layout(width)
	r := e.displayRow(starts, ends)
	if r == len(ends)-1 {
		return false
	}
	e.pos = min(starts[r+1]+(e.pos-starts[r]), ends[r+1])
	return true
}

// HistoryPrev recalls an older entry, holding the live buffer aside.
func (e *editor) HistoryPrev() {
	if e.histIdx == 0 || len(e.history) == 0 {
		return
	} else if e.histIdx >= len(e.history) {
		e.histIdx = len(e.history)
		e.stash = e.Value()
	}
	e.histIdx--
	e.SetValue(e.history[e.histIdx])
}

// HistoryNext moves toward the live buffer, restoring it at the end.
func (e *editor) HistoryNext() {
	if e.histIdx >= len(e.history) {
		return
	}
	e.histIdx++
	if e.histIdx == len(e.history) {
		e.SetValue(e.stash)
		e.stash = ""
	} else {
		e.SetValue(e.history[e.histIdx])
	}
}

// lineStart returns the index just after the newline preceding pos.
func (e *editor) lineStart(pos int) int {
	for i := pos - 1; i >= 0; i-- {
		if e.cells[i] == "\n" {
			return i + 1
		}
	}
	return 0
}

// wordStart returns the index at the start of the word before the cursor.
func (e *editor) wordStart() int {
	i := e.pos
	for i > 0 && isSpaceCell(e.cells[i-1]) {
		i--
	}
	for i > 0 && !isSpaceCell(e.cells[i-1]) {
		i--
	}
	return i
}

func isSpaceCell(c string) bool {
	return c == " " || c == "\t" || c == "\n"
}

// prefixWidths returns the display widths of the prompt glyph and a continuation
// indent. A leading `!` shell marker replaces ❯ with nothing, so its width is 0.
func (e *editor) prefixWidths() (firstW, contW int) {
	marker := promptFirst
	if len(e.cells) > 0 && e.cells[0] == "!" { // the literal `!` serves as the marker
		marker = ""
	}
	return displayWidth(marker), displayWidth(promptCont)
}

// layout returns the buffer's visual rows as cell ranges [starts[k]:ends[k]], laid
// out at width by the same word-wrap rules inputView renders with. A leading `!`
// shell marker occupies row 0 without a glyph, so it wraps differently.
func (e *editor) layout(width int) (starts, ends []int) {
	firstW, contW := e.prefixWidths()
	cells := e.cells

	// Rows as cell ranges: row k renders cells[starts[k]:ends[k]]. Breaks fall on
	// word boundaries (trailing spaces dropped) so a word wraps whole to the next
	// line; explicit newlines and too-wide tokens still split.
	starts = append(starts, 0)
	i := 0
	for i < len(cells) {
		prefixW := firstW
		if len(starts) > 1 { // continuation rows indent by two
			prefixW = contW
		}
		rowStart := i
		end, lineW := rowStart, prefixW
		lastSpace := -1 // last wrap-able space cell index in this row
		overflow := false
		for end < len(cells) && cells[end] != "\n" {
			w := displayWidth(cells[end])
			if lineW+w > width {
				overflow = true
				break
			}
			if cells[end] == " " {
				lastSpace = end
			}
			lineW += w
			end++
		}

		var next int
		switch {
		case end < len(cells) && cells[end] == "\n":
			ends = append(ends, end)
			next = end + 1 // explicit newline ends the row; the \n is not rendered
		case overflow && lastSpace >= 0:
			// wrap at the trailing space: drop it and start the next word fresh
			ends = append(ends, lastSpace)
			next = lastSpace + 1
		default:
			if overflow { // unbreakable token wider than a row: hard-split, always advancing
				end = max(end, rowStart+1)
			}
			ends = append(ends, end)
			next = end
		}
		starts = append(starts, next)
		i = next
	}
	// An empty buffer or a trailing newline still needs one row for the caret:
	// the initial starts[0] already anchors it, only an end is missing.
	if len(cells) == 0 && len(ends) == 0 {
		ends = append(ends, 0)
	} else if cells[len(cells)-1] == "\n" {
		starts = append(starts, len(cells))
		ends = append(ends, len(cells))
	}
	return starts, ends
}

// displayRow returns the index of the visual row containing pos (first match,
// matching inputView's caret mapping).
func (e *editor) displayRow(starts, ends []int) int {
	for k := range starts {
		if e.pos >= starts[k] && e.pos <= ends[k] {
			return k
		}
	}
	return 0
}

// inputView lays the editor out into display rows of at most width columns,
// wrapping on word boundaries so a word is never split across lines (only an
// unbroken token wider than a full row hard-splits), and returns the zero based
// cursor offset within those rows. Wrapping is purely visual: Value() is
// untouched, so submitted input gains no newlines.
func (e *editor) inputView(t Theme, width, maxRows int) (rows []string, curRow, curCol int) {
	firstW, contW := e.prefixWidths()
	shell := len(e.cells) > 0 && e.cells[0] == "!"
	starts, ends := e.layout(width)

	// Render each range: the first row carries the prompt glyph (or nothing for a
	// leading `!`), continuations are indented by two raw spaces.
	for k := 0; k < len(ends); k++ {
		s, en := starts[k], ends[k]
		var line strings.Builder
		switch {
		case k == 0 && shell:
			// the literal `!` in cells[0] is already content; no glyph needed
		case k == 0:
			line.WriteString(t.Prompt.Wrap(promptFirst))
		default:
			line.WriteString(promptCont)
		}
		for j := s; j < en; j++ {
			line.WriteString(e.cells[j])
		}
		if len(e.cells) == 0 && k == 0 { // empty buffer: dim hint on the first row
			line.WriteString(t.Dim.Wrap(truncateDisplay(inputHint, width-firstW)))
		}
		rows = append(rows, line.String())
	}

	// Map the caret to a display row and column.
	for k := 0; k < len(ends); k++ {
		s, en := starts[k], ends[k]
		if e.pos >= s && e.pos <= en {
			pw := firstW
			if k > 0 {
				pw = contW
			}
			col := pw
			for j := s; j < e.pos; j++ {
				col += displayWidth(e.cells[j])
			}
			curRow, curCol = k, col
			break
		}
	}

	if maxRows > 0 && len(rows) > maxRows {
		start := min(max(curRow-maxRows+1, 0), len(rows)-maxRows)
		rows = rows[start : start+maxRows]
		curRow -= start
	}
	return rows, curRow, curCol
}
