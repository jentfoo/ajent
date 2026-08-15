package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
func (t *editTool) resolveApply(call agent.ToolCall) (path string, before, after string, err error) {
	var p editParams
	err = decode(call.Input, &p)
	if err != nil {
		return "", "", "", errors.New("bad args: " + err.Error())
	}
	full, err := t.policy.Resolve(p.Path)
	if err != nil {
		return "", "", "", err
	}
	data, err := readAllFile(full) // missing or unreadable counts as will-fail
	if err != nil {
		return "", "", "", err
	}
	after, err = applyEdits(p.Path, string(data), p.Edits)
	return p.Path, string(data), after, err
}

// DryRun reports whether an edit call would fail before it runs: the file is
// missing or unreadable, or applyEdits rejects any op. The barrier uses it to skip
// a prompt for a doomed call and let Execute return its natural error.
func (t *editTool) DryRun(call agent.ToolCall) error {
	_, _, _, err := t.resolveApply(call)
	return err
}

// Preview returns the path with the file's current text and what it would become,
// so an approval dialog can show a diff of the change before allowing it.
func (t *editTool) Preview(call agent.ToolCall) (path, before, after string, err error) {
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
// failure mid-list leaves the file byte-identical.
func (t *editTool) Execute(ctx context.Context, call agent.ToolCall, out agent.Output) (agent.ToolResult, error) {
	out = ensureOutput(out)
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
	if ck := t.tracker.Check(full); ck != nil { // stale or unread: re-read first
		return resultErr("edit refused: " + ck.Error()), nil
	}

	data, _ := readAllFile(full)
	buf := string(data)

	applied, err := applyEdits(p.Path, buf, p.Edits)
	if err != nil {
		return resultErr(err.Error()), nil
	}

	final := []byte(applied)
	if err := config.WriteFileAtomic(full, final, 0o644); err != nil {
		return resultErr("edit: " + err.Error()), nil
	}
	t.tracker.Observe(full, final, fileInfo(full))
	out.Diff(p.Path, string(data), buf)

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
				return fmt.Errorf("edits %d and %d target overlapping regions", p.idx+1, i+1)
			}
		}
		spans = append(spans, matchSpan{i, s, e})
	}
	return nil
}

// notFoundError guides a retry after zero matches: name the failure, say why
// exact copy matters, and offer one closest line for context.
func notFoundError(idx int, path, old, buf string) string {
	return fmt.Sprintf("no match for edit %d in %s; whitespace/newlines must match exactly. Include a unique surrounding line if the text is ambiguous.\nclosest context:\n%s",
		idx, path, nearMatch(old, buf))
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
