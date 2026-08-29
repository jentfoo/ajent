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

// workspaceWriteCommand is the flag grammar of one bounded directory command.
// Splitting the two kinds lets an unknown flag fail closed, since a flag that
// consumes a value cannot be told from an operand.
type workspaceWriteCommand struct {
	boolFlags     []string // take no value
	valueFlags    []string // consume the next token, or a joined --flag=value
	ancestorFlags []string // also act on the operand's ancestors, so those need checking too
}

// workspaceWriteCommands are bounded directory changes auto+write runs without the
// model, provided every path argument lands inside the scope. Both are recoverable:
// mkdir only creates, rmdir only removes empty directories. mkdir -p needs no
// ancestor check — it only creates what is missing, and an in-scope operand's
// ancestors already exist down from the root — while rmdir -p removes them.
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
