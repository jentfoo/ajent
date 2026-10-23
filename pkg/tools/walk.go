package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-analyze/bulk"
)

// IsGitRepo reports whether root is inside a git work tree.
func IsGitRepo(ctx context.Context, root string) bool {
	return strings.TrimSpace(runQuietRaw(ctx,
		"git", "-C", root, "rev-parse", "--is-inside-work-tree")) == "true"
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

// GitTrackedFiles returns tracked and untracked non-ignored regular files under
// root as slash paths relative to root. The result is empty when git cannot
// answer: outside a work tree, on failure, or with nothing listed.
// Deleted-but-tracked entries drop out and an unmerged path lists once.
func GitTrackedFiles(ctx context.Context, root string) []string {
	// The "." pathspec keeps ls-files to root's subtree, so a nested cwd never
	// lists parent or sibling files as "../". Raw output: in -z framing the
	// leading whitespace of the first name is name content, trimming mangles
	// or drops it.
	out := runQuietRaw(ctx, "git", "-C", root, "ls-files", "-co", "--exclude-standard", "-z", "--", ".")
	parts := strings.Split(out, "\x00")
	names := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, f := range parts {
		if f == "" { // the trailing separator always yields one empty element
			continue
		}
		p := filepath.Join(root, f)
		if _, ok := WithinRoot(root, p); !ok { // older git still emits ../, never leave scope
			continue
		}
		// -c lists deleted-but-tracked files; Lstat drops symlinks like rg's no-follow walk
		if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if _, dup := seen[f]; dup { // unmerged entries repeat, git --deduplicate is 2.31+
			continue
		}
		seen[f] = struct{}{}
		names = append(names, f)
	}
	return names
}

// allWalk returns every regular file under root, skipping VCS and dependency
// directory subtrees so a huge tree cannot hang a grep forever. Symlinks stay
// unlisted, matching rg's no-follow traversal.
func allWalk(ctx context.Context, root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if IsSkippedDir(p) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		out = append(out, p)
		return nil
	})
	return out
}

// repoFiles lists regular files under root with .gitignore semantics inside a
// repo via GitTrackedFiles joined onto root, falling back to allWalk otherwise
// or when git yields nothing.
func repoFiles(ctx context.Context, root string) []string {
	if IsGitRepo(ctx, root) {
		if names := GitTrackedFiles(ctx, root); len(names) > 0 {
			return bulk.SliceTransform(func(n string) string { return filepath.Join(root, n) }, names)
		}
	}
	return allWalk(ctx, root)
}

// WithinRoot returns path relative to root when it stays inside it. ok is
// false when the relation cannot be determined or path escapes root.
func WithinRoot(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// runQuietRaw runs a command with a short timeout and returns raw stdout or
// empty on failure, so an unresponsive child cannot hang the tool. NUL-separated
// callers keep whitespace as part of a record.
func runQuietRaw(ctx context.Context, args ...string) string {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out strings.Builder
	cmd := exec.CommandContext(dctx, args[0], args[1:]...)
	cmd.Stdout = &out
	if cmd.Run() != nil {
		return ""
	}
	return out.String()
}
