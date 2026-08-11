package tools

import (
	"context"
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

	for i := range p.Edits {
		op := &p.Edits[i]
		if op.OldText == "" {
			return resultErr(fmt.Sprintf("edit %d has an empty oldText; exact text is required", i+1)), nil
		}
		count := strings.Count(buf, op.OldText)
		switch {
		case count == 0:
			return resultErr(fmt.Sprintf("no match for edit %d in %s\n%s", i+1, p.Path,
				nearMatch(p.Edits[i].OldText, buf))), nil
		case count > 1 && !op.ReplaceAll:
			return resultErr(ambiguousError(i+1, op.OldText, buf)), nil
		default: // exactly one match, or replace_all with any positive count
			if op.ReplaceAll {
				buf = strings.ReplaceAll(buf, op.OldText, op.NewText)
			} else {
				buf = strings.Replace(buf, op.OldText, op.NewText, 1)
			}
		}
	}

	final := []byte(buf)
	if err := config.WriteFileAtomic(full, final, 0o644); err != nil {
		return resultErr("edit: " + err.Error()), nil
	}
	t.tracker.Observe(full, final, fileInfo(full))
	out.Diff(p.Path, string(data), buf)

	return agent.ToolResult{
		Content: llmBlock(fmt.Sprintf("applied %d edits to %s", len(p.Edits), p.Path)),
	}, nil
}

// nearMatch finds the line of buf with the most token overlap with old, for an
// actionable zero-match error.
func nearMatch(old, buf string) string {
	tokens := strings.Fields(old)
	if len(tokens) == 0 {
		return "no tokens to match against"
	}
	bestLine, bestScore := "", -1
	for i, line := range strings.Split(buf, "\n") {
		score := overlap(line, tokens)
		if score > bestScore {
			bestScore = score
			bestLine = fmt.Sprintf("line %d: %s", i+1, trimToMax(strings.TrimSpace(line)))
		}
	}
	return bestLine + "\n(try matching the line above exactly)"
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

// ambiguousError describes a multi-match so the model can pick replace_all or
// tighten old.
func ambiguousError(idx int, old, buf string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d occurrences of edit %d's old text in the file:\n", strings.Count(buf, old), idx)
	lines := strings.Split(buf, "\n")
	for i, line := range lines {
		if !strings.Contains(line, old) {
			continue
		}
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, trimToMax(strings.TrimSpace(line)))
		if b.Len() > 800 {
			b.WriteString("... more occurrences omitted; use replace_all to apply everywhere\n")
			break
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// trimToMax shortens s at MaxLineChars.
func trimToMax(s string) string { return capLine(s) }
