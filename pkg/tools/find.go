package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// findParams is the model-facing parameter block for find.
type findParams struct {
	Pattern string `json:"pattern" desc:"glob, supporting **; a bare pattern like '*.go' matches at any depth"`
	Path    string `json:"path,omitempty" desc:"directory to search in; default the session cwd"`
	Limit   int    `json:"limit,omitempty" desc:"max results to return"`
}

// findTool enumerates files under a git repo with .gitignore semantics via
// git ls-files, or walks otherwise. Sorted by mtime descending.
type findTool struct {
	policy PathPolicy
}

var _ agent.Tool = (*findTool)(nil)

func (t *findTool) Name() string { return "find" }

func (t *findTool) Label(agent.ToolCall) string {
	return "find"
}

func (t *findTool) Description() string {
	return "Search for files by glob pattern. Returns matching file paths relative to the search directory, newest first. Respects .gitignore."
}
func (t *findTool) Schema() llm.ToolSchema { return llm.ToolSchema{Parameters: SchemaOf[findParams]()} }
func (t *findTool) Mode() agent.ExecutionMode {
	return agent.ModeParallel
}

// Execute lists matching files bounded by the limit, newest first.
func (t *findTool) Execute(ctx context.Context, call agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	var p findParams
	if err := decode(call.Input, &p); err != nil {
		return resultErr("bad args: " + err.Error()), nil
	}
	if strings.TrimSpace(p.Pattern) == "" {
		return resultErr("find needs a non-empty pattern"), nil
	}
	root, err := t.policy.Resolve(argPath(p.Path))
	if err != nil {
		return resultErr(err.Error()), nil
	}

	max := p.Limit
	if max <= 0 {
		max = FindResultLimit().Lines
	}
	matches, truncated := listFiles(root, p.Pattern, max)

	var b strings.Builder
	for _, m := range matches {
		fmt.Fprintln(&b, relTo(root, m))
	}
	if truncated {
		b.WriteString("... more results; narrow your pattern or raise limit\n")
	}

	return agent.ToolResult{Content: llmBlock(strings.TrimRight(b.String(), "\n"))}, nil
}

// listFiles returns files under root matching pattern, bounded by max. It uses
// git ls-files inside a repo for .gitignore semantics and walks otherwise.
func listFiles(root, pattern string, max int) ([]string, bool) {
	var entries []string
	if IsGitRepo(root) {
		out := runQuiet("git", "-C", root, "ls-files", "-co", "--exclude-standard")
		for line := range strings.Lines(out) {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				entries = append(entries, filepath.Join(root, trimmed))
			}
		}
	} else {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if IsSkippedDir(p) {
					return filepath.SkipDir // never descend into VCS or dependency dirs
				}
				return nil
			}
			entries = append(entries, p)
			return nil
		})
	}

	var out []string
	for _, e := range entries {
		if matchGlob(pattern, relTo(root, e)) {
			out = append(out, e)
		}
	}
	slices.SortStableFunc(out, func(a, b string) int { return mtimeCmp(b, a) }) // newest first
	truncated := len(out) > max
	if truncated {
		out = out[:max]
	}
	return out, truncated
}

// IsGitRepo reports whether root is inside a git work tree.
func IsGitRepo(root string) bool {
	return runQuiet("git", "-C", root, "rev-parse", "--is-inside-work-tree") == "true"
}

// IsSkippedDir reports whether path lies under a VCS or dependency directory.
func IsSkippedDir(path string) bool {
	for part := range strings.SplitSeq(filepath.Clean(path), string(filepath.Separator)) {
		switch part {
		case ".git", ".hg", ".svn", "node_modules", ".venv":
			return true
		}
	}
	return false
}

// matchGlob reports whether name matches pattern. A pattern with no path
// separator matches the base name at any depth ("*.go" finds pkg/a.go); a
// pattern with separators matches the whole relative path, with ** spanning
// any number of segments including zero ("**/*.go" also finds ./a.go).
func matchGlob(pattern, name string) bool {
	if !strings.Contains(pattern, "/") {
		ok, err := filepath.Match(pattern, filepath.Base(name))
		return err == nil && ok
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

// matchSegments matches /-separated pattern segments against name segments,
// where ** consumes zero or more name segments.
func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if matchSegments(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, err := filepath.Match(pat[0], name[0])
		if err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// relTo returns path relative to root when inside it.
func relTo(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}

// mtimeCmp orders a before b by modification time (a newer → negative for desc).
func mtimeCmp(a, b string) int {
	ai, _ := os.Stat(a)
	bi, _ := os.Stat(b)
	switch {
	case ai == nil && bi == nil:
		return 0
	case ai == nil:
		return -1
	case bi == nil:
		return 1
	default:
		if ai.ModTime().Equal(bi.ModTime()) {
			return strings.Compare(a, b)
		}
		if ai.ModTime().Before(bi.ModTime()) {
			return -1
		}
		return 1
	}
}

// runQuiet runs a command with a short timeout and returns trimmed stdout or
// empty on failure, so an unresponsive child cannot hang the tool.
func runQuiet(args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out strings.Builder
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = &out
	if cmd.Run() != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}
