package subagent

import (
	"slices"
	"strings"

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
// child's cwd; they are withheld when the cwd is not inside a work tree.
var gitToolNames = []string{"git_status", "git_log", "git_show", "git_diff"}

// isGitTool reports whether name is one of the repo-gated git readers.
func isGitTool(name string) bool {
	return slices.Contains(gitToolNames, name)
}

// childTools returns the read-only tools a child may call: the read-only built-ins
// plus any registry-marked read-only tool, never agent_*. Parent enable state is
// ignored so find/grep/ls reach even a disabled parent. The git readers need a
// repository: inRepo false withholds them, so git capability is never advertised
// where it cannot apply.
func childTools(src ToolSource, inRepo bool) []agent.Tool {
	var out []agent.Tool
	for _, t := range src.All() {
		name := t.Name()
		if strings.HasPrefix(name, "agent_") { // the bar applies last; nothing configures past it
			continue
		}
		if slices.Contains(readOnlyBuiltins, name) || src.ReadOnly(name) {
			if isGitTool(name) && !inRepo { // repo-context gate
				continue
			}
			out = append(out, t)
		}
	}
	return out
}

// toolSet is a fixed read-only view over a child's resolved tools.
type toolSet struct {
	tools []agent.Tool
}

func (t *toolSet) Get(name string) (agent.Tool, bool) {
	for _, x := range t.tools {
		if x.Name() == name {
			return x, true
		}
	}
	return nil, false
}

func (t *toolSet) Schemas() []llm.ToolSchema {
	out := make([]llm.ToolSchema, len(t.tools))
	for i, x := range t.tools {
		out[i] = llm.ToolSchema{Name: x.Name(), Description: x.Description(), Parameters: x.Schema().Parameters}
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
