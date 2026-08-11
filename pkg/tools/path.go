package tools

import (
	"fmt"
	"os"
	"path/filepath"
)

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
// longest existing prefix so read/write/edit agree on one tracker key. Relative
// paths are taken from Cwd; nothing is refused.
func (p PathPolicy) Resolve(path string) (string, error) {
	base := p.Cwd
	if base == "" {
		var err error
		if base, err = os.Getwd(); err != nil {
			return "", fmt.Errorf("tools: cannot resolve cwd: %w", err)
		}
	}
	candidate := cleanAbs(base, path)
	resolved, ok := evalPrefix(candidate)
	if !ok { // nothing exists yet; the cleaned absolute is still a fine key
		return candidate, nil
	}
	return resolved, nil
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
