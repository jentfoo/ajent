package permit

import (
	"slices"
	"strings"
)

// abbrevLong reports whether tok resolves to one of flags under getopt_long
// abbreviation: any prefix past the dashes counts as the flag itself, since
// uniqueness depends on each command's option table. An =-attached value never
// hides the name. A bare -- terminates options instead.
func abbrevLong(tok string, flags ...string) bool {
	if !strings.HasPrefix(tok, "--") {
		return false
	}
	name := tok
	if i := strings.IndexByte(name, '='); i >= 0 {
		name = name[:i]
	}
	if len(name) < 3 {
		return false
	}
	return slices.ContainsFunc(flags, func(flag string) bool {
		return strings.HasPrefix(flag, name)
	})
}
