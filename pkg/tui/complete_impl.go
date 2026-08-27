package tui

import (
	"slices"
	"strings"
)

// completionOverlay is the candidate list drawn above the editor, in either of
// two presentations. A menu is live while typing and owns ↑/↓/Enter/Tab; the
// Tab-driven form holds no selection, consumes no keys and is cleared by the
// next keystroke.
type completionOverlay struct {
	menu   bool
	items  []Completion
	start  int  // grapheme index the accepted Text replaces, up to the cursor
	cursor int  // highlighted index, menu only; what Tab accepts
	moved  bool // selection navigated via ↑/↓; only then does Enter accept it
}

// completionGap separates packed candidate columns.
const completionGap = 2

// open sets the candidates, keeping the cursor in range and dropping a stale
// selection when the start index changes.
func (o *completionOverlay) open(items []Completion, start int) {
	if o.start != start {
		o.moved = false
		o.cursor = 0
	}
	o.items = items
	o.start = start
	if o.cursor >= len(items) {
		o.cursor = 0
	}
}

// accept reports whether the overlay should consume a key rather than the editor.
func (o *completionOverlay) accept(k key) bool {
	switch k.typ {
	case keyEscape:
		return true
	case keyTab, keyUp, keyDown, keyEnter:
		return o.menu
	}
	return false
}

// key applies one overlay key, returning whether the editor should also receive
// it and whether the line should submit now.
func (o *completionOverlay) key(k key, u *UI) (consume, submit bool) {
	switch k.typ {
	case keyUp:
		o.move(-1)
		return true, false
	case keyDown:
		o.move(1)
		return true, false
	case keyTab:
		// accept now; a following Enter just submits
		if len(o.items) > 0 {
			o.applyCurrent(u)
			o.moved = false
		}
		return true, false
	case keyEnter:
		// accept and submit when ↑/↓ moved the selection, else submit as typed
		if o.moved && len(o.items) > 0 {
			o.applyCurrent(u)
			return true, true
		}
		return false, true
	case keyEscape:
		return true, false // close, do not submit
	}
	return false, false
}

// move steps the highlight by delta, wrapping.
func (o *completionOverlay) move(delta int) {
	if len(o.items) > 0 {
		o.cursor = wrapIndex(o.cursor+delta, len(o.items))
		o.moved = true
	}
}

// applyCurrent replaces the text from start to the cursor with the highlighted
// candidate's Text.
func (o *completionOverlay) applyCurrent(u *UI) {
	if len(o.items) > 0 {
		u.replaceSpan(o.start, o.items[o.cursor].Text)
	}
}

// rows renders the candidates within maxRows: one per line for a menu or when
// they carry detail text, else packed into columns.
func (o *completionOverlay) rows(t Theme, width, maxRows int) []string {
	if len(o.items) == 0 || maxRows <= 0 || width <= 0 {
		return nil
	}
	var rows []string
	var more int
	if o.menu || slices.ContainsFunc(o.items, func(c Completion) bool { return c.Detail != "" }) {
		rows, more = o.detailRows(t, width, maxRows)
	} else {
		rows, more = o.columnRows(t, width, maxRows)
	}
	if more > 0 {
		rows = append(rows, t.Dim.Wrap(selectIndent+moreLabel(more)))
	}
	return rows
}

// detailRows renders one candidate per line with its dim trailing detail,
// marking the highlight in a menu.
func (o *completionOverlay) detailRows(t Theme, width, maxRows int) ([]string, int) {
	n, more := fitRows(len(o.items), maxRows, 1)
	rows := make([]string, 0, maxRows)
	for i, c := range o.items[:n] {
		marker, style := selectIndent, t.Dim
		if o.menu && i == o.cursor {
			marker, style = selectMarker, t.Accent
		}
		line := style.Wrap(marker + c.Label)
		if c.Detail != "" {
			line += t.Dim.Wrap("  " + c.Detail)
		}
		rows = append(rows, truncateDisplay(line, width))
	}
	return rows, more
}

// columnRows packs the labels into equal-width columns, filling row by row.
func (o *completionOverlay) columnRows(t Theme, width, maxRows int) ([]string, int) {
	colW := completionGap
	for _, c := range o.items {
		colW = max(colW, displayWidth(c.Label)+completionGap)
	}
	cols := max(1, (width-displayWidth(selectIndent))/colW)
	n, more := fitRows(len(o.items), maxRows, cols)

	rows := make([]string, 0, maxRows)
	for i := 0; i < n; i += cols {
		var b strings.Builder
		b.WriteString(selectIndent)
		for _, c := range o.items[i:min(i+cols, n)] {
			b.WriteString(c.Label)
			b.WriteString(strings.Repeat(" ", colW-displayWidth(c.Label)))
		}
		rows = append(rows, truncateDisplay(t.Dim.Wrap(strings.TrimRight(b.String(), " ")), width))
	}
	return rows, more
}

// fitRows returns how many of total fit in maxRows rows of perRow, plus the
// remainder; an overflow reserves a whole row for the "N more" line.
func fitRows(total, maxRows, perRow int) (shown, more int) {
	if total <= maxRows*perRow {
		return total, 0
	}
	shown = max(maxRows-1, 0) * perRow
	return shown, total - shown
}

// commonPrefix returns the longest prefix every candidate's Text shares, cut on
// a grapheme boundary.
func commonPrefix(items []Completion) string {
	if len(items) == 0 {
		return ""
	}
	cells := graphemesOf(items[0].Text)
	for _, it := range items[1:] {
		other := graphemesOf(it.Text)
		n := min(len(cells), len(other))
		i := 0
		for i < n && cells[i] == other[i] {
			i++
		}
		cells = cells[:i]
		if i == 0 {
			break
		}
	}
	return strings.Join(cells, "")
}
