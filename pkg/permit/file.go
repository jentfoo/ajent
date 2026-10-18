package permit

import (
	"slices"
	"strings"
)

// fileCompileFlag reports whether tok is file's magic-compile flag: -C in a
// short cluster, or --compile by exact or getopt_long abbreviated spelling.
func fileCompileFlag(tok string) bool {
	if strings.HasPrefix(tok, "--") {
		name := tok
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		return len(name) >= 3 && strings.HasPrefix("--compile", name)
	}
	return strings.HasPrefix(tok, "-") && tok != "-" &&
		strings.ContainsRune(tok[1:], 'C')
}

// fileReadOnly reports whether a file invocation reads only. -C/--compile
// writes a compiled magic database beside its -m sources, any spelling of
// the flag disqualifying.
func fileReadOnly(tokens []string) bool {
	return !slices.ContainsFunc(tokens, fileCompileFlag)
}
