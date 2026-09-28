package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	udiff "github.com/aymanbagabas/go-udiff"
)

const (
	// minIntralineSimilarity is the fraction of a line that must be unchanged before
	// word level emphasis is worth showing instead of a whole line replacement
	minIntralineSimilarity = 0.5
	// edits closer together than this are joined to avoid speckled highlights
	minSpanGap = 4
	// minGutter is the narrowest the line number column gets
	minGutter = 2
	// contextLines kept around each change, matching git's default. Enough to place
	// a change without reprinting the file around it.
	contextLines = 3
)

// DiffSummary names a change in one line, for an approval dialog whose subject
// sits below the rendered diff. Empty when before and after match.
func DiffSummary(path, before, after string) string {
	added, removed, ok := diffStat(before, after)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s +%d -%d", path, added, removed)
}

// RenderDiff returns a colorized, line numbered unified diff of before and
// after, empty when they match. Git shaped: each changed region is its own hunk
// under an @@ header, with contextLines of surrounding code.
func RenderDiff(t Theme, path, before, after string) string {
	hunks, ok := diffHunks(before, after)
	if !ok {
		return ""
	}
	gw := max(minGutter, len(strconv.Itoa(max(lineCount(before), lineCount(after)))))

	var body []string
	var added, removed int
	for _, h := range hunks {
		body = append(body, t.DiffHunk.Wrap(hunkHeader(h)))
		rows, a, d := renderHunk(t, h, gw)
		body = append(body, rows...)
		added, removed = added+a, removed+d
	}

	header := t.DiffFile.Wrap(path) + " " +
		t.DiffAdd.Wrap("+"+strconv.Itoa(added)) + " " +
		t.DiffDel.Wrap("-"+strconv.Itoa(removed))
	return header + "\n" + strings.Join(body, "\n") + "\n"
}

// hunkHeader renders a hunk's @@ range marker, following git in dropping the
// count when a side spans a single line.
func hunkHeader(h *udiff.Hunk) string {
	var fromCount, toCount int
	for _, l := range h.Lines {
		switch l.Kind {
		case udiff.Delete:
			fromCount++
		case udiff.Insert:
			toCount++
		default:
			fromCount++
			toCount++
		}
	}
	return "@@ -" + hunkRange(h.FromLine, fromCount) + " +" + hunkRange(h.ToLine, toCount) + " @@"
}

func hunkRange(start, count int) string {
	switch count {
	case 0: // an empty side starts at 0, as git and diff -u both report it
		return "0,0"
	case 1:
		return strconv.Itoa(start)
	default:
		return strconv.Itoa(start) + "," + strconv.Itoa(count)
	}
}

// diffHunks returns the line diff of before and after as hunks, ok reporting
// whether they differ at all.
func diffHunks(before, after string) ([]*udiff.Hunk, bool) {
	if before == after {
		return nil, false
	}
	edits := udiff.Lines(before, after)
	if len(edits) == 0 {
		return nil, false
	}
	u, err := udiff.ToUnifiedDiff("", "", before, edits, contextLines)
	if err != nil || len(u.Hunks) == 0 {
		return nil, false
	}
	return u.Hunks, true
}

// diffStat counts the added and removed lines between before and after.
func diffStat(before, after string) (added, removed int, ok bool) {
	hunks, ok := diffHunks(before, after)
	if !ok {
		return 0, 0, false
	}
	for _, h := range hunks {
		for _, l := range h.Lines {
			switch l.Kind {
			case udiff.Insert:
				added++
			case udiff.Delete:
				removed++
			}
		}
	}
	return added, removed, true
}

// lineCount returns the number of lines in s, counting a trailing partial line.
func lineCount(s string) int {
	if s == "" {
		return 1
	}
	return strings.Count(strings.TrimSuffix(s, "\n"), "\n") + 1
}

// diffRow is one file line with the number to show beside it.
type diffRow struct {
	num  int
	text string
}

// renderHunk renders every line of a hunk, tracking the old and new file line
// numbers so each row carries the number it has in the file it came from.
func renderHunk(t Theme, h *udiff.Hunk, gw int) (rows []string, added, removed int) {
	oldNo, newNo := h.FromLine, h.ToLine
	var dels, adds []diffRow
	flush := func() {
		rows = append(rows, renderDiffRun(t, gw, dels, adds)...)
		dels, adds = nil, nil
	}
	for _, l := range h.Lines {
		text := strings.ReplaceAll(strings.TrimRight(l.Content, "\r\n"), "\t", tabSpaces)
		switch l.Kind {
		case udiff.Delete:
			dels = append(dels, diffRow{num: oldNo, text: text})
			oldNo++
			removed++
		case udiff.Insert:
			adds = append(adds, diffRow{num: newNo, text: text})
			newNo++
			added++
		default:
			flush()
			rows = append(rows, rowPrefix(gw, newNo)+t.Dim.Wrap("  "+text))
			oldNo++
			newNo++
		}
	}
	flush()
	return rows, added, removed
}

// rowPrefix returns the line number gutter for one row. The change marker is
// the caller's so its color can start at it.
func rowPrefix(gw, num int) string {
	return fmt.Sprintf("%*d ", gw, num)
}

// renderDiffRun renders a removed/added run, emphasizing word level changes when
// the two sides line up one to one.
func renderDiffRun(t Theme, gw int, dels, adds []diffRow) []string {
	if len(dels) == 0 && len(adds) == 0 {
		return nil
	}
	out := make([]string, 0, len(dels)+len(adds))
	var delSpans, addSpans [][][2]int
	if len(dels) == len(adds) { // one to one: word level emphasis, computed once per pair
		delSpans, addSpans = make([][][2]int, len(dels)), make([][][2]int, len(adds))
		for i := range dels {
			delSpans[i], addSpans[i] = intralineSpans(dels[i].text, adds[i].text)
		}
	}
	for i, d := range dels {
		var spans [][2]int
		if delSpans != nil {
			spans = delSpans[i]
		}
		out = append(out, rowPrefix(gw, d.num)+applySpans("- ", d.text, spans, t.DiffDel, t.DiffDelWord))
	}
	for i, a := range adds {
		var spans [][2]int
		if addSpans != nil {
			spans = addSpans[i]
		}
		out = append(out, rowPrefix(gw, a.num)+applySpans("+ ", a.text, spans, t.DiffAdd, t.DiffAddWord))
	}
	return out
}

// intralineSpans returns the changed byte ranges within before and within after.
// Both are nil when the lines are too dissimilar for word level emphasis to help.
func intralineSpans(before, after string) (delSpans, addSpans [][2]int) {
	edits := mergeCloseEdits(before, udiff.Strings(before, after))
	if len(edits) == 0 {
		return nil, nil
	}
	slices.SortFunc(edits, func(a, b udiff.Edit) int { return a.Start - b.Start })

	var oldPos, newPos int
	for _, e := range edits {
		newPos += e.Start - oldPos
		if e.End > e.Start {
			delSpans = append(delSpans, [2]int{e.Start, e.End})
		}
		if len(e.New) > 0 {
			addSpans = append(addSpans, [2]int{newPos, newPos + len(e.New)})
		}
		newPos += len(e.New)
		oldPos = e.End
	}
	// Gate on post-merge coverage: a joined gap highlights unchanged bytes too,
	// so pre-merge counts would let a chain of tiny edits pass as a minor tweak.
	total := len(before) + len(after)
	if total == 0 || float64(total-spanBytes(delSpans)-spanBytes(addSpans))/float64(total) <
		minIntralineSimilarity {
		return nil, nil
	}
	return delSpans, addSpans
}

// mergeCloseEdits joins edits separated by less than minSpanGap unchanged bytes
// so a stray byte or two between changes still renders as one span. The gap is
// absorbed on both sides at once: joining spans per side instead would
// highlight bytes the other side still shows as unchanged.
func mergeCloseEdits(before string, edits []udiff.Edit) []udiff.Edit {
	out := make([]udiff.Edit, 0, len(edits))
	for _, e := range edits {
		i := len(out) - 1
		// two bare insertions stay apart, there is no gap to absorb
		if i >= 0 && e.Start-out[i].End < minSpanGap &&
			(out[i].Start != out[i].End || e.Start != e.End) {
			out[i].New += before[out[i].End:e.Start] + e.New
			out[i].End = e.End
			continue
		}
		out = append(out, e)
	}
	return out
}

// spanBytes sums the bytes covered by each span.
func spanBytes(spans [][2]int) int {
	var n int
	for _, s := range spans {
		n += s[1] - s[0]
	}
	return n
}

// applySpans styles the change marker and content with base, switching to emph
// inside the given ranges of content. The gutter is not part of either string.
func applySpans(marker, content string, spans [][2]int, base, emph Style) string {
	if len(spans) == 0 || emph.Open() == "" {
		return base.Wrap(marker + content)
	}
	var b strings.Builder
	b.WriteString(base.Open())
	b.WriteString(marker)
	var pos int
	for _, s := range spans {
		if s[0] > len(content) {
			break
		}
		end := min(s[1], len(content))
		b.WriteString(content[pos:s[0]])
		b.WriteString(emph.Open())
		b.WriteString(content[s[0]:end])
		b.WriteString(sgrReset)
		b.WriteString(base.Open())
		pos = end
	}
	b.WriteString(content[pos:])
	b.WriteString(base.Close())
	return b.String()
}
