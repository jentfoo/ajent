package permit

import (
	"slices"
	"strings"
)

// dateValueLongs are date's read-only long options taking a separate argument.
var dateValueLongs = []string{"--date", "--file", "--reference"}

// datePlainLongs are date's read-only long options taking no separate value.
// --iso-8601 and --rfc-3339 carry only attached =FMT values.
var datePlainLongs = []string{
	"--utc", "--universal", "--iso-8601", "--rfc-2822", "--rfc-3339",
}

// dateValueLong reports whether name is, or abbreviates, a value-taking
// read-only long option.
func dateValueLong(name string) bool {
	return slices.ContainsFunc(dateValueLongs, func(flag string) bool {
		return len(name) >= 3 && strings.HasPrefix(flag, name)
	})
}

// datePlainLong reports whether name is, or abbreviates, a value-free
// read-only long option.
func datePlainLong(name string) bool {
	return slices.ContainsFunc(datePlainLongs, func(flag string) bool {
		return len(name) >= 3 && strings.HasPrefix(flag, name)
	})
}

// dateValueShorts are the value-taking read-only shorts across date variants
// (-d/-D describe or input format, -f input format, -r reference, -v display
// adjust), -I carrying its optional value attached.
const dateValueShorts = "dDfrv"

// dateReadOnly reports whether a date invocation leaves the system clock
// alone. Setting goes through -s/--set (GNU, busybox), -t (BSD kernel
// timezone) or a bare new_date operand (BSD/macOS, -j opting out of the set),
// and formats start with +. Options off the tables are unverifiable: the
// clock-setting spellings and busybox's -S fail safe as unknowns.
func dateReadOnly(tokens []string) bool {
	var jFlag, value bool
	for _, tok := range tokens[1:] {
		if value { // argument of the previous value-taking flag
			value = false
			continue
		}
		switch {
		case tok == "--":
		case strings.HasPrefix(tok, "--"):
			name := tok
			attached := strings.Contains(tok, "=")
			if i := strings.IndexByte(name, '='); i >= 0 {
				name = name[:i]
			}
			if dateValueLong(name) {
				if !attached {
					value = true
				}
			} else if !datePlainLong(name) {
				return false // unknown or clock-setting long option
			}
		case strings.HasPrefix(tok, "-") && tok != "-":
			for k := 1; k < len(tok); k++ {
				switch c := tok[k]; {
				case c == 'I':
					k = len(tok) // optional attached FMT value
				case c == 'j':
					jFlag = true // display/conversion only
				case c == 'u' || c == 'R':
				case strings.IndexByte(dateValueShorts, c) >= 0:
					if k+1 == len(tok) {
						value = true // separate value follows
					} else {
						k = len(tok) // attached value
					}
				default:
					return false // unknown or clock-setting short
				}
			}
		case !strings.HasPrefix(tok, "+") && !jFlag:
			return false // bare operand: the BSD/macOS set form
		}
	}
	return true
}
