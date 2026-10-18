package permit

import (
	"slices"
	"strings"
)

// dateSetLong reports whether name is --set or a getopt_long abbreviation
// of it, date's only long option starting with s.
func dateSetLong(name string) bool {
	return len(name) >= 3 && strings.HasPrefix("--set", name)
}

// dateValueLongs are date's read-only long options taking a separate argument.
var dateValueLongs = []string{"--date", "--file", "--reference"}

// dateValueLong reports whether name is a value-taking read-only long option
// or an abbreviation of one.
func dateValueLong(name string) bool {
	return slices.ContainsFunc(dateValueLongs, func(flag string) bool {
		return len(name) >= 3 && strings.HasPrefix(flag, name)
	})
}

// dateValueShorts are the value-taking shorts across date variants (-d/-D
// describe, -f input format, -r reference, -v display adjust). -s/-t are
// rejected separately, -I carries an optional attached value.
const dateValueShorts = "dDfrv"

// dateReadOnly reports whether a date invocation leaves the system clock
// alone. Setting goes through -s/--set (GNU, busybox), -t (BSD kernel
// timezone) or a bare new_date operand (BSD/macOS, -j opting out of the set).
// Formats start with + and never set.
func dateReadOnly(tokens []string) bool {
	var jFlag, value bool
	for _, tok := range tokens[1:] {
		if value { // argument of the previous value-taking flag
			value = false
			continue
		}
		switch {
		case strings.HasPrefix(tok, "--"):
			name := tok
			if i := strings.IndexByte(name, '='); i >= 0 {
				name = name[:i]
			}
			if dateSetLong(name) {
				return false
			}
			value = dateValueLong(name)
		case strings.HasPrefix(tok, "-") && tok != "-":
			for k := 1; k < len(tok); k++ {
				switch tok[k] {
				case 's', 't':
					return false // clock or kernel timezone set
				case 'I':
					k = len(tok) // optional attached FMT value
				case 'j':
					jFlag = true // display/conversion only
				default:
					if strings.IndexByte(dateValueShorts, tok[k]) >= 0 {
						if k+1 == len(tok) {
							value = true // separate value follows
						} else {
							k = len(tok) // attached value
						}
					}
				}
			}
		case !strings.HasPrefix(tok, "+") && !jFlag:
			return false // bare operand: the BSD/macOS set form
		}
	}
	return true
}
