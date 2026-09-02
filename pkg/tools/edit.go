package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
)

// editOp is one exact-string replacement.
type editOp struct {
	OldText    string `json:"oldText" desc:"exact text to replace; must match a unique region unless replace_all is set"`
	NewText    string `json:"newText" desc:"replacement text"`
	ReplaceAll bool   `json:"replace_all,omitempty" desc:"replace every occurrence instead of exactly one"`
}

// editParams carries a list of edits applied atomically to one file.
type editParams struct {
	Path  string   `json:"path"`
	Edits []editOp `json:"edits" desc:"ordered replacements; all apply or none do"`
}

// editTool applies exact-string edits to a single file, all-or-nothing. The
// array form gives one round trip for a multi-site refactor.
type editTool struct {
	policy  PathPolicy
	tracker *Tracker
}

var _ agent.Tool = (*editTool)(nil)
var _ DryRunner = (*editTool)(nil)
var _ Previewer = (*editTool)(nil)

// resolveApply resolves the path and returns the current file text with the edits
// applied in LF space, so DryRun and Preview share one code path.
func (t *editTool) resolveApply(call agent.ToolCall) (Change, error) {
	var p editParams
	err := decode(call.Input, &p)
	if err != nil {
		return Change{}, errors.New("bad args: " + err.Error())
	}
	full, err := t.policy.Resolve(p.Path)
	if err != nil {
		return Change{}, err
	}
	data, err := os.ReadFile(full) // missing or unreadable counts as will-fail
	if err != nil {
		return Change{}, err
	}
	before := normalizeToLF(string(data))
	after, _, _, aerr := applyEdits(p.Path, string(data), p.Edits)
	return Change{Path: p.Path, Before: before, After: after}, aerr
}

// DryRun reports whether an edit call would fail before it runs: the file is
// missing or unreadable, or applyEdits rejects any op. The barrier uses it to skip
// a prompt for a doomed call and let Execute return its natural error.
func (t *editTool) DryRun(call agent.ToolCall) error {
	_, err := t.resolveApply(call)
	return err
}

// Preview returns the file's current text and what it would become, so the diff
// is shown before the call is vetted.
func (t *editTool) Preview(call agent.ToolCall) (Change, error) {
	return t.resolveApply(call)
}

func (t *editTool) Name() string { return "edit" }
func (t *editTool) Label(agent.ToolCall) string {
	return "edit"
}
func (t *editTool) Description() string {
	return "Edit a single file using exact text replacement. Every edits[].oldText must match a unique region of the file, or set replace_all. All edits apply atomically or none do. Built-in safeguards make this safe for agent use; prefer `edit` over bash tools to modify files."
}
func (t *editTool) Schema() llm.ToolSchema { return llm.ToolSchema{Parameters: SchemaOf[editParams]()} }
func (t *editTool) Mode() agent.ExecutionMode {
	return agent.ModeSerial
}

// Execute applies every edit against the original buffer, then writes once so a
// failure mid-list leaves the file byte-identical. The diff is rendered by the
// guard wrapper before this runs.
func (t *editTool) Execute(ctx context.Context, call agent.ToolCall, out agent.Output) (agent.ToolResult, error) {
	var p editParams
	if err := decode(call.Input, &p); err != nil {
		return resultErr("bad args: " + err.Error()), nil
	}
	if len(p.Edits) == 0 {
		return resultErr("edit requires at least one entry in edits"), nil
	}
	full, err := t.policy.Resolve(p.Path)
	if err != nil {
		return resultErr(err.Error()), nil
	}

	// The file is re-read here; a stale or changed file simply fails the exact-text match in applyEdits.
	data, err := os.ReadFile(full)
	if err != nil {
		return resultErr("edit: " + err.Error()), nil
	}
	_, final, warns, err := applyEdits(p.Path, string(data), p.Edits)
	if err != nil {
		return resultErr(err.Error()), nil
	}

	if err := config.WriteFileAtomic(full, final, writePerm(full)); err != nil {
		return resultErr("edit: " + err.Error()), nil
	}
	t.tracker.Observe(full, final, fileInfo(full))

	msg := fmt.Sprintf("applied %d edits to %s", len(p.Edits), p.Path)
	if len(warns) > 0 {
		msg += "\n\n" + strings.Join(warns, "\n\n")
	}
	return agent.ToolResult{
		Content: llmBlock(msg),
	}, nil
}

// applyEdits validates ops, resolves every op's span on the LF-normalized
// buffer (so edits never cascade onto each other), and applies them all at
// once. It returns the LF-space result for diffs, the final file bytes (where
// untouched regions are copied verbatim and each replacement adopts the line
// ending of its neighbouring lines), and post-apply duplication warnings. It
// fails before any change when an op is empty, a no-op, duplicated, missing,
// ambiguous (more than one match without replace_all), or overlaps another
// edit.
func applyEdits(path string, orig string, ops []editOp) (after string, final []byte, warns []string, err error) {
	buf := normalizeToLF(orig)
	for i := range ops { // match in LF space so CRLF oldText never needs a \r
		ops[i].OldText = normalizeToLF(ops[i].OldText)
		ops[i].NewText = normalizeToLF(ops[i].NewText)
	}
	if err := validateEdits(ops); err != nil {
		return "", nil, nil, err
	}

	var spans []matchSpan
	for i := range ops {
		op := &ops[i]
		switch count := strings.Count(buf, op.OldText); {
		case count == 0:
			return "", nil, nil, errors.New(missingError(i+1, path, op.OldText, buf, ops))
		case count > 1 && !op.ReplaceAll:
			return "", nil, nil, errors.New(ambiguousError(i+1, path, op.OldText, buf))
		default: // exactly one match, or replace_all with any positive count
			if op.ReplaceAll {
				for s := 0; ; {
					j := strings.Index(buf[s:], op.OldText)
					if j < 0 {
						break
					}
					s += j
					spans = append(spans, matchSpan{idx: i, s: s, e: s + len(op.OldText)})
					s += len(op.OldText)
				}
			} else {
				s := strings.Index(buf, op.OldText)
				spans = append(spans, matchSpan{idx: i, s: s, e: s + len(op.OldText)})
			}
		}
	}

	// reject overlapping spans across ops before rebuilding the buffer
	slices.SortStableFunc(spans, func(a, b matchSpan) int { return a.s - b.s })
	for i := 1; i < len(spans); i++ {
		if spans[i].s >= spans[i-1].e {
			continue
		}
		a, bb := spans[i-1], spans[i]
		return "", nil, nil, fmt.Errorf("edits %d and %d target overlapping regions in %s; adjust their oldText so each targets a distinct region", a.idx+1, bb.idx+1, path)
	}

	var out strings.Builder // LF rebuild from the original buffer in span order
	last := 0
	var replaced []afterSpan
	for _, sp := range spans {
		out.WriteString(buf[last:sp.s])
		s := out.Len()
		out.WriteString(ops[sp.idx].NewText)
		replaced = append(replaced, afterSpan{idx: sp.idx, s: s, e: out.Len()})
		last = sp.e
	}
	out.WriteString(buf[last:])
	after = out.String()
	return after, rebuild(orig, buf, spans, ops), duplicationWarnings(after, replaced), nil
}

// matchSpan is one replacement's byte range in the LF-normalized buffer.
type matchSpan struct {
	idx int // index of the owning op
	s   int
	e   int
}

// afterSpan is one newText's byte range in the after (LF) buffer, for warning attribution.
type afterSpan struct {
	idx int // owning op index
	s   int
	e   int
}

// rebuild applies spans directly to the original bytes so untouched regions
// keep their exact line endings, with each newText adopting the ending of the
// line it starts on — a mixed-ending file keeps its mix outside the edits.
func rebuild(orig, buf string, spans []matchSpan, ops []editOp) []byte {
	// one walk records each line's start in both spaces plus its ending;
	// normalizeToLF only deletes the \r of a CRLF pair, so bytes map one-to-one
	// inside a line and line starts just shift by the pairs before them
	var starts, nstarts []int
	var crlfs []bool
	o, n := 0, 0
	for i := 0; i < len(orig); i++ {
		if orig[i] != '\n' {
			continue
		}
		crlf := i > 0 && orig[i-1] == '\r'
		starts, nstarts, crlfs = append(starts, o), append(nstarts, n), append(crlfs, crlf)
		if crlf {
			n += i - o // the \r before this \n is dropped by normalization
		} else {
			n += i - o + 1
		}
		o = i + 1
	}
	terminated := strings.HasSuffix(orig, "\n")
	if !terminated && len(orig) > 0 { // trailing line with no terminator
		starts, nstarts, crlfs = append(starts, o), append(nstarts, n), append(crlfs, false)
	}
	toOrig := func(k int) int {
		l := strings.Count(buf[:k], "\n")
		if l >= len(starts) { // EOF on a newline boundary
			return len(orig)
		}
		return starts[l] + (k - nstarts[l])
	}

	var out strings.Builder
	last := 0
	for _, sp := range spans {
		out.WriteString(orig[last:toOrig(sp.s)])
		// the replacement adopts the ending of the line it starts on; a
		// terminator-less last line borrows the one before it
		l := strings.Count(buf[:sp.s], "\n")
		if l == len(crlfs)-1 && !terminated && l > 0 {
			l--
		}
		ending := "\n"
		if l >= 0 && l < len(crlfs) && crlfs[l] {
			ending = "\r\n"
		}
		out.WriteString(restoreLineEndings(ops[sp.idx].NewText, ending))
		last = toOrig(sp.e)
	}
	out.WriteString(orig[last:])
	return []byte(out.String())
}

// validateEdits runs the order-independent intent checks: empty or no-op edits
// and duplicate old texts. Missing/ambiguous/overlapping text is left to
// applyEdits' span resolution on the original buffer.
func validateEdits(ops []editOp) error {
	seen := make(map[string]int)
	for i := range ops {
		op := &ops[i]
		if op.OldText == "" {
			return fmt.Errorf("edit %d: empty oldText; provide the exact text you want replaced", i+1)
		}
		if op.OldText == op.NewText {
			return fmt.Errorf("edit %d: no-op edit, newText equals oldText", i+1)
		}
		if j, dup := seen[op.OldText]; dup {
			return fmt.Errorf("edits %d and %d repeat the same oldText; use replace_all or add context", j+1, i+1)
		}
		seen[op.OldText] = i
	}
	return nil
}

// missingError diagnoses why a zero-match edit failed and guides a retry: name
// the failure, call out an earlier edit whose newText would create it (a cascade),
// report each reliably-detected cause, and offer one closest line for context.
func missingError(idx int, path, old, buf string, ops []editOp) string {
	var b strings.Builder
	fmt.Fprintf(&b, "no match for edit %d in %s.\n", idx, path)
	if c := cascadeIssue(idx, old, ops); c != "" {
		b.WriteString("- " + c + "\n")
	} else if issues := diagnoseNoMatch(old, buf); len(issues) > 0 {
		for _, it := range issues {
			fmt.Fprintf(&b, "- %s\n", it)
		}
	} else {
		b.WriteString("you must provide the oldText exactly as it appears in the file\n")
	}
	if ctx := nearMatch(old, buf); ctx != "" && ctx != "(none)" {
		b.WriteString("closest context:\n" + ctx + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// cascadeIssue reports when an earlier op's newText would create old, which is why
// matching it against the original buffer fails.
func cascadeIssue(idx int, old string, ops []editOp) string {
	for j := 0; j < idx; j++ {
		if strings.Contains(ops[j].NewText, old) {
			return fmt.Sprintf("'%s' is not in the file, but edit %d's newText would create it. Every edit is matched against the original file, never against another edit's output; combine edits %d and %d into one edit",
				old, j+1, j+1, idx+1)
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
		return []string{"your oldText is not in the file — its content differs from every region here; copy it exactly"}
	}
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

	var issues []string
	for k := range seq {
		d := lineWhitespaceDiff(oldLines[seq[k].n-1], bufLines[fileSeq[match+k].n-1])
		if d != "" {
			issues = append(issues, fmt.Sprintf("line %d: %s", fileSeq[match+k].n, d))
		}
		d = blankGapIssue(fileSeq[match+k].n, seq[k].after, fileSeq[match+k].after)
		if d != "" {
			issues = append(issues, d)
		}
	}
	return issues
}

// lineWhitespaceDiff names how a matched pair's whitespace differs. Both lines have
// identical stripped content, so only leading/trailing indentation or inter-word
// spacing can vary.
func lineWhitespaceDiff(oldLine, fileLine string) string {
	ot := strings.TrimSpace(oldLine)
	ft := strings.TrimSpace(fileLine)

	var parts []string

	if ot != ft { // internal word-spacing differs; leading/trailing handled below
		if d := interWordIssue(ft, ot); d != "" {
			parts = append(parts, d) // file is ground truth; state what it has
		}
	}

	if d := indentIssue(leadingWS(oldLine), leadingWS(fileLine)); d != "" {
		parts = append(parts, d)
	}

	oTail := oldLine[len(strings.TrimRightFunc(oldLine, unicode.IsSpace)):]
	fTail := fileLine[len(strings.TrimRightFunc(fileLine, unicode.IsSpace)):]
	switch {
	case oTail != "" && fTail == "": // text has a trailing run the file lacks
		parts = append(parts, fmt.Sprintf("your line ends with %s that is not in the file; remove it", describeRun(oTail)))
	case fTail != "" && !strings.HasSuffix(oldLine, fTail): // file's trailing run text omits
		parts = append(parts, fmt.Sprintf("the file line ends with %s your text omits", describeRun(fTail)))
	}

	return strings.Join(parts, "; ")
}

// leadingWS returns the whitespace prefix of s. Indentation is ASCII space/tab, so
// byte-wise scanning is rune-safe for the prefixes that matter here.
func leadingWS(s string) string {
	i := 0
	for i < len(s) && unicode.IsSpace(rune(s[i])) {
		i++
	}
	return s[:i]
}

// indentIssue describes how want (oldText's indent) and have (the file line's)
// differ: tab-vs-space, or a count difference of the same kind.
func indentIssue(want, have string) string {
	wTabs, wSpaces := countWS(want)
	hTabs, hSpaces := countWS(have)
	switch {
	case wTabs > 0 && hTabs == 0:
		if hSpaces == 0 { // file line has no indentation at all
			return fmt.Sprintf("your text indents with %s but the file line is not indented", plural(wTabs, "tab"))
		}
		return fmt.Sprintf("your text indents with %d tab(s) but the file line uses %d space(s); you must use spaces", wTabs, hSpaces)
	case hTabs > 0 && wTabs == 0:
		if wSpaces == 0 { // your text has no leading whitespace
			return fmt.Sprintf("the file line indents with %s but your text is not indented; match it exactly", plural(hTabs, "tab"))
		}
		return fmt.Sprintf("the file line indents with %d tab(s) but your text uses %d space(s); you must match it exactly", hTabs, wSpaces)

	case wTabs == 0 && hTabs == 0 && wSpaces != hSpaces:
		return fmt.Sprintf("indentation count differs: your text has %d leading spaces, the file line has %d; you must provide that exact count", wSpaces, hSpaces)
	case wTabs > 0 && hTabs > 0 && len(want) != len(have):
		return fmt.Sprintf("tab indentation depth differs: the file line has %s but your text has %s; match the file's exact indentation", describeRun(have), describeRun(want))
	}
	return "" // indistinguishable or already covered
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

// interWordIssue names how the file and oldText space their words apart. Both a
// (file body) and b (text body) have identical stripped content but differ in at
// least one internal whitespace run; report the first difference with exact counts.
func interWordIssue(a, b string) string {
	ar := wsRuns(a)
	br := wsRuns(b)
	n := len(ar)
	if len(br) < n {
		n = len(br)
	}
	for i := 0; i < n; i++ {
		if ar[i] == br[i] {
			continue
		}
		return fmt.Sprintf("spacing between words differs: the file has %s, your text has %s", describeRun(ar[i]), describeRun(br[i]))
	}
	return "" // indistinguishable or already covered by indentation
}

// wsRuns returns the whitespace runs separating a body's leading-trimmed words.
func wsRuns(s string) []string {
	var runs []string
	cur := strings.Builder{}
	for _, r := range s {
		if unicode.IsSpace(r) {
			cur.WriteRune(r)
			continue
		}
		if cur.Len() > 0 { // a word ended; flush the preceding whitespace run
			runs = append(runs, cur.String())
			cur.Reset()
		}
	}
	return runs
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

// dupFinding is one adjacent duplicate region introduced by a replacement:
// contiguous lines [start..end] (inclusive, 0-based) that contain repeated text,
// plus every edit whose replaced span intersects the region.
type dupFinding struct {
	start, end int   // inclusive line indexes of the duplicated region
	opIdx      []int // edits intersecting this region, ascending and unique
}

func nonBlank(s string) bool { return stripSpace(s) != "" }

// allNonBlank reports whether every line is non-blank.
func allNonBlank(ls []string) bool {
	for _, l := range ls {
		if !nonBlank(l) {
			return false
		}
	}
	return true
}

// duplicationWarnings reports adjacent identical lines (1- or 2-line blocks)
// that an edit introduced, so a replacement that re-emits text the file already
// retains is caught in the same round trip. Only duplications whose lines
// intersect a replacement region are reported; pre-existing duplication the
// edit never touched is out of scope. Blank lines never count.
func duplicationWarnings(after string, replaced []afterSpan) []string {
	lines := dropTrailingEmpty(strings.Split(after, "\n"))
	n := len(lines)
	if n == 0 {
		return nil
	}
	lineStart := make([]int, n)
	off := 0
	for i := range lines {
		lineStart[i] = off
		off += len(lines[i]) + 1 // +1 for the newline separator (LF-normalized)
	}

	// find every adjacent equal-sequence occurrence for block sizes 1 and 2, then
	// merge overlapping ones so a contiguous duplicate run reports exactly once.
	type dupOcc struct{ start, end int } // inclusive line indexes
	var occs []dupOcc
	for _, bs := range [2]int{1, 2} {
		for i := 0; i+2*bs <= n; i++ {
			if !allNonBlank(lines[i : i+2*bs]) {
				continue
			}
			if slices.Equal(lines[i:i+bs], lines[i+bs:i+2*bs]) {
				occs = append(occs, dupOcc{i, i + 2*bs - 1})
			}
		}
	}
	slices.SortStableFunc(occs, func(x, y dupOcc) int {
		if x.start != y.start {
			return x.start - y.start
		}
		return x.end - y.end
	})

	type lineSpan struct{ start, end int }
	var spans []lineSpan
	for _, o := range occs {
		if len(spans) == 0 || o.start > spans[len(spans)-1].end {
			spans = append(spans, lineSpan(o))
			continue
		}
		if o.end > spans[len(spans)-1].end {
			spans[len(spans)-1].end = o.end
		}
	}

	// opsOverlap returns every distinct edit whose replaced span intersects [a,b).
	opsOverlap := func(a, b int) []int {
		var ops []int
		for i := range replaced {
			if a < replaced[i].e && b > replaced[i].s {
				ops = append(ops, replaced[i].idx)
			}
		}
		slices.SortFunc(ops, func(x, y int) int { return x - y })
		return slices.Compact(ops)
	}

	var findings []dupFinding
	for _, sp := range spans {
		a := lineStart[sp.start]
		b := lineStart[sp.end] + len(lines[sp.end])
		if ops := opsOverlap(a, b); len(ops) > 0 {
			findings = append(findings, dupFinding{start: sp.start, end: sp.end, opIdx: ops})
		}
	}

	var warns []string
	shownTo := -1 // highest context line index already rendered by an earlier warning
	for i := range findings {
		if i >= 3 {
			warns = append(warns, "... more duplications omitted")
			break
		}
		f := findings[i]
		warns = append(warns, renderDupWarning(lines, f, shownTo))
		if end := min(len(lines)-1, f.end+5); end > shownTo {
			shownTo = end
		}
	}

	return warns
}

// editRef renders an op-index list as "edit N", "edits A and B", or "edits A, B and C".
func editRef(idxs []int) string {
	names := make([]string, len(idxs))
	for i, x := range idxs {
		names[i] = strconv.Itoa(x + 1)
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return "edit " + names[0]
	case 2:
		return "edits " + strings.Join(names, " and ")
	default:
		last := len(names) - 1
		return "edits " + strings.Join(names[:last], ", ") + " and " + names[last]
	}
}

// lineRange renders a 1-based inclusive [a,b] as "X" or "X-Y".
func lineRange(a, b int) string {
	if a == b {
		return strconv.Itoa(a + 1)
	}
	return fmt.Sprintf("%d-%d", a+1, b+1)
}

// dropTrailingEmpty removes the empty element a newline-terminated split leaves,
// so it never renders as a numbered blank row at file end.
func dropTrailingEmpty(ls []string) []string {
	if n := len(ls); n > 0 && ls[n-1] == "" {
		return ls[:n-1]
	}
	return ls
}

// renderDupWarning renders one duplication warning with a header and numbered context.
func renderDupWarning(lines []string, f dupFinding, shownTo int) string {
	var b strings.Builder
	// name both halves when the region splits evenly, else state the whole span.
	desc := fmt.Sprintf("lines %s contain duplicated content", lineRange(f.start, f.end))
	if half := (f.end - f.start + 1); half%2 == 0 {
		m := f.start + half/2 - 1
		if slices.Equal(lines[f.start:m+1], lines[m+1:f.end+1]) {
			desc = fmt.Sprintf("lines %s and %s are identical", lineRange(f.start, m), lineRange(m+1, f.end))
		}
	}
	fmt.Fprintf(&b, "WARN: Duplicate text detected after edit (%s: %s), ensure the following is correct:\n",
		editRef(f.opIdx), desc)
	start := max(0, f.start-5)        // clamp to file bounds
	end := min(len(lines)-1, f.end+5) // inclusive last line shown

	for j := start; j <= end; j++ {
		if j <= shownTo && (j < f.start || j > f.end) {
			continue // context already rendered by an earlier finding's window
		}
		prefix := ""
		if j >= f.start && j <= f.end {
			prefix = ">> "
		}
		fmt.Fprintf(&b, "%6d\t%s%s\n", j+1, prefix, strutil.Clip(lines[j], MaxLineRunes))
	}

	return strings.TrimRight(b.String(), "\n")
}

// nearMatch finds the closest matching line of buf to old, as read-only context.
func nearMatch(old, buf string) string {
	tokens := strings.Fields(old)
	if len(tokens) == 0 {
		return "(none)"
	}
	bestLine, bestScore := "", -1
	for i, line := range strings.Split(buf, "\n") {
		score := overlap(line, tokens)
		if score > bestScore {
			bestScore = score
			bestLine = fmt.Sprintf("line %d: %s", i+1, strutil.Clip(strings.TrimSpace(line), MaxLineRunes))
		}
	}
	return bestLine
}

// overlap counts how many of tokens appear in line.
func overlap(line string, tokens []string) int {
	n := 0
	for _, tok := range tokens {
		if strings.Contains(line, tok) {
			n++
		}
	}
	return n
}

// ambiguousError names the count and gives two concrete retry options, then lists
// matching lines (capped) so the model can pick unique context.
func ambiguousError(idx int, path string, old, buf string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "edit %d matches %d occurrences in %s; widen its oldText with surrounding context or set replace_all:true.\n", idx, strings.Count(buf, old), path)
	lines := strings.Split(buf, "\n")
	for i, line := range lines {
		if !strings.Contains(line, old) {
			continue
		}
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, strutil.Clip(strings.TrimSpace(line), MaxLineRunes))
		if b.Len() > 800 {
			b.WriteString("... more matches omitted; add unique context or set replace_all:true\n")
			break
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
