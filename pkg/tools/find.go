package tools

import (
	"context"
	"fmt"
	"os"
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

	trimmed := strings.TrimRight(b.String(), "\n")
	// Display mirrors the model-visible text so history shows head+collapse.
	return agent.ToolResult{Content: llmBlock(trimmed), Display: trimmed}, nil
}

// listFiles returns files under root matching pattern, bounded by max. It uses
// git ls-files inside a repo for .gitignore semantics and walks otherwise.
func listFiles(root, pattern string, max int) ([]string, bool) {
	var out []fileEntry // stat once so mtime sort does not re-Stat per comparison
	for _, p := range repoFiles(root) {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if matchGlob(pattern, relTo(root, p)) {
			out = append(out, fileEntry{path: p, mod: fi.ModTime()})
		}
	}
	slices.SortStableFunc(out, func(a, b fileEntry) int { // newest first
		switch {
		case a.mod.Equal(b.mod):
			return strings.Compare(a.path, b.path)
		case a.mod.Before(b.mod):
			return 1
		default:
			return -1
		}
	})
	paths := make([]string, len(out))
	for i := range out {
		paths[i] = out[i].path
	}
	truncated := len(paths) > max
	if truncated {
		paths = paths[:max]
	}
	return paths, truncated
}

// fileEntry pairs a path with its modification time so listFiles sorts without
// re-statting every comparison.
type fileEntry struct {
	path string
	mod  time.Time
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
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return rel
}
