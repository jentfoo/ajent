package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
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
// applied, so DryRun and Preview share one code path.
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
	after, err := applyEdits(p.Path, string(data), p.Edits)
	return Change{Path: p.Path, Before: string(data), After: after}, err
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
	return "Edit a single file using exact text replacement. Every edits[].oldText must match a unique region of the file, or set replace_all. All edits apply atomically or none do."
}
func (t *editTool) Schema() llm.ToolSchema { return llm.ToolSchema{Parameters: SchemaOf[editParams]()} }
func (t *editTool) Mode() agent.ExecutionMode {
	return agent.ModeSerial
}

// Execute applies every edit against an in-memory buffer, then writes once so a
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
	buf := string(data)

	applied, err := applyEdits(p.Path, buf, p.Edits)
	if err != nil {
		return resultErr(err.Error()), nil
	}

	final := []byte(applied)
	if err := config.WriteFileAtomic(full, final, writePerm(full)); err != nil {
		return resultErr("edit: " + err.Error()), nil
	}
	t.tracker.Observe(full, final, fileInfo(full))

	return agent.ToolResult{
		Content: llmBlock(fmt.Sprintf("applied %d edits to %s", len(p.Edits), p.Path)),
	}, nil
}

// applyEdits validates ops against buf then applies them in order. It fails before
// any change when an op's old text is empty, missing, ambiguous (more than one match
// without replace_all), duplicated across edits, overlapping another edit, or a no-op.
func applyEdits(path string, buf string, ops []editOp) (string, error) {
	if err := validateEdits(buf, ops); err != nil {
		return "", err
	}
	for i := range ops {
		op := &ops[i]
		count := strings.Count(buf, op.OldText)
		switch {
		case count == 0:
			return "", errors.New(notFoundError(i+1, path, ops[i].OldText, buf))
		case count > 1 && !op.ReplaceAll:
			return "", errors.New(ambiguousError(i+1, path, op.OldText, buf))
		default: // exactly one match, or replace_all with any positive count
			if op.ReplaceAll {
				buf = strings.ReplaceAll(buf, op.OldText, op.NewText)
			} else {
				buf = strings.Replace(buf, op.OldText, op.NewText, 1)
			}
		}
	}
	return buf, nil
}

// matchSpan is one non-replace edit's byte range in the original buffer.
type matchSpan struct {
	idx int // index of the owning op
	s   int
	e   int
}

// validateEdits runs the order-independent intent checks: empty or no-op edits,
// duplicate old texts and overlapping non-replace regions. Missing/ambiguous text
// is left to applyEdits' per-op loop, which sees the evolving buffer.
func validateEdits(buf string, ops []editOp) error {
	seen := make(map[string]int)
	var spans []matchSpan
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
		if op.ReplaceAll {
			continue // many spans; overlap is only checked for single replacements
		}
		s := strings.Index(buf, op.OldText)
		if s < 0 {
			continue // missing is reported by the apply loop with full guidance
		}
		e := s + len(op.OldText)
		for _, p := range spans {
			if s < p.e && p.s < e { // regions share at least one byte in the original text
				return fmt.Errorf("edits %d and %d target overlapping regions; adjust their oldText so each targets a distinct region", p.idx+1, i+1)
			}
		}
		spans = append(spans, matchSpan{i, s, e})
	}
	return nil
}

// notFoundError diagnoses why a zero-match edit failed and guides a retry:
// name the failure, report each reliably-detected cause (line endings,
// indentation type/count), and offer one closest line for context.
func notFoundError(idx int, path, old, buf string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "no match for edit %d in %s.\n", idx, path)
	issues := diagnoseNoMatch(old, buf)
	if len(issues) == 0 {
		b.WriteString("you must provide the oldText exactly as it appears in the file\n")
	} else {
		for _, it := range issues {
			fmt.Fprintf(&b, "- %s\n", it)
		}
	}
	if ctx := nearMatch(old, buf); ctx != "" && ctx != "(none)" {
		b.WriteString("closest context:\n" + ctx + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// diagnoseNoMatch inspects why old is absent from buf and reports each reliably
// detectable cause: line-ending style, then per-line indentation/spacing when the
// words are all present (a pure-spacing mismatch). Returns nil when nothing
// reliable can be said.
func diagnoseNoMatch(old, buf string) []string {
	compact := stripSpace(old)
	if compact == "" {
		return nil // only-whitespace oldText: no anchor to reason about
	}

	nlIssue := newlineStyleIssue(old, buf)
	var issues []string
	if nlIssue != "" {
		issues = append(issues, nlIssue)
	}

	switch {
	case strings.Contains(stripSpace(buf), compact) && nlIssue == "":
		// every word present: a pure-spacing mismatch, name it per line when we can
		specific := lineWhitespaceIssues(old, buf)
		if len(specific) == 0 {
			issues = append(issues, "your words are all in the file but separated by different whitespace; you must match it exactly")
		} else {
			issues = append(issues, specific...)
		}
	case strings.Contains(strings.ToLower(stripSpace(buf)), strings.ToLower(compact)) && nlIssue == "":
		// words and order present ignoring case: only letter casing differs
		issues = append(issues, "the text matches the file only if you ignore letter case; your oldText's capitalization differs — copy it exactly")
	case nlIssue == "":
		// content genuinely absent, not just spacing
		if stripSpace(buf) == "" {
			issues = append(issues, "the file appears empty or whitespace-only")
		} else {
			issues = append(issues, "the text is not in the file — its words differ; provide oldText exactly as shown below")
		}
	}
	return issues
}

// newlineStyleIssue reports when old and buf disagree on CRLF vs LF.
func newlineStyleIssue(old, buf string) string {
	switch {
	case strings.Contains(buf, "\r\n") && !strings.Contains(old, "\r\n"):
		return "the file uses CRLF (\\r\\n) line endings but your oldText has LF; you must include the \\r before each newline"
	case !strings.Contains(buf, "\r\n") && strings.Contains(old, "\r\n"):
		return "your oldText has \\r\\n newlines but the file is LF-only; you must use plain \\n"
	}
	return ""
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
		return "tab indentation depth differs; match the file's exact number of tabs"
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
			bestLine = fmt.Sprintf("line %d: %s", i+1, capLine(strings.TrimSpace(line)))
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
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, capLine(strings.TrimSpace(line)))
		if b.Len() > 800 {
			b.WriteString("... more matches omitted; add unique context or set replace_all:true\n")
			break
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
