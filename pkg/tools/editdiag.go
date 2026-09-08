package tools

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	udiff "github.com/aymanbagabas/go-udiff"
	"github.com/go-analyze/bulk"

	"github.com/jentfoo/ajent/pkg/strutil"
)

const (
	// maxBlockCandidates bounds how many shortlisted block starts get the
	// deciding similarity score.
	maxBlockCandidates = 8
	// maxAnchorHits is how often a line may repeat before it stops anchoring
	// anything: a brace on its own votes for every block in the file.
	maxAnchorHits = 64
	// minBlockSimilarity is the floor under which no text is offered to copy.
	// Pointing at a decoy is worse than admitting nothing here is close.
	minBlockSimilarity = 0.5
	// diffQuoteLimit bounds a difference worth quoting back. Past it the
	// verbatim block below says more than a quotation would.
	diffQuoteLimit = 40
	// maxWhitespaceIssues bounds distinct spacing complaints in one message.
	maxWhitespaceIssues = 5
	// maxIssueLines is how many line numbers name one repeated complaint.
	maxIssueLines = 3
)

// missingError diagnoses why a zero-match edit failed and guides a retry: name
// the failure, call out an earlier edit whose newText would create it (a cascade),
// report each reliably-detected cause, and end with the closest text verbatim so
// it can be copied.
func missingError(idx int, t editTarget, old, buf string, ops []editOp) string {
	cascade := cascadeIssue(idx, old, ops)

	var b strings.Builder
	fmt.Fprintf(&b, "no match for edit %d in %s.\n", idx, t.Path)
	// diagnose in canonical space: the tiers already tried every lookalike
	// difference, so reasoning here names the real cause instead of a stray quote.
	cold, _ := canonical(old)
	cbuf, _ := canonical(buf)
	switch issues := diagnoseNoMatch(cold, cbuf); {
	case cascade != "":
		b.WriteString("- " + cascade + "\n")
	case len(issues) > 0:
		for _, it := range issues {
			fmt.Fprintf(&b, "- %s\n", it)
		}
	default:
		b.WriteString("you must provide the oldText exactly as it appears in the file\n")
	}
	head := strings.TrimRight(b.String(), "\n")

	text, line, ok := closestBlock(old, buf)
	if !ok {
		return head + fmt.Sprintf("\nno similar text found in %s; read it before editing", t.Path)
	}
	if d := soleDifference(old, text); d != "" {
		head += "\n" + d // name the difference rather than leaving it to be spotted
	}
	// verbatim and untrimmed: this is the one payload that must be reproduced
	// byte for byte, so it carries no line-number gutter to strip
	bounded, _ := Elide(text, editTextLimit)
	return head + fmt.Sprintf("\nclosest text in %s at line %d (must match verbatim):\n", t.Path, line) + bounded
}

// soleDifference names the one run by which old and text differ, or is empty when
// they differ in more than one place or by too much to quote.
func soleDifference(old, text string) string {
	a, b, ok := driftRun(old, text)
	if !ok || a == "" || b == "" || len(a) > diffQuoteLimit || len(b) > diffQuoteLimit ||
		strings.Contains(a, "\n") || strings.Contains(b, "\n") {
		return ""
	}
	return fmt.Sprintf("you wrote %q where the file has %q; everything else matches", a, b)
}

// cascadeIssue reports when an earlier op's newText would create old, which is why
// matching it against the original buffer fails. idx is 1-based, so the scan stops
// before the failing op itself: an edit whose newText contains its own oldText is
// ordinary, not a cascade.
func cascadeIssue(idx int, old string, ops []editOp) string {
	for j := 0; j < idx-1 && j < len(ops); j++ {
		if strings.Contains(ops[j].NewText, old) {
			return fmt.Sprintf("'%s' is not in the file, but edit %d's newText would create it. Every edit is matched against the original file, never against another edit's output; combine edits %d and %d into one edit",
				old, j+1, j+1, idx)
		}
	}
	return ""
}

// diagnoseNoMatch inspects why old is absent from buf and reports each reliably
// detectable cause: per-line indentation/spacing when the words are all present (a
// pure-spacing mismatch), or casing. Returns nil when nothing reliable can be said.
func diagnoseNoMatch(old, buf string) []string {
	compact := stripSpace(old)
	if compact == "" {
		return nil // only-whitespace oldText: no anchor to reason about
	}
	switch {
	case strings.Contains(stripSpace(buf), compact):
		// every word present: a pure-spacing mismatch, name it per line when we can
		if specific := lineWhitespaceIssues(old, buf); len(specific) > 0 {
			return specific
		}
		return []string{"your words are all in the file but separated by different whitespace; you must match it exactly"}
	case strings.Contains(strings.ToLower(stripSpace(buf)), strings.ToLower(compact)):
		// words and order present ignoring case: only letter casing differs
		return []string{"the text matches the file only if you ignore letter case; your oldText's capitalization differs — copy it exactly"}
	default:
		if stripSpace(buf) == "" {
			return []string{"the file appears empty or whitespace-only"}
		}
		// the words genuinely differ; a retry needs exact text, not an approximation
		return []string{"your oldText appears nowhere in this file; copy it exactly"}
	}
}

// stripTrailingWS removes the whitespace at each line's end.
func stripTrailingWS(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRightFunc(l, unicode.IsSpace)
	}
	return strings.Join(lines, "\n")
}

// stripSpace removes all unicode whitespace.
func stripSpace(s string) string {
	var b strings.Builder
	for _, r := range s {
		if !unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// buildContent maps a text's non-empty lines to their 1-based line numbers and the
// count of blank (whitespace-only) lines immediately following each one.
type contentLine struct {
	n     int // 1-based original line number
	c     string
	after int // blank lines until the next content line or end of text
}

func buildContent(lines []string) []contentLine {
	var seq []contentLine
	for i := 0; i < len(lines); {
		if c := stripSpace(lines[i]); c == "" {
			i++
			continue
		} else {
			cl := contentLine{n: i + 1, c: c}
			i++
			for i < len(lines) && stripSpace(lines[i]) == "" {
				cl.after++
				i++
			}
			seq = append(seq, cl)
		}
	}
	return seq
}

// blankGapIssue reports when the number of blank lines after a matched content line
// differs between oldText and the file. want is what oldText has, have is ground truth.
func blankGapIssue(fileLine int, want, have int) string {
	switch {
	case want > have:
		return fmt.Sprintf("your oldText has %s after line %d, but the file has only %s there; remove them", countPhrase(want), fileLine, countPhrase(have))
	case want < have:
		return fmt.Sprintf("the file has %s after line %d that your oldText omits; add them to match exactly", countPhrase(have-want), fileLine)
	}
	return ""
}

// countPhrase renders a blank-line count as prose, singular-aware and zero-safe.
func countPhrase(n int) string {
	switch n {
	case 0:
		return "no blank lines"
	case 1:
		return "1 blank line"
	default:
		return strconv.Itoa(n) + " blank lines"
	}
}

// lineWhitespaceIssues aligns old's non-empty lines against the file and reports,
// per matched file line, how its whitespace differs from oldText. Only called when
// every word is present (a pure-spacing mismatch), so each pair shares stripped form.
func lineWhitespaceIssues(old, buf string) []string {
	oldLines := strings.Split(old, "\n")
	bufLines := strings.Split(buf, "\n")

	seq := buildContent(oldLines)
	fileSeq := buildContent(bufLines)

	match := -1
outer:
	for s := 0; s+len(seq) <= len(fileSeq); s++ {
		for k := range seq {
			if fileSeq[s+k].c != seq[k].c {
				continue outer
			}
		}
		match = s
		break
	}
	if match < 0 {
		return nil // lines don't align word-for-word; fall back to the generic note
	}

	// one complaint per distinct difference, naming the lines it was found on:
	// a block indented wrongly throughout is one fact, not one fact per line
	var order, gaps []string
	at := make(map[string][]int)
	for k := range seq {
		n := fileSeq[match+k].n
		if d := whitespaceIssue(oldLines[seq[k].n-1], bufLines[n-1]); d != "" {
			if _, seen := at[d]; !seen {
				order = append(order, d)
			}
			at[d] = append(at[d], n)
		}
		if d := blankGapIssue(n, seq[k].after, fileSeq[match+k].after); d != "" {
			gaps = append(gaps, d)
		}
	}

	issues := make([]string, 0, len(order)+len(gaps))
	for i, d := range order {
		if i == maxWhitespaceIssues {
			issues = append(issues, fmt.Sprintf("... and %d more spacing differences", len(order)-i))
			break
		}
		issues = append(issues, describeIssueLines(at[d])+": "+d)
	}
	if len(gaps) > maxWhitespaceIssues {
		gaps = append(gaps[:maxWhitespaceIssues],
			fmt.Sprintf("... and %d more blank-line differences", len(gaps)-maxWhitespaceIssues))
	}
	return append(issues, gaps...)
}

// describeIssueLines renders the file lines one complaint covers, capped.
func describeIssueLines(ls []int) string {
	if len(ls) == 1 {
		return fmt.Sprintf("line %d", ls[0])
	}
	shown, more := ls, ""
	if len(shown) > maxIssueLines {
		shown, more = shown[:maxIssueLines], fmt.Sprintf(" and %d more", len(ls)-maxIssueLines)
	}
	parts := make([]string, len(shown))
	for i, l := range shown {
		parts[i] = strconv.Itoa(l)
	}
	return "lines " + strings.Join(parts, ", ") + more
}

// whitespaceRuns returns line's gaps: the leading run first, empty when the line is
// flush left, then the run between each pair of words. Callers pass canonical text,
// where no trailing run survives, so none is reported.
func whitespaceRuns(line string) []string {
	runs := []string{leadingWS(line)}
	var cur strings.Builder
	for _, r := range strings.TrimSpace(line) {
		if unicode.IsSpace(r) {
			cur.WriteRune(r)
			continue
		}
		if cur.Len() > 0 { // a word ended, flush the run before it
			runs = append(runs, cur.String())
			cur.Reset()
		}
	}
	return runs
}

// whitespaceIssue names the first gap where oldLine and fileLine space their words
// differently, empty when they agree. Both must carry identical stripped content,
// which makes a gap the only thing that can differ; the file is ground truth.
func whitespaceIssue(oldLine, fileLine string) string {
	want, have := whitespaceRuns(oldLine), whitespaceRuns(fileLine)
	for i := 0; i < len(want) && i < len(have); i++ {
		if want[i] == have[i] {
			continue
		}
		if i == 0 {
			return fmt.Sprintf("indentation differs: the file line uses %s, your text has %s; match it exactly",
				describeIndent(have[i]), describeIndent(want[i]))
		}
		return fmt.Sprintf("spacing between words differs: the file has %s, your text has %s",
			describeRun(have[i]), describeRun(want[i]))
	}
	return ""
}

// leadingWS returns the indentation of s's first line, never crossing into the
// next one. Indentation is ASCII space/tab, so byte-wise scanning is rune-safe.
func leadingWS(s string) string {
	i := 0
	for i < len(s) && s[i] != '\n' && unicode.IsSpace(rune(s[i])) {
		i++
	}
	return s[:i]
}

// countWS tallies tabs and spaces in s.
func countWS(s string) (tabs, spaces int) {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\t':
			tabs++
		case ' ':
			spaces++
		}
	}
	return tabs, spaces
}

// describeRun renders one whitespace run as its exact tabs/spaces composition.
func describeRun(run string) string {
	tabs, spaces := countWS(run)
	switch {
	case tabs > 0 && spaces == 0:
		return plural(tabs, "tab")
	case spaces > 0 && tabs == 0:
		return plural(spaces, "space")
	default:
		return fmt.Sprintf("mixed %s and %s", plural(tabs, "tab"), plural(spaces, "space"))
	}
}

// plural renders n unit(s), singular for one.
func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

// closestBlock finds the region of buf most similar to old and returns it
// verbatim with the 1-based line it starts on. ok is false when nothing in the
// file resembles old, so a decoy is never offered as the text to copy.
func closestBlock(old, buf string) (text string, line int, ok bool) {
	fileLines := dropTrailingEmpty(strings.Split(buf, "\n"))
	oldLines := strings.Split(old, "\n")
	if len(fileLines) == 0 || stripSpace(old) == "" {
		return "", 0, false
	}

	starts := anchorCandidates(oldLines, fileLines)
	if len(starts) == 0 {
		// a fragment sitting inside a line has no whole-line anchor
		starts = tokenCandidates(old, fileLines)
	}

	best, bestScore := -1, 0.0
	for _, s := range starts {
		end := min(s+len(oldLines), len(fileLines))
		if sc := blockSimilarity(old, strings.Join(fileLines[s:end], "\n")); sc > bestScore {
			best, bestScore = s, sc
		}
	}
	if best < 0 || bestScore < minBlockSimilarity {
		return "", 0, false // nothing here resembles it; say so rather than point at a decoy
	}
	end := min(best+len(oldLines), len(fileLines))
	return strings.Join(fileLines[best:end], "\n"), best + 1, true
}

// anchorCandidates returns the block starts that old's lines agree on: every
// content line votes for the start that would align it with an identical file
// line. Voting keeps selection linear instead of scoring every window.
func anchorCandidates(oldLines, fileLines []string) []int {
	index := make(map[string][]int, len(fileLines))
	for i, l := range fileLines {
		if c := stripSpace(l); c != "" {
			index[c] = append(index[c], i)
		}
	}
	votes := make(map[int]int)
	for k, l := range oldLines {
		c := stripSpace(l)
		if c == "" {
			continue
		}
		hits := index[c]
		if len(hits) > maxAnchorHits { // a line repeated everywhere anchors nothing
			continue
		}
		for _, j := range hits {
			if s := j - k; s >= 0 && s < len(fileLines) {
				votes[s]++
			}
		}
	}
	return topCandidates(votes)
}

// tokenCandidates shortlists lines sharing a word with old, for a fragment that
// sits inside its line and so has no whole-line anchor. Token overlap filters
// only; blockSimilarity still decides which candidate wins.
func tokenCandidates(old string, fileLines []string) []int {
	tokens := strings.Fields(old)
	if len(tokens) == 0 {
		return nil
	}
	votes := make(map[int]int)
	for i, l := range fileLines {
		var n int
		for _, t := range tokens {
			if strings.Contains(l, t) {
				n++
			}
		}
		if n > 0 {
			votes[i] = n
		}
	}
	return topCandidates(votes)
}

// topCandidates returns the highest-voted starts, most votes first and earliest
// first among equals, capped so the deciding score runs a bounded number of times.
func topCandidates(votes map[int]int) []int {
	starts := bulk.MapKeysSlice(votes)
	slices.SortFunc(starts, func(a, b int) int {
		if votes[a] != votes[b] {
			return votes[b] - votes[a]
		}
		return a - b
	})
	if len(starts) > maxBlockCandidates {
		starts = starts[:maxBlockCandidates]
	}
	return starts
}

// blockSimilarity scores two texts from 0 to 1 as twice the matched bytes over
// their combined length, the ratio a unified diff implies. It decides between
// candidates, so it runs only on the shortlist.
func blockSimilarity(a, b string) float64 {
	switch {
	case a == b:
		return 1
	case a == "" || b == "":
		return 0
	}
	var deleted int
	for _, e := range udiff.Strings(a, b) {
		deleted += e.End - e.Start
	}
	matched := max(0, len(a)-deleted)
	return 2 * float64(matched) / float64(len(a)+len(b))
}

// ambiguousError names the occurrence count and gives two concrete retry
// options, then lists the line each match starts on (capped). It is given the
// resolved matches rather than searching for oldText itself, so a near-miss
// match is reported where it actually landed.
func ambiguousError(idx int, path string, old, buf string, ms []match) string {
	var b strings.Builder
	fmt.Fprintf(&b, "edit %d matches %d occurrences in %s; widen its oldText with surrounding context or set replace_all:true.\n",
		idx, len(ms), path)
	lines := dropTrailingEmpty(strings.Split(buf, "\n"))
	starts := lineStarts(lines)
	for _, m := range ms {
		n := lineOf(starts, m.s)
		if n >= len(lines) {
			continue
		}
		fmt.Fprintf(&b, "%6d\t%s\n", n+1, strutil.Clip(strings.TrimSpace(lines[n]), MaxLineRunes))
		if b.Len() > 800 {
			b.WriteString("... more matches omitted; add unique context or set replace_all:true\n")
			break
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
