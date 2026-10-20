package permit

import (
	"slices"
	"strings"
)

// fileLongs are file's read-only long options; values and operands are reads
// alike, so value-taking and plain options share one table.
var fileLongs = []string{
	"--brief", "--extension", "--exclude", "--files-from", "--keep-going",
	"--magic-file", "--mime", "--mime-encoding", "--mime-type",
	"--separator", "--special-files",
}

// fileShorts are file's read-only shorts. The write and exec forms stay off
// the table on purpose: -C compiles a magic database, -p utimes analysed
// files, -S drops libmagic's sandbox, -z/-Z run decompression helpers.
const fileShorts = "0IbcefFhikLmnNrsv"

// fileReadOnly reports whether a file invocation reads only. Options off the
// tables are unverifiable, the writing and helper-running spellings failing
// safe as unknowns.
func fileReadOnly(tokens []string) bool {
	for _, tok := range tokens[1:] {
		switch {
		case tok == "--" || tok == "-":
		case strings.HasPrefix(tok, "--"):
			name := tok
			if i := strings.IndexByte(name, '='); i >= 0 {
				name = name[:i]
			}
			if !slices.ContainsFunc(fileLongs, func(flag string) bool {
				return len(name) >= 3 && strings.HasPrefix(flag, name)
			}) {
				return false // unknown or writing long option
			}
		case strings.HasPrefix(tok, "-") && tok != "-":
			for k := 1; k < len(tok); k++ {
				if strings.IndexByte(fileShorts, tok[k]) < 0 {
					return false // unknown or writing short
				}
			}
		}
	}
	return true
}
