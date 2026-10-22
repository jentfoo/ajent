package tools

import "strings"

// DiffEcho selects when the edit and write results echo a unified diff of the
// change into their tool response. The zero value is the default.
type DiffEcho uint8

const (
	// DiffValidate echoes only changes worth checking: an edit that did not
	// resolve byte-exactly, or a write that displaced existing content.
	DiffValidate DiffEcho = iota
	// DiffNever never echoes a diff; error results are unaffected.
	DiffNever
	// DiffOverwrite echoes write's displaced-content diff only, never an edit's.
	DiffOverwrite
	// DiffAlways echoes every applied change unconditionally.
	DiffAlways
)

// ParseDiffEcho resolves s as a DiffEcho name, case insensitively. The empty
// string is the default. ok reports whether s named a mode.
func ParseDiffEcho(s string) (DiffEcho, bool) {
	switch strings.ToLower(s) {
	case "", "validate":
		return DiffValidate, true
	case "never":
		return DiffNever, true
	case "overwrite":
		return DiffOverwrite, true
	case "always":
		return DiffAlways, true
	default:
		return DiffValidate, false
	}
}

// String returns the configuration name of m.
func (m DiffEcho) String() string {
	switch m {
	case DiffNever:
		return "never"
	case DiffOverwrite:
		return "overwrite"
	case DiffAlways:
		return "always"
	default:
		return "validate"
	}
}

// editDiff reports whether an edit result echoes its diff under m; review marks
// a change worth checking.
func (m DiffEcho) editDiff(review bool) bool {
	switch m {
	case DiffAlways:
		return true
	case DiffValidate:
		return review
	default: // never, overwrite leave edits summary-only
		return false
	}
}

// writeDiff reports whether an overwriting write echoes its displaced-content
// diff under m. A new file has no before side to show in any mode.
func (m DiffEcho) writeDiff() bool { return m != DiffNever }
