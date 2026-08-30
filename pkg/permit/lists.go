package permit

import (
	"regexp"

	"github.com/go-analyze/bulk"
)

// readOnlyCommands are verifiably side-effect-free; a matching head is auto-allowed.
var readOnlyCommands = bulk.SliceToSet([]string{
	"awk", "ls", "find", "grep", "rg", "diff", "which", "ps", "jq",
	"cat", "echo", "head", "tail", "wc", "file", "cd", "pwd", "sort", "du",
	"date", "od",
})

// workspaceWriteCommand is the flag grammar of one bounded directory command;
// keeping the kinds separate lets an unknown flag fail closed.
type workspaceWriteCommand struct {
	boolFlags     []string // take no value
	valueFlags    []string // consume the next token, or a joined --flag=value
	ancestorFlags []string // also act on the operand's ancestors, so those need checking too
}

// workspaceWriteCommands are the bounded directory changes auto+write runs
// without the model when every path argument lands inside the scope: both are
// recoverable (mkdir only creates, rmdir only removes empty directories).
var workspaceWriteCommands = map[string]workspaceWriteCommand{
	"mkdir": {
		boolFlags:  []string{"-p", "--parents", "-v", "--verbose", "-Z"},
		valueFlags: []string{"-m", "--mode", "--context"},
	},
	"rmdir": {
		boolFlags:     []string{"-p", "--parents", "-v", "--verbose", "--ignore-fail-on-non-empty"},
		ancestorFlags: []string{"-p", "--parents"},
	},
}

// findUnsafeFlags match find(1) actions that delete, execute or write files.
var findUnsafeFlags = []*regexp.Regexp{
	regexp.MustCompile(`-exec\b`),
	regexp.MustCompile(`-execdir\b`),
	regexp.MustCompile(`-ok\b`),
	regexp.MustCompile(`-okdir\b`),
	regexp.MustCompile(`-delete\b`),
	// -fprint and -fprintf each need their own pattern: \b does not cross the
	// shared word boundary, so one match never covers the other.
	regexp.MustCompile(`-fprint\b`),
	regexp.MustCompile(`-fprint0\b`),
	regexp.MustCompile(`-fprintf\b`),
	regexp.MustCompile(`-fls\b`),
}
