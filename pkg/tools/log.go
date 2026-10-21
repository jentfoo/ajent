package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/go-git/go-git/v5"
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
	return "List recent commits of the repository containing path, newest first: short hash, author date and subject. Optional file narrows to commits touching that file (following its renames back through history) or directory. Read-only: the repository is parsed in process and nothing from it is executed."
}

func (t *gitLogTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{Parameters: SchemaOf[gitLogParams]()}
}

func (t *gitLogTool) Mode() agent.ExecutionMode {
	return agent.ModeParallel
}
func (t *gitLogTool) Close() {}

// selfBounding: git_log bounds and spills its own results.
func (*gitLogTool) selfBounding() {}

// Execute walks history from HEAD, bounded by limit. With file set the walk
// follows the file's renames, like `git log --follow`. The complete list
// spills to disk when the tool bound cuts it.
func (t *gitLogTool) Execute(ctx context.Context, call agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	var p gitLogParams
	if err := decode(call.Input, &p); err != nil {
		return agent.ErrorToolResult("bad args: " + err.Error()), nil
	}
	root, err := t.policy.Resolve(argPath(p.Path))
	if err != nil {
		return agent.ErrorToolResult(err.Error()), nil
	}
	r, err := openGitRepo(root)
	if err != nil {
		return agent.ErrorToolResult(ToolGitLog + ": " + err.Error()), nil
	}

	head, err := gitCommit(r, "HEAD")
	if err != nil {
		return agent.ErrorToolResult(ToolGitLog + ": " + err.Error()), nil
	}
	walk := defaultLogCount
	if p.Limit > 0 {
		walk = min(p.Limit, maxLogCommits)
	}
	file := normalizeGitPathspec(p.File)
	var out string
	var shown int // commits actually listed, never walk
	var more bool
	var chain []string
	iter, logErr := r.Log(&git.LogOptions{From: head.ID(), Order: git.LogOrderCommitterTime})
	if logErr != nil {
		return agent.ErrorToolResult(ToolGitLog + ": " + logErr.Error()), nil
	}
	if file != "" {
		out, shown, more, chain, err = gitLogFollow(ctx, iter, file, walk)
	} else {
		out, shown, more, err = gitLogLines(ctx, iter, walk)
	}
	if err != nil {
		return agent.ErrorToolResult(ToolGitLog + ": " + err.Error()), nil
	}
	// only a finished walk may claim an empty scope matched nothing
	if !more && out == "" && file != "" {
		out = "(no commits touch " + file + ")"
	}
	if len(chain) > 0 {
		out += "\nrenames followed: " + strings.Join(chain, ", ")
	}
	paging := "raise limit or narrow with file"
	if more { // cap cuts can list fewer than limit
		out += "\n... " + strutil.Plural(shown, "commit") + " shown"
	}
	text, _ := truncateOutput(t.sessionID, ToolGitLog, out, GitResultLimit(), paging)
	return agent.ToolResult{Content: llmBlock(text), Display: text}, nil
}

// gitLogLines renders at most limit commits, reporting how many it listed and
// whether history continues past them.
func gitLogLines(ctx context.Context, iter object.CommitIter, limit int) (string, int, bool, error) {
	var b strings.Builder
	var shown int
	for i := 0; i < limit; i++ {
		if ctx.Err() != nil {
			return "", 0, false, ctx.Err()
		}
		c, err := iter.Next()
		if errors.Is(err, io.EOF) {
			return strings.TrimRight(b.String(), "\n"), shown, false, nil
		}
		if err != nil {
			return "", 0, false, err
		}
		fmt.Fprintf(&b, "%s %s %s\n", shortHash(c.ID()), c.Author.When.Format(logTimeFormat), commitSubject(c))
		shown++
	}
	_, err := iter.Next() // one past the limit decides "more"
	return strings.TrimRight(b.String(), "\n"), shown, !errors.Is(err, io.EOF), nil
}

// gitLogFollow walks full history newest first, listing commits that touch
// file and following its renames backwards, `git log --follow` style: when a
// commit renames the tracked path, older commits are matched under the old
// name. A directory scope instead matches every change beneath it and never
// follows renames or ends early, like `git log -- dir/`. It reports how many
// commits it listed, whether history continues past them and the rename
// chain, oldest first.
func gitLogFollow(ctx context.Context, iter object.CommitIter, file string, limit int) (string, int, bool, []string, error) {
	var b strings.Builder
	var chain []string
	path := file
	var shown, steps int
	for {
		if ctx.Err() != nil {
			return "", 0, false, nil, ctx.Err()
		}
		if steps++; steps > maxLogCommits {
			// a cut can land at history's end: one more step decides "more"
			_, err := iter.Next()
			return strings.TrimRight(b.String(), "\n"), shown, !errors.Is(err, io.EOF), chain, nil
		}
		c, err := iter.Next()
		if errors.Is(err, io.EOF) {
			return strings.TrimRight(b.String(), "\n"), shown, false, chain, nil
		}
		if err != nil {
			return "", 0, false, nil, err
		}
		touched, prev, ended, err := followTouch(ctx, c, path)
		if err != nil {
			return "", 0, false, nil, err
		}
		if !touched {
			continue
		}
		if shown >= limit {
			// past the limit: the next older touch decides whether history continues
			return strings.TrimRight(b.String(), "\n"), shown, !ended, chain, nil
		}
		_, _ = fmt.Fprintf(&b, "%s %s %s\n", shortHash(c.ID()), c.Author.When.Format(logTimeFormat), commitSubject(c))
		if prev != "" {
			chain = append(chain, prev+" -> "+path)
			path = prev
		}
		shown++
		if ended {
			return strings.TrimRight(b.String(), "\n"), shown, false, chain, nil // born or deleted: history ends
		}
	}
}

// followTouch reports whether scope changed at c against its first parent.
// The scope matches an exact path or anything beneath it as a directory; only
// an exact file match tracks renames. ended marks an exact path's history
// finishing here: born (created without a rename source) or deleted. prev
// carries the pre-rename name, empty when the path was merely modified or
// renamed away on a merge side.
func followTouch(ctx context.Context, c *object.Commit, file string) (touched bool, prev string, ended bool, err error) {
	keep := gitPathScope(file)
	var parent *object.Commit
	if c.NumParents() > 0 {
		if parent, err = c.Parent(0); err != nil {
			return false, "", false, err
		}
	}
	if parent != nil { // unchanged blobs cannot be part of any change
		ph, pok := entryHash(parent, file)
		chh, cok := entryHash(c, file)
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
	// pairing spans every change in the commit, no scope filter first: an old
	// name outside the scope must still pair onto the tracked path. go-git's
	// mode gate would otherwise end this walk at a rename+chmod with a false
	// birth or deletion.
	changes, err := pairedTreeChanges(ctx, fromTree, toTree)
	if err != nil {
		return false, "", false, err
	}
	for _, ch := range changes {
		if !keep(ch.From.Name) && !keep(ch.To.Name) {
			continue
		}
		switch {
		case ch.To.Name == file:
			switch {
			case ch.From.Name == "":
				return true, "", true, nil // born here
			case ch.From.Name != file:
				return true, ch.From.Name, false, nil // renamed into path
			default:
				return true, "", false, nil // modified
			}
		case ch.From.Name == file && ch.To.Name == "":
			return true, "", true, nil // deleted: history ends here
		case ch.From.Name == file:
			return true, "", false, nil // renamed away on a merge side
		default:
			// under a directory scope: one file's birth or deletion never
			// ends the walk, other in-scope history may continue below
			return true, "", false, nil
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
