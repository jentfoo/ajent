// Package strutil holds tiny string helpers shared across packages.
package strutil

import "strings"

// FirstLine returns s up to the first newline, or s when it has none. Used for
// tool headers and picker labels that want only the opening line of a command.
func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
