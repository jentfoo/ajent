package tui

import (
	"slices"
	"strconv"
	"strings"

	udiff "github.com/aymanbagabas/go-udiff"
)

const (
	// minIntralineSimilarity is the fraction of a line that must be unchanged before
	// word level emphasis is worth showing instead of a whole line replacement
	minIntralineSimilarity = 0.5
	// spans closer together than this are merged to avoid speckled highlights
	minSpanGap = 4
)

// RenderDiff returns a colorized unified diff of before and after, empty when they match.
func RenderDiff(t Theme, path, before, after string) string {
	unified := udiff.Unified(path, path, before, after)
	if unified == "" {
		return ""
	}
	var body []string
	var added, removed int
	var dels, adds []string
	flush := func() {
		body = append(body, renderDiffRun(t, dels, adds)...)
		dels, adds = nil, nil
	}
	for _, line := range strings.Split(strings.TrimRight(unified, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "---"), strings.HasPrefix(line, "+++"):
			continue // replaced by our own header
		case strings.HasPrefix(line, "-"):
			removed++
			dels = append(dels, line[1:])
		case strings.HasPrefix(line, "+"):
			added++
			adds = append(adds, line[1:])
		case strings.HasPrefix(line, "@@"):
			flush()
			body = append(body, t.DiffHunk.Wrap(line))
		default:
			flush()
			body = append(body, t.Dim.Wrap(line))
		}
	}
	flush()

	header := t.DiffFile.Wrap(path) + " " +
		t.DiffAdd.Wrap("+"+strconv.Itoa(added)) + " " +
		t.DiffDel.Wrap("-"+strconv.Itoa(removed))
	return header + "\n" + strings.Join(body, "\n") + "\n"
}

// renderDiffRun renders a removed/added run, emphasizing word level changes when
// the two sides line up one to one.
func renderDiffRun(t Theme, dels, adds []string) []string {
	if len(dels) == 0 && len(adds) == 0 {
		return nil
	}
	out := make([]string, 0, len(dels)+len(adds))
	paired := len(dels) == len(adds)
	for i, d := range dels {
		if !paired {
			out = append(out, t.DiffDel.Wrap("-"+d))
			continue
		}
		delSpans, _ := intralineSpans(d, adds[i])
		out = append(out, applySpans("-", d, delSpans, t.DiffDel, t.DiffDelWord))
	}
	for i, a := range adds {
		if !paired {
			out = append(out, t.DiffAdd.Wrap("+"+a))
			continue
		}
		_, addSpans := intralineSpans(dels[i], a)
		out = append(out, applySpans("+", a, addSpans, t.DiffAdd, t.DiffAddWord))
	}
	return out
}

// intralineSpans returns the changed byte ranges within before and within after.
// Both are nil when the lines are too dissimilar for word level emphasis to help.
func intralineSpans(before, after string) (delSpans, addSpans [][2]int) {
	edits := udiff.Strings(before, after)
	if len(edits) == 0 {
		return nil, nil
	}
	slices.SortFunc(edits, func(a, b udiff.Edit) int { return a.Start - b.Start })

	var changed int
	var oldPos, newPos int
	for _, e := range edits {
		newPos += e.Start - oldPos
		if e.End > e.Start {
			delSpans = append(delSpans, [2]int{e.Start, e.End})
			changed += e.End - e.Start
		}
		if len(e.New) > 0 {
			addSpans = append(addSpans, [2]int{newPos, newPos + len(e.New)})
			changed += len(e.New)
		}
		newPos += len(e.New)
		oldPos = e.End
	}
	if total := len(before) + len(after); total == 0 ||
		float64(total-changed)/float64(total) < minIntralineSimilarity {
		return nil, nil
	}
	return mergeSpans(delSpans), mergeSpans(addSpans)
}

// mergeSpans joins spans separated by less than minSpanGap unchanged bytes.
func mergeSpans(spans [][2]int) [][2]int {
	if len(spans) < 2 {
		return spans
	}
	out := append(make([][2]int, 0, len(spans)), spans[0])
	for _, s := range spans[1:] {
		last := &out[len(out)-1]
		if s[0]-last[1] < minSpanGap {
			last[1] = s[1]
		} else {
			out = append(out, s)
		}
	}
	return out
}

// applySpans styles marker and content with base, switching to emph inside the
// given ranges of content.
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
