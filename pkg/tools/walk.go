package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// lookPath reports whether an executable is on PATH.
func lookPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// IsGitRepo reports whether root is inside a git work tree.
func IsGitRepo(ctx context.Context, root string) bool {
	return runQuiet(ctx, "git", "-C", root, "rev-parse", "--is-inside-work-tree") == "true"
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

// allWalk returns every regular file under root, skipping VCS and dependency
// directory subtrees so a huge tree cannot hang a grep forever.
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
		out = append(out, p)
		return nil
	})
	return out
}

// repoFiles lists files under root with .gitignore semantics inside a repo via
// git ls-files -z (quoting disabled, so non-ASCII names stay usable), falling
// back to allWalk otherwise or when git yields nothing. Entries that no longer
// exist on disk are dropped.
func repoFiles(ctx context.Context, root string) []string {
	var entries []string
	if IsGitRepo(ctx, root) {
		// The "." pathspec keeps ls-files to root's subtree, so a nested cwd never
		// lists parent or sibling files as "../".
		out := runQuiet(ctx, "git", "-C", root, "ls-files", "-co", "--exclude-standard", "-z", "--", ".")
		seen := make(map[string]struct{})
		for _, f := range strings.Split(out, "\x00") {
			if f == "" { // the trailing separator always yields one empty element
				continue
			}
			p := filepath.Join(root, f)
			if !withinRoot(root, p) { // older git still emits ../; never leave scope
				continue
			}
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() { // -c lists deleted-but-tracked files
				if _, dup := seen[p]; dup { // unmerged entries repeat; git --deduplicate is 2.31+
					continue
				}
				seen[p] = struct{}{}
				entries = append(entries, p)
			}
		}
		if len(entries) > 0 {
			return entries
		}
	}
	return allWalk(ctx, root)
}

// withinRoot reports whether path stays inside root.
func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// runQuiet runs a command with a short timeout and returns trimmed stdout or
// empty on failure, so an unresponsive child cannot hang the tool.
func runQuiet(ctx context.Context, args ...string) string {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out strings.Builder
	cmd := exec.CommandContext(dctx, args[0], args[1:]...)
	cmd.Stdout = &out
	if cmd.Run() != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}
