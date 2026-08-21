package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var userHome = os.UserHomeDir // injectable for tests

// PathPolicy decides how a tool argument becomes an absolute path. There is no
// containment: relative paths are taken from Cwd and anything else the model
// asks for resolves as-is, so extensions decide their own limits if any.
type PathPolicy struct {
	Cwd string // base for relative paths
}

// argPath returns p, or "." when empty so a tool defaults to the session cwd.
func argPath(p string) string {
	if p == "" {
		return "."
	}
	return p
}

// Resolve returns the canonical absolute form of path, folding symlinks in its
// longest existing prefix so read/write/edit agree on one tracker key. A leading
// ~ or ~/ expands to the user's home directory; other relative paths are taken
// from Cwd. Nothing is refused.
func (p PathPolicy) Resolve(path string) (string, error) {
	var candidate string
	if expanded, ok := expandTilde(path); ok {
		candidate = expanded // absolute home path, no base needed
	} else {
		base := p.Cwd
		if base == "" {
			var err error
			if base, err = os.Getwd(); err != nil {
				return "", fmt.Errorf("tools: cannot resolve cwd: %w", err)
			}
		}
		candidate = cleanAbs(base, path)
	}
	resolved, ok := evalPrefix(candidate)
	if !ok { // nothing exists yet; the cleaned absolute is still a fine key
		return candidate, nil
	}
	return resolved, nil
}

// expandTilde replaces a leading ~ or ~/ with the user's home directory. ok is
// false when the path does not start with ~ or no home can be found.
func expandTilde(path string) (string, bool) {
	if path != "~" && (len(path) < 2 || !isSlash(path[1])) {
		return "", false
	}
	home, err := userHome()
	if err != nil || home == "" {
		return "", false
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/")
	return filepath.Join(home, rest), true
}

// isSlash reports whether c separates path components.
func isSlash(c byte) bool { return c == '/' || c == '\\' }

// HasGlob reports whether s contains a wildcard metacharacter, so it cannot be
// treated as one concrete path and must instead match many.
func HasGlob(s string) bool {
	return strings.ContainsAny(s, "*?[")
}

// cleanAbs returns the absolute cleaned form of path joined to base.
func cleanAbs(base, path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return filepath.Clean(path)
}

// evalPrefix resolves symlinks on the longest existing prefix of candidate,
// returning the fully resolved absolute path. ok is false when nothing exists.
func evalPrefix(candidate string) (string, bool) {
	var suffix []string
	p := candidate
	for {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(append([]string{resolved}, suffix...)...), true
		}
		suffix = append([]string{filepath.Base(p)}, suffix...) // keep original order
		parent := filepath.Dir(p)
		if parent == p { // reached the filesystem root without finding anything
			break
		}
		p = parent
	}
	return "", false
}
