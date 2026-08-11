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
			if isSkipped(p) {
				return filepath.SkipDir
			}
			return nil
		}
		out = append(out, p)
		return nil
	})
	return out
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
