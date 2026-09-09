package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
)

// editOp is one string replacement.
type editOp struct {
	OldText    string `json:"oldText" desc:"text to replace; must identify a unique region unless replace_all is set"`
	NewText    string `json:"newText" desc:"replacement text"`
	ReplaceAll bool   `json:"replace_all,omitempty" desc:"replace every occurrence instead of exactly one"`
}

// editParams carries a list of edits applied atomically to one file.
type editParams struct {
	Path  string   `json:"path"`
	Edits []editOp `json:"edits" desc:"ordered replacements; all apply or none do"`
}

// editTarget names the file an apply runs against and what the session knows
// about it, so a diagnostic can say why the text no longer matches.
type editTarget struct {
	Path   string      // the model-supplied path, for messages
	Edited []lineRange // lines this session already wrote; tierFuzzy will not heal them
}

// editTool applies string edits to a single file, all-or-nothing. The array
// form gives one round trip for a multi-site refactor.
type editTool struct {
	policy  PathPolicy
	tracker *Tracker
}

var _ agent.Tool = (*editTool)(nil)
var _ DryRunner = (*editTool)(nil)
var _ Previewer = (*editTool)(nil)

// editOutcome is what one apply produced: the LF-space text on both sides for
// rendering, the bytes to write, and what the model should be told.
type editOutcome struct {
	before string   // LF-space original
	after  string   // LF-space result
	final  []byte   // bytes to write, original line endings preserved
	notes  []string // why the result may not be what was asked for
	// review gates the result diff; set when a tier fired, several sites were
	// hit, or the apply duplicated text.
	review bool
	edited []lineRange // lines this apply wrote, in the rebuilt text
	shifts []lineShift // how this apply moved the lines it did not write
}

// resolveApply resolves the path and returns the current file text with the edits
// applied in LF space, so DryRun and Preview share one code path.
func (t *editTool) resolveApply(call agent.ToolCall) (Change, error) {
	p, err := decodeEditParams(call.Input)
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
	o, aerr := applyEdits(t.target(p.Path, full, data), string(data), p.Edits)
	return Change{Path: p.Path, Before: normalizeToLF(string(data)), After: o.after}, aerr
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
	return "Edit a single file by replacing text. Every edits[].oldText must identify a unique region of the file, or set replace_all. Copy oldText from the file exactly. All edits apply atomically or none do. Built-in safeguards make this safe for agent use; prefer `edit` over bash tools to modify files."
}
func (t *editTool) Schema() llm.ToolSchema { return llm.ToolSchema{Parameters: SchemaOf[editParams]()} }
func (t *editTool) Mode() agent.ExecutionMode {
	return agent.ModeSerial
}

// Execute applies every edit against the original buffer, then writes once so a
// failure mid-list leaves the file byte-identical. The diff is rendered by the
// guard wrapper before this runs.
func (t *editTool) Execute(ctx context.Context, call agent.ToolCall, out agent.Output) (agent.ToolResult, error) {
	p, err := decodeEditParams(call.Input)
	if err != nil {
		return resultErr("bad args: " + err.Error()), nil
	}
	full, err := t.policy.Resolve(p.Path)
	if err != nil {
		return resultErr(err.Error()), nil
	}

	// The file is re-read here; a stale or changed file simply fails to match in applyEdits.
	data, err := os.ReadFile(full)
	if err != nil {
		return resultErr("edit: " + err.Error()), nil
	}
	tgt := t.target(p.Path, full, data)
	o, err := applyEdits(tgt, string(data), p.Edits)
	if err != nil {
		return resultErr(err.Error()), nil
	}

	if err := config.WriteFileAtomic(full, o.final, writePerm(full)); err != nil {
		return resultErr("edit: " + err.Error()), nil
	}
	t.tracker.Observe(full, o.final, fileInfo(full)) // drops the ranges below
	t.tracker.markEdited(full, append(shiftRanges(tgt.Edited, o.shifts), o.edited...))

	return agent.ToolResult{
		Content: llmBlock(editReport(p.Path, len(p.Edits), o)),
	}, nil
}

// target describes the file for diagnostics and for the tiers that must not
// rewrite what this session already edited. data is the file's current content,
// which the recorded lines are only meaningful against.
func (t *editTool) target(path, full string, data []byte) editTarget {
	return editTarget{Path: path, Edited: t.tracker.editedFor(full, data)}
}

// decodeEditParams decodes edit's arguments, tolerating what models emit in place
// of the declared schema: a double-encoded argument object, edits as one JSON
// string, or a single edit where an array is declared.
func decodeEditParams(raw json.RawMessage) (editParams, error) {
	var shim struct {
		Path  string          `json:"path"`
		Edits json.RawMessage `json:"edits"`
	}
	if err := decode(unquoteJSON(raw), &shim); err != nil {
		return editParams{}, err
	}

	p := editParams{Path: shim.Path}
	body := unquoteJSON(shim.Edits)
	switch firstByte(body) {
	case 0: // absent or null; the empty-edits check reports it
		return p, nil
	case '{': // a lone edit where an array is declared
		var op editOp
		if err := decode(body, &op); err != nil {
			return editParams{}, err
		}
		p.Edits = []editOp{op}
	default:
		if err := decode(body, &p.Edits); err != nil {
			return editParams{}, err
		}
	}
	return p, nil
}

// unquoteJSON unwraps a JSON string that itself holds JSON, which is how a model
// double-encodes an argument it should have sent as an object or array.
func unquoteJSON(raw json.RawMessage) json.RawMessage {
	if firstByte(raw) != '"' {
		return raw
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return raw
	}
	return json.RawMessage(s)
}

// firstByte returns raw's first non-space byte, or 0 when it holds none.
func firstByte(raw json.RawMessage) byte {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 {
		return trimmed[0]
	}
	return 0
}

// applyEdits validates ops, resolves every op's span on the LF-normalized
// buffer (so edits never cascade onto each other), and applies them all at
// once. Untouched regions are copied verbatim and each replacement adopts the
// line ending of its neighbouring lines. It fails before any change when an op
// is empty, duplicated, missing, ambiguous (more than one match without
// replace_all), a no-op, or overlaps another edit.
func applyEdits(t editTarget, orig string, ops []editOp) (editOutcome, error) {
	buf := normalizeToLF(orig)
	for i := range ops { // match in LF space so CRLF oldText never needs a \r
		ops[i].OldText = normalizeToLF(ops[i].OldText)
		ops[i].NewText = normalizeToLF(ops[i].NewText)
	}
	if err := validateEdits(ops); err != nil {
		return editOutcome{}, err
	}

	var spans []matchSpan
	var notes []string
	var review bool
	wrote := make([]string, len(ops)) // each single-site op's written text
	for i := range ops {
		op := &ops[i]
		ms, tier := findMatches(buf, op.OldText, op.NewText, t.Edited)
		switch {
		case len(ms) == 0:
			return editOutcome{}, errors.New(missingError(i+1, t, op.OldText, buf, ops))
		case len(ms) > 1 && !op.ReplaceAll:
			return editOutcome{}, errors.New(ambiguousError(i+1, t.Path, op.OldText, buf, ms))
		}
		// an edit that writes nothing: newText repeated as oldText, or a tier that
		// matched oldText onto text newText already holds
		if !slices.ContainsFunc(ms, func(m match) bool { return m.repl != buf[m.s:m.e] }) {
			if op.OldText == op.NewText {
				return editOutcome{}, fmt.Errorf("edit %d: oldText and newText are identical, so this edit would change nothing; oldText must be the text the file holds now, newText the text you want",
					i+1)
			}
			return editOutcome{}, fmt.Errorf("edit %d: newText is already what %s holds at that region, so this edit would change nothing; read it again to see the current text",
				i+1, t.Path)
		}
		if tier != tierExact { // name the difference, so the next edit is exact
			notes = append(notes, tierNote(i+1, tier, op.OldText, buf[ms[0].s:ms[0].e]))
			review = true
		}
		if op.ReplaceAll && len(ms) > 1 {
			notes = append(notes, fmt.Sprintf("edit %d: replaced %d occurrences", i+1, len(ms)))
			review = true
		}
		if !op.ReplaceAll {
			ms = ms[:1]
			wrote[i] = ms[0].repl
		}
		for _, m := range ms {
			spans = append(spans, matchSpan{idx: i, s: m.s, e: m.e, repl: m.repl})
		}
	}

	// reject overlapping spans across ops before rebuilding the buffer
	slices.SortStableFunc(spans, func(a, b matchSpan) int { return a.s - b.s })
	for i := 1; i < len(spans); i++ {
		if spans[i].s >= spans[i-1].e {
			continue
		}
		a, bb := spans[i-1], spans[i]
		return editOutcome{}, fmt.Errorf("edits %d and %d target overlapping regions in %s; adjust their oldText so each targets a distinct region", a.idx+1, bb.idx+1, t.Path)
	}

	var out strings.Builder // LF rebuild from the original buffer in span order
	var written [][2]int    // each replacement's offsets in the rebuilt text
	last := 0
	for _, sp := range spans {
		out.WriteString(buf[last:sp.s])
		at := out.Len()
		out.WriteString(sp.repl)
		written = append(written, [2]int{at, out.Len()})
		last = sp.e
	}
	out.WriteString(buf[last:])
	after := out.String()

	// lines recorded against the rebuilt text, which the next edit matches on,
	// plus how far each span moved everything below it
	beforeStarts := lineStarts(strings.Split(buf, "\n"))
	afterStarts := lineStarts(strings.Split(after, "\n"))
	edited := make([]lineRange, 0, len(written))
	var shifts []lineShift
	for i, w := range written {
		wrote := spanLines(afterStarts, after, w[0], w[1])
		took := spanLines(beforeStarts, buf, spans[i].s, spans[i].e)
		edited = append(edited, wrote)
		if d := (wrote.to - wrote.from) - (took.to - took.from); d != 0 {
			shifts = append(shifts, lineShift{at: took.to, delta: d})
		}
	}

	for i := range ops {
		if n := duplicateNote(i+1, ops[i], wrote[i], after); n != "" {
			notes = append(notes, n)
			review = true
		}
	}
	if n := seamNote(after, edited); n != "" {
		notes = append(notes, n)
		review = true
	}

	return editOutcome{
		before: buf,
		after:  after,
		final:  rebuild(orig, buf, spans),
		notes:  notes,
		edited: edited,
		shifts: shifts,
		review: review,
	}, nil
}

// spanLines returns the half-open line span text[s:e] occupies, given the line
// offsets of text.
func spanLines(starts []int, text string, s, e int) lineRange {
	to := lineOf(starts, e)
	if e > 0 && text[e-1] == '\n' {
		to-- // a trailing newline ends the line before it
	}
	return lineRange{lineOf(starts, s), to + 1}
}

// matchSpan is one replacement's byte range in the LF-normalized buffer, with
// the text to write there. The text is the op's newText unless a match tier
// reindented it to follow the file.
type matchSpan struct {
	idx  int // index of the owning op
	s    int
	e    int
	repl string
}

// rebuild applies spans directly to the original bytes so untouched regions
// keep their exact line endings, with each replacement adopting the ending of
// the line it starts on — a mixed-ending file keeps its mix outside the edits.
func rebuild(orig, buf string, spans []matchSpan) []byte {
	// one walk records each line's start in both spaces plus its ending;
	// normalizeToLF only deletes the \r of a CRLF pair, so bytes map one-to-one
	// inside a line and line starts just shift by the pairs before them
	var starts, nstarts []int
	var crlfs []bool
	var o, n int
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
	// spans arrive sorted and never overlap, so one cursor walks buf instead of
	// re-counting newlines from the start for every lookup
	var cur, line int
	lineAt := func(k int) int {
		line += strings.Count(buf[cur:k], "\n")
		cur = k
		return line
	}
	toOrig := func(l, k int) int {
		if l >= len(starts) { // EOF on a newline boundary
			return len(orig)
		}
		return starts[l] + (k - nstarts[l])
	}

	var out strings.Builder
	last := 0
	for _, sp := range spans {
		l := lineAt(sp.s)
		out.WriteString(orig[last:toOrig(l, sp.s)])
		// the replacement adopts the ending of the line it starts on; a
		// terminator-less last line borrows the one before it
		if l == len(crlfs)-1 && !terminated && l > 0 {
			l--
		}
		ending := "\n"
		if l >= 0 && l < len(crlfs) && crlfs[l] {
			ending = "\r\n"
		}
		out.WriteString(restoreLineEndings(sp.repl, ending))
		last = toOrig(lineAt(sp.e), sp.e)
	}
	out.WriteString(orig[last:])
	return []byte(out.String())
}

// validateEdits runs the order-independent intent checks: an empty list, empty
// oldText, and duplicate old texts. Missing/ambiguous/overlapping text and no-op
// edits are left to applyEdits' span resolution on the original buffer.
func validateEdits(ops []editOp) error {
	if len(ops) == 0 {
		return errors.New("edit requires at least one entry in edits")
	}
	seen := make(map[string]int)
	for i := range ops {
		op := &ops[i]
		if op.OldText == "" {
			return fmt.Errorf("edit %d: empty oldText; provide the exact text you want replaced", i+1)
		}
		if j, dup := seen[op.OldText]; dup {
			return fmt.Errorf("edits %d and %d repeat the same oldText; use replace_all or add context", j+1, i+1)
		}
		seen[op.OldText] = i
	}
	return nil
}
