package tui

import (
	"strconv"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	prettytext "github.com/jedib0t/go-pretty/v6/text"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

const (
	bulletMarker   = "• "
	quotePrefix    = "▏ "
	codeIndent     = "  "
	minRuleWidth   = 8
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
		return prefixLines(r.theme.Quote.Wrap(body), r.theme.Dim.Wrap(quotePrefix)), flowWrap
	case ast.KindList:
		return r.list(n.(*ast.List), src), flowWrap
	case ast.KindThematicBreak:
		w := max(r.width, minRuleWidth)
		return r.theme.Dim.Wrap(strings.Repeat(ruleChar, w)), flowClip
	case east.KindTable:
		return r.table(n, src), flowClip
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

// table renders a GFM table through go-pretty, hard sized since it cannot reflow.
func (r mdRenderer) table(n ast.Node, src []byte) string {
	tw := table.NewWriter()
	tw.SetStyle(table.StyleLight)
	tw.Style().Options.SeparateRows = false
	tw.Style().Format.Header = prettytext.FormatDefault // keep the author's casing
	if w := min(r.width, maxTableWidth); w > 0 {
		tw.SetAllowedRowLength(w)
	}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch c.Kind() {
		case east.KindTableHeader:
			tw.AppendHeader(r.tableRow(c, src))
			tw.SetColumnConfigs(r.columnConfigs(c))
		case east.KindTableRow:
			tw.AppendRow(r.tableRow(c, src))
		}
	}
	return strings.TrimRight(tw.Render(), "\n")
}

func (r mdRenderer) tableRow(n ast.Node, src []byte) table.Row {
	var row table.Row
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		row = append(row, r.inline(c, src, ""))
	}
	return row
}

func (r mdRenderer) columnConfigs(header ast.Node) []table.ColumnConfig {
	var cfgs []table.ColumnConfig
	var i int
	for c := header.FirstChild(); c != nil; c = c.NextSibling() {
		i++
		cell, ok := c.(*east.TableCell)
		if !ok {
			continue
		}
		var align prettytext.Align
		switch cell.Alignment {
		case east.AlignCenter:
			align = prettytext.AlignCenter
		case east.AlignRight:
			align = prettytext.AlignRight
		default:
			continue
		}
		cfgs = append(cfgs, table.ColumnConfig{Number: i, Align: align, AlignHeader: align})
	}
	return cfgs
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

func prefixLines(s, prefix string) string {
	return indentLines(s, prefix, prefix)
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
