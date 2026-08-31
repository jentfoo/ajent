package mcp

import (
	"path"
	"slices"

	"github.com/go-analyze/bulk"
)

// pathMatch reports whether name matches a single glob pattern. A malformed
// pattern never matches rather than erroring, so config cannot break discovery.
func pathMatch(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

// filterTools applies a server's allow/deny lists and exact-name exclusions to
// discovered tools. A tool must be allowed AND not denied: an explicit deny wins
// over a matching allow, so a broad allow cannot re-admit something refused; an
// empty allow list admits everything not denied; exclude drops by exact name
// regardless of globs.
func filterTools(defs []ToolDef, f ToolFilter, exclude []string) []ToolDef {
	return bulk.SliceFilter(func(d ToolDef) bool {
		return allowed(d.Name, f.Allow) && !denied(d.Name, f.Deny) && !slices.Contains(exclude, d.Name)
	}, defs)
}

func allowed(name string, patterns []string) bool {
	if len(patterns) == 0 {
		return true // no allow list: everything admitted unless denied
	}
	for _, p := range patterns {
		if pathMatch(p, name) {
			return true
		}
	}
	return false
}

func denied(name string, patterns []string) bool {
	for _, p := range patterns {
		if pathMatch(p, name) {
			return true
		}
	}
	return false
}
