package tools

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// lsParams is the model-facing parameter block for ls.
type lsParams struct {
	Path  string `json:"path,omitempty" desc:"directory to list; default the session cwd"`
	Limit int    `json:"limit,omitempty" desc:"max entries to return"`
}

// lsTool lists one directory's entries, sorted alphabetically with a '/' suffix
// on directories. Registered off by default: it exists for the sub-agent, which
// has no shell.
type lsTool struct {
	policy    PathPolicy
	sessionID string // names the spill directory for long results
}

var _ agent.Tool = (*lsTool)(nil)

func (t *lsTool) Name() string { return "ls" }

func (t *lsTool) Label(agent.ToolCall) string {
	return "ls"
}

func (t *lsTool) Description() string {
	return "List directory contents, or files matching a wildcard pattern. Returns entries sorted alphabetically, with '/' suffix for directories."
}
func (t *lsTool) Schema() llm.ToolSchema { return llm.ToolSchema{Parameters: SchemaOf[lsParams]()} }
func (t *lsTool) Mode() agent.ExecutionMode {
	return agent.ModeParallel
}

// selfBounding: ls bounds and spills its own results.
func (*lsTool) selfBounding() {}

// Execute lists the directory bounded by the limit. The complete listing spills
// to disk when the bound cuts it, so the model can page the file.
func (t *lsTool) Execute(ctx context.Context, call agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	var p lsParams
	if err := decode(call.Input, &p); err != nil {
		return resultErr("bad args: " + err.Error()), nil
	}
	full, err := t.policy.Resolve(argPath(p.Path))
	if err != nil {
		return resultErr(err.Error()), nil
	}

	// a wildcard pattern is not a real directory: list the files it matches.
	if HasGlob(full) {
		return t.listMatches(full, p.Limit), nil
	}

	entries, err := os.ReadDir(full) // ReadDir returns entries sorted by name
	if err != nil {
		return resultErr("ls: " + err.Error()), nil
	}

	var b strings.Builder
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		b.WriteString(name + "\n")
	}
	text, _ := truncateOutput(t.sessionID, "ls", b.String(), lsLimit(p.Limit), lsPaging(p.Limit))
	return agent.ToolResult{Content: llmBlock(text), Display: text}, nil
}

// listMatches lists the files a wildcard pattern matches, sorted with relative
// paths when under Cwd. An empty match set reports an error so a mistyped glob
// is never mistaken for an empty directory.
func (t *lsTool) listMatches(pattern string, limit int) agent.ToolResult {
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return resultErr("ls: nothing matches " + relTo(t.policy.Cwd, pattern))
	}
	slices.Sort(matches)

	var b strings.Builder
	for _, m := range matches {
		name := relTo(t.policy.Cwd, m)
		if fi, err := os.Stat(m); err == nil && fi.IsDir() {
			name += "/"
		}
		b.WriteString(name + "\n")
	}
	text, _ := truncateOutput(t.sessionID, "ls", b.String(), lsLimit(limit), lsPaging(limit))
	return agent.ToolResult{Content: llmBlock(text), Display: text}
}

// lsLimit narrows the bound's line axis to an explicit limit; the tool bound
// stays the hard cap either way.
func lsLimit(limit int) Limit {
	lim := LsResultLimit()
	if limit > 0 && limit < lim.Lines {
		lim.Lines = limit
	}
	return lim
}

// lsPaging names how to see more: raising limit only helps below the bound.
func lsPaging(limit int) string {
	if limit > 0 && limit < LsResultLimit().Lines {
		return "narrow the path or raise limit"
	}
	return "narrow the path"
}
