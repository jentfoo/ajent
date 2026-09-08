package tools

import (
	"fmt"
	"slices"
	"strings"

	udiff "github.com/aymanbagabas/go-udiff"
)

// editTextLimit bounds the text an edit result carries, whether a diff or the
// closest-match hint. Head and tail survive, so both seams of an oversized block
// reach the model.
var editTextLimit = Limit{Lines: 400, Bytes: 32 << 10}

// diffContext is how many lines of unchanged text frame each change.
const diffContext = 3

// editReport renders a successful edit for the model: summary line, notes,
// and the diff when review asks for one.
func editReport(path string, count int, o editOutcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "applied %d edits to %s", count, path)
	for _, n := range o.notes {
		b.WriteString("\n" + n)
	}
	if o.review {
		if d := unifiedDiff(path, o.before, o.after); d != "" {
			b.WriteString("\n\n" + d)
		}
	}
	return b.String()
}

// dupCheckMin is the shortest newText worth counting repeats of; shorter text
// appears everywhere, so warning trains the model to ignore warnings.
const dupCheckMin = 24

// duplicateNote returns a note when a single-site edit's text now appears
// more than once in after. wrote is what a tier wrote, which may differ from
// newText after a reindent.
func duplicateNote(idx int, op editOp, wrote, after string) string {
	if op.ReplaceAll || len(strings.TrimSpace(op.NewText)) < dupCheckMin {
		return ""
	}
	n := strings.Count(after, op.NewText)
	if wrote != op.NewText {
		n = max(n, strings.Count(after, wrote))
	}
	if n > 1 {
		return fmt.Sprintf("edit %d: this newText now appears %d times in the file; check the diff for a duplicated block", idx, n)
	}
	return ""
}

// seamNote returns a note when the apply wrote a line identical to its
// neighbour, the re-emission shape a repeat count misses.
func seamNote(after string, edited []lineRange) string {
	lines := dropTrailingEmpty(strings.Split(after, "\n"))
	for i := 0; i+1 < len(lines); i++ {
		if !nonBlank(lines[i]) || lines[i] != lines[i+1] {
			continue
		}
		if slices.ContainsFunc(edited, func(r lineRange) bool { return i < r.to && r.from < i+2 }) {
			return fmt.Sprintf("lines %d and %d are identical after the edit; check the diff for a duplicated block", i+1, i+2)
		}
	}
	return ""
}

// unifiedDiff renders a plain unified diff of before and after, empty when they
// match. pkg/tools never imports pkg/tui, so this drives go-udiff directly
// rather than reusing the theme-coupled renderer there.
func unifiedDiff(path, before, after string) string {
	if before == after {
		return ""
	}
	edits := udiff.Lines(before, after)
	if len(edits) == 0 {
		return ""
	}
	out, err := udiff.ToUnified(path, path, before, edits, diffContext)
	if err != nil || out == "" {
		return ""
	}
	bounded, _ := Elide(strings.TrimRight(out, "\n"), editTextLimit)
	return bounded
}

// lineStarts returns each line's byte offset in the LF-joined text.
func lineStarts(lines []string) []int {
	starts := make([]int, len(lines))
	off := 0
	for i := range lines {
		starts[i] = off
		off += len(lines[i]) + 1 // +1 for the newline separator
	}
	return starts
}

// lineOf returns the 0-based index of the line containing byte offset off.
func lineOf(starts []int, off int) int {
	i, found := slices.BinarySearch(starts, off)
	if found {
		return i
	}
	return max(0, i-1)
}

// nonBlank reports whether s holds anything but whitespace.
func nonBlank(s string) bool { return strings.TrimSpace(s) != "" }

// dropTrailingEmpty removes the empty element a newline-terminated split leaves,
// so it never renders as a numbered blank row at file end.
func dropTrailingEmpty(ls []string) []string {
	if n := len(ls); n > 0 && ls[n-1] == "" {
		return ls[:n-1]
	}
	return ls
}
