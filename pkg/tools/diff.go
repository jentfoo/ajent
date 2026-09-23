package tools

import (
	"context"
	"errors"
	"strings"

	git "github.com/go-git/go-git/v5"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// gitWorktreeRef selects the working tree as a diff target.
const gitWorktreeRef = "worktree"

// gitDiffParams is the model-facing parameter block for git_diff.
type gitDiffParams struct {
	From string `json:"from,omitempty" desc:"base commit-ish, or an \"a..b\" range shorthand for from+to; default HEAD"`
	To   string `json:"to,omitempty" desc:"target commit-ish, or \"worktree\" for uncommitted changes; empty means the change from introduces"`
	File string `json:"file,omitempty" desc:"limit to changes under this file or directory, repo-relative"`
	Path string `json:"path,omitempty" desc:"path inside the repository; default the session cwd"`
}

// gitDiffTool renders diffs between refs, of a single commit, or against the
// working tree. Registered off by default: it exists for the sub-agent, which
// has no shell.
type gitDiffTool struct {
	policy    PathPolicy
	sessionID string // names the spill directory for long results
}

var _ agent.Tool = (*gitDiffTool)(nil)

func (t *gitDiffTool) Name() string { return ToolGitDiff }

func (t *gitDiffTool) Label(agent.ToolCall) string {
	return ToolGitDiff
}

func (t *gitDiffTool) Description() string {
	return "Diff the repository containing path: from a commit-ish to another (from+to), the patch one commit introduces (one of from/to), or committed state against the working tree (to \"worktree\" for uncommitted changes). Optional file limits the diff to one path or directory prefix. Read-only: the repository is parsed in process and nothing from it is executed. Binary deltas and untracked files are named, never rendered; uncommitted renames appear as separate delete and add."
}

func (t *gitDiffTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{Parameters: SchemaOf[gitDiffParams]()}
}

func (t *gitDiffTool) Mode() agent.ExecutionMode {
	return agent.ModeParallel
}

// selfBounding: git_diff bounds and spills its own results.
func (*gitDiffTool) selfBounding() {}

// Execute renders the requested diff bounded by the tool limit. The complete
// patch spills to disk when the bound cuts it, so the model can page the file.
func (t *gitDiffTool) Execute(ctx context.Context, call agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	var p gitDiffParams
	if err := decode(call.Input, &p); err != nil {
		return resultErr("bad args: " + err.Error()), nil
	}
	if p.From == gitWorktreeRef {
		return resultErr(ToolGitDiff + ": from must be a commit-ish; put \"worktree\" in to"), nil
	}
	root, err := t.policy.Resolve(argPath(p.Path))
	if err != nil {
		return resultErr(err.Error()), nil
	}
	r, err := openGitRepo(root)
	if err != nil {
		return resultErr(ToolGitDiff + ": " + err.Error()), nil
	}

	out, err := gitDiffText(ctx, r, p.From, p.To, p.File)
	if err != nil {
		return resultErr(ToolGitDiff + ": " + err.Error()), nil
	}
	text, _ := truncateOutput(t.sessionID, ToolGitDiff, out, GitResultLimit(), "narrow with file or a smaller range")
	return agent.ToolResult{Content: llmBlock(text), Display: text}, nil
}

// splitGitRange expands git's "a..b" range shorthand into its two revisions.
// Three-dot (merge-base) ranges are refused.
func splitGitRange(rev string) (from, to string, err error) {
	if i := strings.Index(rev, "..."); i >= 0 {
		return "", "", errors.New("three-dot ranges are not supported; use from and to")
	}
	i := strings.Index(rev, "..")
	if i < 0 {
		return rev, "", nil
	}
	from, to = rev[:i], rev[i+2:]
	if from == "" || to == "" {
		return "", "", errors.New("range needs a revision on each side of the dots")
	}
	return from, to, nil
}

// gitDiffText resolves the requested range and renders its unified patch,
// limited to file's path scope when non-empty.
func gitDiffText(ctx context.Context, r *git.Repository, from, to, file string) (string, error) {
	if to == "" { // accept git's "a..b" range shorthand in from
		var err error
		if from, to, err = splitGitRange(from); err != nil {
			return "", err
		}
	}
	switch {
	case to == gitWorktreeRef:
		base, err := gitCommit(r, from)
		if err != nil {
			return "", err
		}
		baseTree, err := gitTree(base)
		if err != nil {
			return "", err
		}
		body, notes, err := gitDiffWorktreeText(ctx, r, baseTree, file)
		if err != nil {
			return "", err
		}
		return finishDiff(body, notes), nil
	case from != "" && to != "":
		fromC, err := gitCommit(r, from)
		if err != nil {
			return "", err
		}
		toC, err := gitCommit(r, to)
		if err != nil {
			return "", err
		}
		fromTree, err := gitTree(fromC)
		if err != nil {
			return "", err
		}
		toTree, err := gitTree(toC)
		if err != nil {
			return "", err
		}
		body, notes, err := gitDiffTreeText(ctx, fromTree, toTree, file)
		if err != nil {
			return "", err
		}
		return finishDiff(body, notes), nil
	case from != "" || to != "":
		rev := from // exactly one side names the commit to show
		if rev == "" {
			rev = to
		}
		c, err := gitCommit(r, rev)
		if err != nil {
			return "", err
		}
		return gitCommitDiffText(ctx, c, file)
	default:
		head, err := gitCommit(r, "HEAD")
		if err != nil {
			return "", err
		}
		return gitCommitDiffText(ctx, head, file)
	}
}
