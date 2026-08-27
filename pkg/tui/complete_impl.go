package tui

import (
	"slices"
	"strings"
)

// completionList is the passive candidate listing drawn above the editor after a
// Tab that could not advance the buffer. It consumes no keys and holds no
// selection: the next keystroke clears it, and narrowing it means typing more,
// as in a shell.
type completionList struct {
	items []Completion
}

// completionGap separates packed candidate columns.
const completionGap = 2

// rows renders the candidates within maxRows: one per line when they carry
// detail text, else packed into columns like a shell's listing.
func (l *completionList) rows(t Theme, width, maxRows int) []string {
	if len(l.items) == 0 || maxRows <= 0 || width <= 0 {
		return nil
	}
	if slices.ContainsFunc(l.items, func(c Completion) bool { return c.Detail != "" }) {
		return l.detailRows(t, width, maxRows)
	}
	return l.columnRows(t, width, maxRows)
}

// detailRows renders one candidate per line with its dim trailing detail.
func (l *completionList) detailRows(t Theme, width, maxRows int) []string {
	n, more := fitRows(len(l.items), maxRows, 1)
	rows := make([]string, 0, maxRows)
	for _, c := range l.items[:n] {
		line := t.Dim.Wrap(selectIndent + c.Label)
		if c.Detail != "" {
			line += t.Dim.Wrap("  " + c.Detail)
		}
		rows = append(rows, truncateDisplay(line, width))
	}
	if more > 0 {
		rows = append(rows, t.Dim.Wrap(selectIndent+moreLabel(more)))
	}
	return rows
}

// columnRows packs the labels into equal-width columns, filling row by row.
func (l *completionList) columnRows(t Theme, width, maxRows int) []string {
	colW := completionGap
	for _, c := range l.items {
		colW = max(colW, displayWidth(c.Label)+completionGap)
	}
	avail := width - displayWidth(selectIndent)
	cols := max(1, avail/colW)
	n, more := fitRows(len(l.items), maxRows, cols)

	rows := make([]string, 0, maxRows)
	for i := 0; i < n; i += cols {
		var b strings.Builder
		b.WriteString(selectIndent)
		for _, c := range l.items[i:min(i+cols, n)] {
			b.WriteString(c.Label)
			b.WriteString(strings.Repeat(" ", colW-displayWidth(c.Label)))
		}
		rows = append(rows, truncateDisplay(t.Dim.Wrap(strings.TrimRight(b.String(), " ")), width))
	}
	if more > 0 {
		rows = append(rows, t.Dim.Wrap(selectIndent+moreLabel(more)))
	}
	return rows
}

// fitRows returns how many of total items fit in maxRows rows of perRow items,
// reserving a whole row for the "N more" line when they do not all fit.
func fitRows(total, maxRows, perRow int) (shown, more int) {
	if total <= maxRows*perRow {
		return total, 0
	}
	shown = max(maxRows-1, 0) * perRow
	return shown, total - shown
}

// commonPrefix returns the longest prefix every candidate's Text shares,
// truncated to whole grapheme cells.
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
