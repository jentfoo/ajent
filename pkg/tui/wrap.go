package tui

import (
	"strings"

	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/rivo/uniseg"
)

// maxHangRatio caps the continuation indent so a deeply marked line still has
// room for text
const maxHangRatio = 2

// cell is one grapheme cluster together with the SGR styling active at it.
type cell struct {
	text  string
	style string
	width int
}

// cells splits styled text into grapheme cells, dropping the escape sequences
// into each cell's style so the text can be re-cut at any column.
func cells(s string) []cell {
	var out []cell
	var active strings.Builder
	for _, seg := range splitANSI(s) {
		if seg.escape {
			if seg.text == sgrReset {
				active.Reset()
			} else {
				active.WriteString(seg.text)
			}
			continue
		}
		g := uniseg.NewGraphemes(seg.text)
		for g.Next() {
			out = append(out, cell{
				text:  g.Str(),
				style: active.String(),
				width: uniseg.StringWidth(g.Str()),
			})
		}
	}
	return out
}

// renderCells reassembles cells behind prefix, emitting a style only where it changes.
func renderCells(cs []cell, prefix string) string {
	var b strings.Builder
	b.WriteString(prefix)
	var active string
	for _, c := range cs {
		if c.style != active {
			if active != "" {
				b.WriteString(sgrReset)
			}
			b.WriteString(c.style)
			active = c.style
		}
		b.WriteString(c.text)
	}
	if active != "" {
		b.WriteString(sgrReset)
	}
	return b.String()
}

// wrapLine breaks one logical line into rows of at most width columns, preferring
// word boundaries and aligning continuation rows under any leading indent or list
// marker. Wrapping here rather than in the terminal keeps the row count exact and
// the layout identical across emulators.
func wrapLine(line string, width int) []string {
	if width <= 0 || displayWidth(line) <= width {
		return []string{line}
	}
	cs := cells(line)
	hang := min(hangWidth(line), width/maxHangRatio)
	indent := strings.Repeat(" ", hang)

	var rows []string
	var start int
	for start < len(cs) {
		avail := width
		prefix := ""
		if len(rows) > 0 {
			avail, prefix = width-hang, indent
		}
		end, w := start, 0
		for end < len(cs) && w+cs[end].width <= avail {
			w += cs[end].width
			end++
		}
		if end == start {
			end = start + 1 // a single cell wider than the row, never stall
		}
		if end >= len(cs) {
			rows = append(rows, renderCells(cs[start:], prefix))
			break
		}
		brk := breakPoint(cs, start, end)
		stop := brk
		for stop > start && cs[stop-1].text == " " {
			stop-- // the break space belongs to neither row
		}
		rows = append(rows, renderCells(cs[start:stop], prefix))
		start = brk
		for start < len(cs) && cs[start].text == " " {
			start++ // the break consumed the space, do not indent past it
		}
	}
	return rows
}

// breakPoint returns the last word boundary at or before end, or end when none exists.
func breakPoint(cs []cell, start, end int) int {
	if end < len(cs) && cs[end].text == " " {
		return end
	}
	for b := end; b > start; b-- {
		if cs[b-1].text == " " {
			return b
		}
	}
	return end
}

// hangWidth returns the display width of a line's leading indent plus any list
// or quote marker, so continuation rows align under the text.
func hangWidth(line string) int {
	plain := strutil.StripANSI(line)
	var w int
	for len(plain) > 0 && plain[0] == ' ' {
		plain, w = plain[1:], w+1
	}
	for _, m := range []string{bulletMarker, quotePrefix, "- ", "* "} {
		if strings.HasPrefix(plain, m) {
			return w + displayWidth(m)
		}
	}
	var d int
	for d < len(plain) && plain[d] >= '0' && plain[d] <= '9' {
		d++
	}
	if d > 0 && d+1 < len(plain) && (plain[d] == '.' || plain[d] == ')') && plain[d+1] == ' ' {
		return w + d + 2
	}
	return w
}
