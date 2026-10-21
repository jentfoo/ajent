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
	Ref       string `json:"ref,omitempty" desc:"commit-ish to show: HEAD, HEAD~1, a branch, tag or hash; default HEAD"`
	File      string `json:"file,omitempty" desc:"limit the patch to this file or directory, repo-relative"`
	Submodule string `json:"submodule,omitempty" desc:"how submodule pointer changes render: \"short\" Subproject hunks (default), \"log\" child commit subjects, or \"diff\" the child's own patch"`
	Path      string `json:"path,omitempty" desc:"path inside the repository; default the session cwd"`
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
	return "Show one commit of the repository containing path: full metadata plus the patch it introduces. Optional file limits the patch to one path or directory prefix, and submodule picks git's --submodule rendering for pointer changes (short Subproject hunks by default, log or diff forms). Read-only: the repository is parsed in process and nothing from it is executed. Binary deltas are named, never rendered; merges diff against the first parent and say so."
}

func (t *gitShowTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{Parameters: SchemaOf[gitShowParams]()}
}

func (t *gitShowTool) Mode() agent.ExecutionMode {
	return agent.ModeParallel
}
func (t *gitShowTool) Close() {}

// selfBounding: git_show bounds and spills its own results.
func (*gitShowTool) selfBounding() {}

// Execute renders the commit bounded by the tool limit. Blocks stream through
// the bound and the complete output spills to disk when it cuts.
func (t *gitShowTool) Execute(ctx context.Context, call agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	var p gitShowParams
	if err := decode(call.Input, &p); err != nil {
		return agent.ErrorToolResult("bad args: " + err.Error()), nil
	}
	root, err := t.policy.Resolve(argPath(p.Path))
	if err != nil {
		return agent.ErrorToolResult(err.Error()), nil
	}
	r, err := openGitRepo(root)
	if err != nil {
		return agent.ErrorToolResult(ToolGitShow + ": " + err.Error()), nil
	}
	c, err := gitCommit(r, p.Ref)
	if err != nil {
		return agent.ErrorToolResult(ToolGitShow + ": " + err.Error()), nil
	}
	style, err := parseSubmoduleStyle(p.Submodule)
	if err != nil {
		return agent.ErrorToolResult("bad args: " + err.Error()), nil
	}
	d := newGitDiffOut(t.sessionID, ToolGitShow)
	defer func() { _ = d.close() }()
	// the bound never errors on an in-memory head, only diverts to the spill
	_, _ = fmt.Fprintf(d, "commit %s\nAuthor: %s\nDate:   %s\n\n%s\n",
		c.ID(), c.Author.String(), c.Author.When.Format(object.DateFormat), strings.TrimRight(c.Message, "\n"))
	if err := gitCommitDiffText(ctx, c, normalizeGitPathspec(p.File), newSubmoduleRender(r, style), d); err != nil {
		return agent.ErrorToolResult(ToolGitShow + ": " + err.Error()), nil
	}
	if d.EmptyPatch() { // the patch section must name its emptiness
		d.Meta("(no changes)")
	}
	text := d.Text("use git_log to find a smaller commit")
	return agent.ToolResult{Content: llmBlock(text), Display: text}, nil
}
