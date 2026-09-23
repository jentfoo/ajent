package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// gitShowParams is the model-facing parameter block for git_show.
type gitShowParams struct {
	Ref  string `json:"ref,omitempty" desc:"commit-ish to show: HEAD, HEAD~1, a branch, tag or hash; default HEAD"`
	File string `json:"file,omitempty" desc:"limit the patch to this file or directory, repo-relative"`
	Path string `json:"path,omitempty" desc:"path inside the repository; default the session cwd"`
}

// gitShowTool renders one commit's metadata plus the patch it introduces.
// Registered off by default: it exists for the sub-agent, which has no shell.
type gitShowTool struct {
	policy    PathPolicy
	sessionID string // names the spill directory for long results
}

var _ agent.Tool = (*gitShowTool)(nil)

func (t *gitShowTool) Name() string { return ToolGitShow }

func (t *gitShowTool) Label(agent.ToolCall) string {
	return ToolGitShow
}

func (t *gitShowTool) Description() string {
	return "Show one commit of the repository containing path: full metadata plus the patch it introduces. Optional file limits the patch to one path or directory prefix. Read-only: the repository is parsed in process and nothing from it is executed. Binary deltas are named, never rendered; merges diff against the first parent and say so."
}

func (t *gitShowTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{Parameters: SchemaOf[gitShowParams]()}
}

func (t *gitShowTool) Mode() agent.ExecutionMode {
	return agent.ModeParallel
}

// selfBounding: git_show bounds and spills its own results.
func (*gitShowTool) selfBounding() {}

// Execute renders the commit bounded by the tool limit. The complete output
// spills to disk when the bound cuts it, so the model can page the file.
func (t *gitShowTool) Execute(ctx context.Context, call agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	var p gitShowParams
	if err := decode(call.Input, &p); err != nil {
		return resultErr("bad args: " + err.Error()), nil
	}
	root, err := t.policy.Resolve(argPath(p.Path))
	if err != nil {
		return resultErr(err.Error()), nil
	}
	r, err := openGitRepo(root)
	if err != nil {
		return resultErr(ToolGitShow + ": " + err.Error()), nil
	}
	c, err := gitCommit(r, p.Ref)
	if err != nil {
		return resultErr(ToolGitShow + ": " + err.Error()), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "commit %s\nAuthor: %s\nDate:   %s\n\n%s\n",
		c.ID(), c.Author.String(), c.Author.When.Format(object.DateFormat), strings.TrimRight(c.Message, "\n"))
	body, err := gitCommitDiffText(ctx, c, p.File)
	if err != nil {
		return resultErr(ToolGitShow + ": " + err.Error()), nil
	}
	b.WriteString("\n" + body)
	text, _ := truncateOutput(t.sessionID, ToolGitShow, b.String(), GitResultLimit(), "use git_log to find a smaller commit")
	return agent.ToolResult{Content: llmBlock(text), Display: text}, nil
}
