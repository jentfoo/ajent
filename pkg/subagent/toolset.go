package subagent

import (
	"slices"
	"strings"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// ToolSource is the parent registry's view a child tool set is built from.
type ToolSource interface {
	All() []agent.Tool
	ReadOnly(name string) bool
}

// readOnlyBuiltins are the built-in tools a child may call, by name. find/grep/ls
// ship disabled in the parent but must still reach a child, which has no shell.
// Kept in step with tools.ReadOnlyBuiltins, which this package may not import.
var readOnlyBuiltins = []string{"read", "grep", "find", "ls", "git_status", "git_log", "git_show", "git_diff"}

// gitToolNames are the read-only built-ins that need a repository at the
// child's cwd, withheld when the cwd is not inside a work tree.
var gitToolNames = []string{"git_status", "git_log", "git_show", "git_diff"}

// childBarred names tools no child may resolve regardless of read-only metadata or
// enable state; the parent still auto-runs ask_user without approval under allow-read.
var childBarred = bulk.SliceToSet([]string{
	"ask_user",                                                  // interactive: needs an endpoint a sub-agent has no right to reach
	"dev_implement", "dev_review", "dev_revise", "dev_complete", // plan control tools, phase-mutating (pkg/plan)
})

// isChildBarred reports whether name must never reach a child.
func isChildBarred(name string) bool {
	_, ok := childBarred[name]
	return ok
}

// isGitTool reports whether name is one of the repo-gated git readers.
func isGitTool(name string) bool {
	return slices.Contains(gitToolNames, name)
}

// childTools returns the read-only tools a child may call: the read-only built-ins
// plus any registry-marked read-only tool, never agent_* or childBarred. Parent enable state is
// ignored so find/grep/ls reach even a disabled parent. The git readers need a repository:
// inRepo false withholds them, so git capability is never advertised where it cannot apply.
func childTools(src ToolSource, inRepo bool) []agent.Tool {
	return bulk.SliceFilter(func(t agent.Tool) bool {
		name := t.Name()
		if strings.HasPrefix(name, "agent_") || isChildBarred(name) { // structural bar: nothing configures past it
			return false
		}
		if !slices.Contains(readOnlyBuiltins, name) && !src.ReadOnly(name) {
			return false
		}
		return !isGitTool(name) || inRepo // repo-context gate
	}, src.All())
}

// toolSet is a fixed read-only view over a child's resolved tools.
type toolSet struct {
	tools  []agent.Tool
	byName map[string]agent.Tool
}

// newToolSet indexes resolved tools by name. First wins on a repeated name,
// matching the parent registry's lookup.
func newToolSet(resolved []agent.Tool) *toolSet {
	byName := make(map[string]agent.Tool, len(resolved))
	for _, t := range resolved {
		if _, ok := byName[t.Name()]; !ok {
			byName[t.Name()] = t
		}
	}
	return &toolSet{tools: resolved, byName: byName}
}

func (t *toolSet) Get(name string) (agent.Tool, bool) {
	x, ok := t.byName[name]
	return x, ok
}

func (t *toolSet) Schemas() []llm.ToolSchema {
	out := make([]llm.ToolSchema, len(t.tools))
	for i, x := range t.tools {
		schema := x.Schema()
		out[i] = llm.ToolSchema{Name: x.Name(), Description: x.Description(),
			Parameters: schema.Parameters, Deferred: schema.Deferred, Grammar: schema.Grammar}
	}
	return out
}

func (t *toolSet) Names() []string {
	out := make([]string, len(t.tools))
	for i, x := range t.tools {
		out[i] = x.Name()
	}
	return out
}

var _ agent.ToolSet = (*toolSet)(nil)
