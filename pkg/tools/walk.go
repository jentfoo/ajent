package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// allWalk returns every regular file under root, skipping VCS and dependency
// directory subtrees so a huge tree cannot hang a grep forever.
func allWalk(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
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
func repoFiles(root string) []string {
	var entries []string
	if IsGitRepo(root) {
		out := runQuiet("git", "-C", root, "ls-files", "-co", "--exclude-standard", "-z")
		seen := make(map[string]struct{})
		for _, f := range strings.Split(out, "\x00") {
			if f == "" { // the trailing separator always yields one empty element
				continue
			}
			p := filepath.Join(root, f)
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
	return allWalk(root)
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

// runCaptured runs a command with a short timeout, returning trimmed stdout.
// Exit status 1 is "no matches" for search tools and yields empty output; any
// higher exit status returns stderr as an error so the model sees the cause.
func runCaptured(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return strings.TrimSpace(stdout.String()), nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return "", nil // no matches
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = err.Error()
	}
	return "", fmt.Errorf("%s: %s", name, msg)
}
