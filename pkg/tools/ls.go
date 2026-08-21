package tools

import (
	"context"
	"fmt"
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
	policy PathPolicy
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

// Execute lists the directory bounded by the limit.
func (t *lsTool) Execute(ctx context.Context, call agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	var p lsParams
	if err := decode(call.Input, &p); err != nil {
		return resultErr("bad args: " + err.Error()), nil
	}
	full, err := t.policy.Resolve(argPath(p.Path))
	if err != nil {
		return resultErr(err.Error()), nil
	}

	max := p.Limit
	if max <= 0 {
		max = LsResultLimit().Lines
	}

	// a wildcard pattern is not a real directory: list the files it matches.
	if HasGlob(full) {
		return t.listMatches(full, max), nil
	}

	entries, err := os.ReadDir(full) // ReadDir returns entries sorted by name
	if err != nil {
		return resultErr("ls: " + err.Error()), nil
	}

	var b strings.Builder
	for i, e := range entries {
		if i >= max {
			fmt.Fprintf(&b, "... %d more entries; raise limit or narrow the path\n", len(entries)-max)
			break
		}
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		b.WriteString(name + "\n")
	}

	trimmed := strings.TrimRight(b.String(), "\n")
	// Display mirrors the model-visible text so history shows head+collapse.
	return agent.ToolResult{Content: llmBlock(trimmed), Display: trimmed}, nil
}

// listMatches lists the files a wildcard pattern matches, sorted with relative
// paths when under Cwd. An empty match set reports an error so a mistyped glob
// is never mistaken for an empty directory.
func (t *lsTool) listMatches(pattern string, max int) agent.ToolResult {
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return resultErr("ls: nothing matches " + relTo(t.policy.Cwd, pattern))
	}
	slices.Sort(matches)

	var b strings.Builder
	for i, m := range matches {
		if i >= max {
			fmt.Fprintf(&b, "... %d more files matched; narrow the pattern\n", len(matches)-max)
			break
		}
		name := relTo(t.policy.Cwd, m)
		if fi, err := os.Stat(m); err == nil && fi.IsDir() {
			name += "/"
		}
		b.WriteString(name + "\n")
	}

	trimmed := strings.TrimRight(b.String(), "\n")
	return agent.ToolResult{Content: llmBlock(trimmed), Display: trimmed}
}
