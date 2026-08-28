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
