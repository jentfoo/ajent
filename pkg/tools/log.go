package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
)

// defaultLogCount is how many commits git_log lists without an explicit limit.
const defaultLogCount = 30

// maxLogCommits caps the history walk so a huge repository cannot stall the call.
const maxLogCommits = 1000

// logTimeFormat is the compact author-date stamp of one log line.
const logTimeFormat = "2006-01-02 15:04"

// gitLogParams is the model-facing parameter block for git_log.
type gitLogParams struct {
	Limit int    `json:"limit,omitempty" desc:"max commits to list; default 30"`
	File  string `json:"file,omitempty" desc:"only commits touching this file or directory, repo-relative"`
	Path  string `json:"path,omitempty" desc:"path inside the repository; default the session cwd"`
}

// gitLogTool lists recent commits: short hash, author date, subject. Registered
// off by default: it exists for the sub-agent, which has no shell, and runs on
// go-git so an untrusted repository cannot execute code.
type gitLogTool struct {
	policy    PathPolicy
	sessionID string // names the spill directory for long results
}

var _ agent.Tool = (*gitLogTool)(nil)

func (t *gitLogTool) Name() string { return ToolGitLog }

func (t *gitLogTool) Label(agent.ToolCall) string {
	return ToolGitLog
}

func (t *gitLogTool) Description() string {
	return "List recent commits of the repository containing path, newest first: short hash, author date and subject. Optional file narrows to commits touching that file or directory and follows its renames back through history. Read-only: the repository is parsed in process and nothing from it is executed."
}

func (t *gitLogTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{Parameters: SchemaOf[gitLogParams]()}
}

func (t *gitLogTool) Mode() agent.ExecutionMode {
	return agent.ModeParallel
}

// selfBounding: git_log bounds and spills its own results.
func (*gitLogTool) selfBounding() {}

// Execute walks history from HEAD, bounded by limit. With file set the walk
// follows the file's renames, like `git log --follow`. The complete list
// spills to disk when the tool bound cuts it.
func (t *gitLogTool) Execute(ctx context.Context, call agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	var p gitLogParams
	if err := decode(call.Input, &p); err != nil {
		return resultErr("bad args: " + err.Error()), nil
	}
	root, err := t.policy.Resolve(argPath(p.Path))
	if err != nil {
		return resultErr(err.Error()), nil
	}
	r, err := openGitRepo(root)
	if err != nil {
		return resultErr(ToolGitLog + ": " + err.Error()), nil
	}

	head, err := gitCommit(r, "HEAD")
	if err != nil {
		return resultErr(ToolGitLog + ": " + err.Error()), nil
	}
	walk := defaultLogCount
	if p.Limit > 0 {
		walk = min(p.Limit, maxLogCommits)
	}
	file := strings.TrimSpace(p.File)
	var out string
	var more bool
	var chain []string
	iter, logErr := r.Log(&git.LogOptions{From: head.ID(), Order: git.LogOrderCommitterTime})
	if logErr != nil {
		return resultErr(ToolGitLog + ": " + logErr.Error()), nil
	}
	if file != "" {
		out, more, chain, err = gitLogFollow(ctx, iter, file, walk)
	} else {
		out, more, err = gitLogLines(ctx, iter, walk)
	}
	if err != nil {
		return resultErr(ToolGitLog + ": " + err.Error()), nil
	}
	if out == "" && file != "" { // a scope that matches nothing still needs an answer
		out = "(no commits touch " + file + ")"
	}
	if len(chain) > 0 {
		out += "\nrenames followed: " + strings.Join(chain, ", ")
	}
	paging := "raise limit or narrow with file"
	if more {
		out += "\n... " + plural(walk, "commit") + " shown"
	}
	text, _ := truncateOutput(t.sessionID, ToolGitLog, out, GitResultLimit(), paging)
	return agent.ToolResult{Content: llmBlock(text), Display: text}, nil
}

// gitLogLines renders at most limit commits, reporting whether history
// continues past them.
func gitLogLines(ctx context.Context, iter object.CommitIter, limit int) (string, bool, error) {
	var b strings.Builder
	for i := 0; i < limit; i++ {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		c, err := iter.Next()
		if errors.Is(err, io.EOF) {
			return strings.TrimRight(b.String(), "\n"), false, nil
		}
		if err != nil {
			return "", false, err
		}
		fmt.Fprintf(&b, "%s %s %s\n", shortHash(c.ID()), c.Author.When.Format(logTimeFormat), commitSubject(c))
	}
	_, err := iter.Next() // one past the limit decides "more"
	return strings.TrimRight(b.String(), "\n"), !errors.Is(err, io.EOF), nil
}

// gitLogFollow walks full history newest first, listing commits that touch
// file and following its renames backwards, `git log --follow` style: when a
// commit renames the tracked path, older commits are matched under the old
// name. It reports whether the walk was cut (by limit or step cap) and the
// rename chain, oldest first.
func gitLogFollow(ctx context.Context, iter object.CommitIter, file string, limit int) (string, bool, []string, error) {
	var b strings.Builder
	var chain []string
	path := file
	shown, steps := 0, 0
	for {
		if ctx.Err() != nil {
			return "", false, nil, ctx.Err()
		}
		if steps++; steps > maxLogCommits {
			return strings.TrimRight(b.String(), "\n"), true, chain, nil
		}
		c, err := iter.Next()
		if errors.Is(err, io.EOF) {
			return strings.TrimRight(b.String(), "\n"), false, chain, nil
		}
		if err != nil {
			return "", false, nil, err
		}
		touched, prev, ended, err := followTouch(ctx, c, path)
		if err != nil {
			return "", false, nil, err
		}
		if !touched {
			continue
		}
		if shown >= limit {
			// past the limit: the next older touch decides whether history continues
			return strings.TrimRight(b.String(), "\n"), !ended, chain, nil
		}
		fmt.Fprintf(&b, "%s %s %s\n", shortHash(c.ID()), c.Author.When.Format(logTimeFormat), commitSubject(c))
		if prev != "" {
			chain = append(chain, prev+" -> "+path)
			path = prev
		}
		shown++
		if ended {
			return strings.TrimRight(b.String(), "\n"), false, chain, nil // born or deleted: history ends
		}
	}
}

// followTouch reports whether path changed at c against its first parent.
// ended marks the path's history finishing here: born (created without a
// rename source) or deleted. prev carries the pre-rename name, empty when the
// path was merely modified or renamed away on a merge side.
func followTouch(ctx context.Context, c *object.Commit, path string) (touched bool, prev string, ended bool, err error) {
	var parent *object.Commit
	if c.NumParents() > 0 {
		if parent, err = c.Parent(0); err != nil {
			return false, "", false, err
		}
	}
	if parent != nil { // unchanged blobs cannot be part of any change
		ph, pok := entryHash(parent, path)
		chh, cok := entryHash(c, path)
		if pok && cok && ph == chh {
			return false, "", false, nil
		}
	}
	fromTree, err := gitTree(parent)
	if err != nil {
		return false, "", false, err
	}
	toTree, err := gitTree(c)
	if err != nil {
		return false, "", false, err
	}
	changes, err := object.DiffTreeWithOptions(ctx, fromTree, toTree, object.DefaultDiffTreeOptions)
	if err != nil {
		return false, "", false, err
	}
	for _, ch := range changes {
		if ch.To.Name == path {
			switch {
			case ch.From.Name == "":
				return true, "", true, nil // born here
			case ch.From.Name != path:
				return true, ch.From.Name, false, nil // renamed into path
			default:
				return true, "", false, nil // modified
			}
		}
		if ch.From.Name == path {
			if ch.To.Name == "" {
				return true, "", true, nil // deleted: path history ends
			}
			return true, "", false, nil // renamed away on a merge side
		}
	}
	return false, "", false, nil
}

// entryHash returns path's blob hash in c, false when it does not exist.
func entryHash(c *object.Commit, path string) (plumbing.Hash, bool) {
	t, err := c.Tree()
	if err != nil {
		return plumbing.ZeroHash, false
	}
	f, err := t.File(path)
	if err != nil {
		return plumbing.ZeroHash, false
	}
	return f.Hash, true
}

// commitSubject returns the first line of the commit message.
func commitSubject(c *object.Commit) string {
	return strings.TrimSpace(strutil.FirstLine(c.Message))
}
