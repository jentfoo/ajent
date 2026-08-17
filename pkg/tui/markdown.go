package tui

import (
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

const (
	bulletMarker = "• "
	quotePrefix  = "▏ "
	codeIndent   = "  "
	minRuleWidth = 8
	// ruleChar is the one glyph repeated to fill a whole row, so uniseg's width
	// for it must match the terminal's: the live-block erase counts the divider
	// as one row. "─" is East Asian Ambiguous, which uniseg and effectively all
	// emulators in their default configuration measure as one column; the live
	// divider is also composed a column short (see repaint) so it never enters
	// the deferred-wrap state.
	ruleChar       = "─"
	maxTableWidth  = 120
	checkedBox     = "[x] "
	uncheckedBox   = "[ ] "
	blockSeparator = "\n\n"
)

// markdown parses GFM, output is unwrapped so the terminal owns line wrapping.
var markdown = goldmark.New(goldmark.WithExtensions(extension.GFM))

// renderMarkdown returns src as ANSI styled lines, each carrying how it should be
// treated on a width change. width is used only for elements that cannot reflow,
// such as rules and tables.
func renderMarkdown(t Theme, width int, src string) []histLine {
	if strings.TrimSpace(src) == "" {
		return nil
	}
	source := []byte(src)
	r := mdRenderer{theme: t, width: width}
	return r.blocks(markdown.Parser().Parse(text.NewReader(source)), source)
}

type mdRenderer struct {
	theme Theme
	width int
}

// blocks renders every child of n as history lines, separated by a blank line.
func (r mdRenderer) blocks(n ast.Node, src []byte) []histLine {
	var out []histLine
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if c.Kind() == east.KindTable {
			// a table is one structured line that re-lays itself on resize.
			if len(out) > 0 {
				out = append(out, histLine{})
			}
			out = append(out, histLine{table: r.buildTable(c, src)})
			continue
		}
		b, flow := r.block(c, src)
		if b == "" {
			continue
		}
		if len(out) > 0 {
			out = append(out, histLine{})
		}
		for _, l := range strings.Split(b, "\n") {
			out = append(out, histLine{text: l, flow: flow})
		}
	}
	return out
}

// blockTexts renders every child of n into an independent block of text, for
// blocks nested inside another that owns their layout.
func (r mdRenderer) blockTexts(n ast.Node, src []byte) []string {
	var out []string
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if b, _ := r.block(c, src); b != "" {
			out = append(out, b)
		}
	}
	return out
}

func (r mdRenderer) block(n ast.Node, src []byte) (string, lineFlow) {
	switch n.Kind() {
	case ast.KindHeading:
		h := n.(*ast.Heading)
		marker := r.theme.Dim.Wrap(strings.Repeat("#", h.Level) + " ")
		return marker + r.theme.Heading.Wrap(r.inline(n, src, "")), flowReflow
	case ast.KindParagraph, ast.KindTextBlock:
		return r.inline(n, src, ""), flowReflow
	case ast.KindFencedCodeBlock, ast.KindCodeBlock:
		return r.codeBlock(n, src), flowWrap
	case ast.KindBlockquote:
		body := strings.Join(r.blockTexts(n, src), blockSeparator)
		prefix := r.theme.Dim.Wrap(quotePrefix)
		return indentLines(r.theme.Quote.Wrap(body), prefix, prefix), flowWrap
	case ast.KindList:
		return r.list(n.(*ast.List), src), flowWrap
	case ast.KindThematicBreak:
		w := max(r.width, minRuleWidth)
		return r.theme.Dim.Wrap(strings.Repeat(ruleChar, w)), flowClip
	case east.KindTable: // nested table (inside a list or quote): laid out once
		t := r.buildTable(n, src)
		return strings.Join(layoutTable(t, r.width), "\n"), flowClip
	case ast.KindHTMLBlock:
		return r.theme.Dim.Wrap(strings.TrimRight(rawLines(n, src), "\n")), flowWrap
	default:
		return r.inline(n, src, ""), flowReflow
	}
}

// codeBlock renders fenced or indented code, indented and styled but not highlighted.
func (r mdRenderer) codeBlock(n ast.Node, src []byte) string {
	var lang string
	if f, ok := n.(*ast.FencedCodeBlock); ok {
		lang = string(f.Language(src))
	}
	var b strings.Builder
	if lang != "" {
		b.WriteString(r.theme.Dim.Wrap(codeIndent + lang))
		b.WriteString("\n")
	}
	body := strings.TrimRight(rawLines(n, src), "\n")
	for i, line := range strings.Split(body, "\n") {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(codeIndent + r.theme.Code.Wrap(line))
	}
	return b.String()
}

func (r mdRenderer) list(l *ast.List, src []byte) string {
	sep := "\n"
	if !l.IsTight {
		sep = blockSeparator
	}
	num := l.Start
	if num == 0 {
		num = 1
	}
	var items []string
	for c := l.FirstChild(); c != nil; c = c.NextSibling() {
		marker := bulletMarker
		if l.IsOrdered() {
			marker = strconv.Itoa(num) + ". "
			num++
		}
		body := strings.Join(r.blockTexts(c, src), sep)
		indent := strings.Repeat(" ", displayWidth(marker))
		items = append(items, indentLines(body, r.theme.Accent.Wrap(marker), indent))
	}
	return strings.Join(items, sep)
}

// mdAlign is a column's horizontal alignment from the GFM delimiter row.
type mdAlign uint8

const (
	alignLeft mdAlign = iota
	alignCenter
	alignRight
)

// mdTable holds one markdown table as styled cells plus per-column alignment, so
// it can be re-laid out at any width instead of being frozen when committed.
type mdTable struct {
	header []string   // header cells (first row), same length as every data row
	rows   [][]string // data cells, one slice per source row
	align  []mdAlign  // per-column alignment from the delimiter colons
}

// buildTable walks a GFM table node into its structured form. Cells are fully
// styled here; layout only measures and pads them.
func (r mdRenderer) buildTable(n ast.Node, src []byte) *mdTable {
	t := &mdTable{}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch c.Kind() {
		case east.KindTableHeader:
			var hdr []string
			for cc := c.FirstChild(); cc != nil; cc = cc.NextSibling() {
				cell, ok := cc.(*east.TableCell)
				if !ok {
					continue
				}
				hdr = append(hdr, r.inline(cc, src, ""))
				switch cell.Alignment {
				case east.AlignCenter:
					t.align = append(t.align, alignCenter)
				case east.AlignRight:
					t.align = append(t.align, alignRight)
				default:
					t.align = append(t.align, alignLeft)
				}
			}
			t.header = hdr
		case east.KindTableRow:
			var row []string
			for cc := c.FirstChild(); cc != nil; cc = cc.NextSibling() {
				if _, ok := cc.(*east.TableCell); !ok {
					continue
				}
				row = append(row, r.inline(cc, src, ""))
			}
			t.rows = append(t.rows, row)
		}
	}
	return t
}

// layoutTable renders the table at width: column widths come from content and are
// shrunk (long cells wrapped) when they exceed it; a separator runs between rows.
func layoutTable(t *mdTable, width int) []string {
	cols := len(t.header)
	if cols == 0 || t.align == nil {
		return nil
	}
	target := min(max(width, 1), maxTableWidth)
	avail := max(target-(3*cols+1), cols) // content room after borders and padding

	nat := make([]int, cols)
	for i, h := range t.header {
		nat[i] = cellMaxWidth(h)
	}
	for _, row := range t.rows {
		for i, c := range row {
			if i < cols {
				nat[i] = max(nat[i], cellMaxWidth(c))
			}
		}
	}
	w := shrinkColumns(nat, avail)

	// wrap every header and data cell to its column width before assembling rows.
	hdrLines := make([][]string, cols)
	for i, h := range t.header {
		hdrLines[i] = wrapCell(h, w[i])
	}
	datLines := make([][][]string, len(t.rows))
	for ri, row := range t.rows {
		datLines[ri] = make([][]string, cols)
		for ci, c := range row {
			if ci < cols {
				datLines[ri][ci] = wrapCell(c, w[ci])
			}
		}
	}

	var out []string
	out = append(out, hBorder("┌", "┬", "┐", w))
	nGroups := len(datLines) + 1 // header group first, then each data row as its own group
	for i := 0; i < nGroups; i++ {
		var cells [][]string
		if i == 0 {
			cells = hdrLines
		} else if i-1 < len(datLines) {
			cells = datLines[i-1]
		}
		out = append(out, tableRowGroup(cells, w, t.align)...)
		if i == nGroups-1 {
			out = append(out, hBorder("└", "┴", "┘", w))
		} else {
			out = append(out, hBorder("├", "┼", "┤", w))
		}
	}
	return out
}

// shrinkColumns reduces natural column widths so their sum fits avail, trimming the
// currently widest column first and never below a per-column floor.
func shrinkColumns(nat []int, avail int) []int {
	sum := 0
	for _, n := range nat {
		sum += n
	}
	if sum <= avail || len(nat) == 0 {
		return append([]int(nil), nat...)
	}
	minCol := min(3, max(avail/len(nat), 1))
	w := append([]int(nil), nat...)
	overflow := sum - avail
	for overflow > 0 {
		bi := -1
		for i := range w {
			if w[i] > minCol && (bi < 0 || w[i] > w[bi]) {
				bi = i
			}
		}
		if bi < 0 { // every column is at its floor; accept the overflow and clip later
			break
		}
		w[bi]--
		overflow--
	}
	return w
}

// hBorder builds a horizontal box line across columns, e.g. ┌──┬──┐.
func hBorder(left, mid, right string, w []int) string {
	var b strings.Builder
	b.WriteString(left)
	for i, cw := range w {
		if i > 0 {
			b.WriteString(mid)
		}
		b.WriteString(strings.Repeat("─", cw+2))
	}
	return b.String() + right
}

// tableRowGroup renders one logical row (header or data) into physical lines,
// wrapping when a cell spans more than one column line.
func tableRowGroup(cells [][]string, w []int, al []mdAlign) []string {
	n := 0
	for _, c := range cells {
		if len(c) > n {
			n = len(c)
		}
	}
	out := make([]string, 0, n)
	var b strings.Builder
	for li := 0; li < n; li++ {
		b.Reset()
		b.WriteString("│")
		for i, c := range cells {
			b.WriteString(" ")
			if li < len(c) {
				b.WriteString(padLine(c[li], w[i], al[i]))
			} else {
				b.WriteString(strings.Repeat(" ", w[i]))
			}
			b.WriteString(" │")
		}
		out = append(out, b.String())
	}
	return out
}

// padLine pads s to width columns honoring a column alignment. Styling is already
// baked into s; the appended spaces are plain.
func padLine(s string, w int, a mdAlign) string {
	sw := displayWidth(s)
	if sw >= w {
		return s
	}
	switch a {
	case alignCenter:
		l := (w - sw) / 2
		return strings.Repeat(" ", l) + s + strings.Repeat(" ", w-sw-l)
	case alignRight:
		return strings.Repeat(" ", w-sw) + s
	default:
		return s + strings.Repeat(" ", w-sw)
	}
}

// wrapCell breaks a cell into lines at most width columns, splitting on hard line
// breaks first and then wrapping long content without any hanging indent.
func wrapCell(s string, w int) []string {
	if w <= 0 {
		return nil
	}
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		out = append(out, wrapCellLine(ln, w)...)
	}
	return out
}

// wrapCellLine word-wraps one cell line to width using the same grapheme cells as
// wrapLine but with no indent or hanging marker.
func wrapCellLine(line string, w int) []string {
	if displayWidth(line) <= w {
		return []string{line}
	}
	cs := cells(line)
	var out []string
	for start := 0; start < len(cs); {
		end, cw := start, 0
		for end < len(cs) && cw+cs[end].width <= w {
			cw += cs[end].width
			end++
		}
		if end == start {
			end = start + 1 // a single cell wider than the column, never stall
		}
		stop := breakPoint(cs, start, end)
		for stop > start && cs[stop-1].text == " " {
			stop--
		}
		out = append(out, renderCells(cs[start:stop], ""))
		start = stop
		for start < len(cs) && cs[start].text == " " {
			start++
		}
	}
	return out
}

// cellMaxWidth returns the widest logical line in a possibly multi-line cell.
func cellMaxWidth(s string) int {
	m := 0
	for _, ln := range strings.Split(s, "\n") {
		if d := displayWidth(ln); d > m {
			m = d
		}
	}
	return m
}

// inline renders the inline children of n. active carries the ancestor styles so a
// nested style can restore them after its own reset.
func (r mdRenderer) inline(n ast.Node, src []byte, active string) string {
	var b strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch v := c.(type) {
		case *ast.Text:
			b.Write(v.Value(src))
			if v.HardLineBreak() {
				b.WriteString("\n")
			} else if v.SoftLineBreak() {
				b.WriteString(" ") // reflow, the terminal decides where to break
			}
		case *ast.String:
			b.Write(v.Value)
		case *ast.CodeSpan:
			b.WriteString(r.styled(r.theme.Code, c, src, active))
		case *ast.Emphasis:
			style := r.theme.Italic
			if v.Level >= 2 {
				style = r.theme.Bold
			}
			b.WriteString(r.styled(style, c, src, active))
		case *east.Strikethrough:
			b.WriteString(r.styled(r.theme.Strike, c, src, active))
		case *ast.Link:
			b.WriteString(r.link(v, src, active))
		case *ast.AutoLink:
			b.WriteString(r.theme.Link.Wrap(string(v.URL(src))) + active)
		case *ast.Image:
			label := r.inline(c, src, active)
			b.WriteString(r.theme.Dim.Wrap("[image: "+label+"]") + active)
		case *ast.RawHTML:
			b.WriteString(r.theme.Dim.Wrap(rawHTML(v, src)) + active)
		case *east.TaskCheckBox:
			if v.IsChecked {
				b.WriteString(r.theme.Accent.Wrap(checkedBox) + active)
			} else {
				b.WriteString(r.theme.Dim.Wrap(uncheckedBox) + active)
			}
		default:
			b.WriteString(r.inline(c, src, active))
		}
	}
	return b.String()
}

func (r mdRenderer) link(n *ast.Link, src []byte, active string) string {
	label := r.inline(n, src, active+r.theme.Link.Open())
	out := r.theme.Link.Wrap(label) + active
	if url := string(n.Destination); url != "" && url != label {
		out += r.theme.Dim.Wrap(" ("+url+")") + active
	}
	return out
}

// styled wraps the rendered children of n, restoring active after the reset.
func (r mdRenderer) styled(s Style, n ast.Node, src []byte, active string) string {
	inner := r.inline(n, src, active+s.Open())
	if s.Open() == "" {
		return inner
	}
	return s.Open() + inner + sgrReset + active
}

// rawLines returns the raw source lines of a block node.
func rawLines(n ast.Node, src []byte) string {
	var b strings.Builder
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		b.Write(seg.Value(src))
	}
	return b.String()
}

func rawHTML(n *ast.RawHTML, src []byte) string {
	var b strings.Builder
	for i := 0; i < n.Segments.Len(); i++ {
		seg := n.Segments.At(i)
		b.Write(seg.Value(src))
	}
	return b.String()
}

// indentLines prefixes the first line with first and the remainder with rest.
func indentLines(s, first, rest string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		if i == 0 {
			lines[i] = first + lines[i]
		} else {
			lines[i] = rest + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

// splitCompleteBlocks divides buffered markdown at the last safe block boundary.
// done is ready to render, rest must stay buffered until more input arrives.
func splitCompleteBlocks(buf string) (done, rest string) {
	var inFence bool
	var fence string
	var boundary int
	lines := strings.SplitAfter(buf, "\n")
	for i, ln := range lines {
		if !strings.HasSuffix(ln, "\n") {
			break // partial line, cannot classify it yet
		}
		trimmed := strings.TrimSpace(ln)
		if inFence {
			if marker := fenceMarker(trimmed); marker == fence {
				inFence, boundary = false, i+1
			}
		} else if marker := fenceMarker(trimmed); marker != "" {
			inFence, fence = true, marker
		} else if trimmed == "" {
			boundary = i + 1
		}
	}
	if boundary == 0 {
		return "", buf
	}
	return strings.Join(lines[:boundary], ""), strings.Join(lines[boundary:], "")
}

// fenceMarker returns the fence characters opening or closing a code fence.
func fenceMarker(trimmed string) string {
	for _, m := range []string{"```", "~~~"} {
		if strings.HasPrefix(trimmed, m) {
			return m
		}
	}
	return ""
}
