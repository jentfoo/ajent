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

// filterTools applies a server's allow/deny lists and exact-name exclusions to discovered tools.
// A tool must be allowed AND not denied: an explicit deny wins over a matching allow, so a broad
// allow cannot re-admit something refused, an empty allow list admits everything not denied,
// exclude drops by exact name regardless of globs; an empty allow list admits
// everything the deny list and excludes did not catch.
func filterTools(defs []ToolDef, f ToolFilter, exclude []string) []ToolDef {
	return bulk.SliceFilter(func(d ToolDef) bool {
		if slices.Contains(exclude, d.Name) {
			return false
		}
		match := func(p string) bool { return pathMatch(p, d.Name) }
		if slices.ContainsFunc(f.Deny, match) {
			return false
		}
		return len(f.Allow) == 0 || slices.ContainsFunc(f.Allow, match)
	}, defs)
}
