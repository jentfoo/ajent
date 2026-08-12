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

func (e *editor) LineStart() { e.pos = e.lineStart(e.pos) }
func (e *editor) LineEnd()   { e.pos = e.lineEnd(e.pos) }

// KillToLineEnd removes from the cursor to the end of the current line.
func (e *editor) KillToLineEnd() {
	e.cells = slices.Delete(e.cells, e.pos, e.lineEnd(e.pos))
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

// Up moves to the previous line, reporting false when already on the first line.
func (e *editor) Up() bool {
	start := e.lineStart(e.pos)
	if start == 0 {
		return false
	}
	col := e.pos - start
	prevStart := e.lineStart(start - 1)
	e.pos = min(prevStart+col, start-1)
	return true
}

// Down moves to the next line, reporting false when already on the last line.
func (e *editor) Down() bool {
	end := e.lineEnd(e.pos)
	if end == len(e.cells) {
		return false
	}
	col := e.pos - e.lineStart(e.pos)
	nextStart := end + 1
	e.pos = min(nextStart+col, e.lineEnd(nextStart))
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

// lineEnd returns the index of the newline at or after pos, or the buffer length.
func (e *editor) lineEnd(pos int) int {
	for i := pos; i < len(e.cells); i++ {
		if e.cells[i] == "\n" {
			return i
		}
	}
	return len(e.cells)
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

// inputView lays the editor out into display rows of at most width columns and
// returns the zero based cursor offset within those rows.
func (e *editor) inputView(t Theme, width, maxRows int) (rows []string, curRow, curCol int) {
	// A leading `!` marks a shell command: let it serve as the prompt glyph itself
	// rather than duplicating it beside ❯. Backspacing that first character returns
	// the marker to the ordinary prompt.
	shell := len(e.cells) > 0 && e.cells[0] == "!"
	marker, cont := promptFirst, promptCont
	if shell {
		marker = "" // the literal `!` in cells[0] is now the marker
	}
	first := t.Prompt.Wrap(marker)
	firstW, contW := displayWidth(marker), displayWidth(cont)

	var line strings.Builder
	line.WriteString(first)
	lineW := firstW
	newRow := func() {
		rows = append(rows, line.String())
		line.Reset()
		line.WriteString(cont)
		lineW = contW
	}
	for i := 0; i <= len(e.cells); i++ {
		if i < len(e.cells) && e.cells[i] != "\n" {
			if w := displayWidth(e.cells[i]); lineW+w > width {
				newRow()
			}
		}
		if i == e.pos {
			curRow, curCol = len(rows), lineW
		}
		if i == len(e.cells) {
			break
		} else if e.cells[i] == "\n" {
			newRow()
		} else {
			line.WriteString(e.cells[i])
			lineW += displayWidth(e.cells[i])
		}
	}
	if len(e.cells) == 0 {
		line.WriteString(t.Dim.Wrap(truncateDisplay(inputHint, width-firstW)))
	}
	rows = append(rows, line.String())

	if maxRows > 0 && len(rows) > maxRows {
		start := min(max(curRow-maxRows+1, 0), len(rows)-maxRows)
		rows = rows[start : start+maxRows]
		curRow -= start
	}
	return rows, curRow, curCol
}
