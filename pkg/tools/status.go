package tools

import (
	"context"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// gitStatusParams is the model-facing parameter block for git_status.
type gitStatusParams struct {
	Path string `json:"path,omitempty" desc:"path inside the repository; default the session cwd"`
}

// gitStatusTool reports the working tree state of the repository containing
// path. Registered off by default: it exists for the sub-agent, which has no
// shell, and runs on go-git so an untrusted repository cannot execute code.
type gitStatusTool struct {
	policy    PathPolicy
	sessionID string // names the spill directory for long results
}

var _ agent.Tool = (*gitStatusTool)(nil)

func (t *gitStatusTool) Name() string { return ToolGitStatus }

func (t *gitStatusTool) Label(agent.ToolCall) string {
	return ToolGitStatus
}

func (t *gitStatusTool) Description() string {
	return "Show the working tree status of the repository containing path: current branch (or detached HEAD), then staged, unstaged and untracked files in git short format with counts. Read-only: the repository is parsed in process and nothing from it is executed."
}

func (t *gitStatusTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{Parameters: SchemaOf[gitStatusParams]()}
}

func (t *gitStatusTool) Mode() agent.ExecutionMode {
	return agent.ModeParallel
}

// selfBounding: git_status bounds and spills its own results.
func (*gitStatusTool) selfBounding() {}

// Execute renders the status bounded by the tool limit. The complete listing
// spills to disk when the bound cuts it, so the model can page the file.
func (t *gitStatusTool) Execute(ctx context.Context, call agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	var p gitStatusParams
	if err := decode(call.Input, &p); err != nil {
		return resultErr("bad args: " + err.Error()), nil
	}
	root, err := t.policy.Resolve(argPath(p.Path))
	if err != nil {
		return resultErr(err.Error()), nil
	}
	r, err := openGitRepo(root)
	if err != nil {
		return resultErr(ToolGitStatus + ": " + err.Error()), nil
	}
	out, err := gitStatusText(r)
	if err != nil {
		return resultErr(ToolGitStatus + ": " + err.Error()), nil
	}
	text, _ := truncateOutput(t.sessionID, ToolGitStatus, out, GitResultLimit(), "narrow the path")
	return agent.ToolResult{Content: llmBlock(text), Display: text}, nil
}
