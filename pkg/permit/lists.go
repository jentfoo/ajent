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

// writeCommands unconditionally mutate state regardless of their arguments. Kept
// tight so a match is a 100%-confident write signal; conditionals (sed -i,
// git <subcommand>, find -exec) live in their own analysers.
var writeCommands = bulk.SliceToSet([]string{
	"rm", "rmdir", "mv", "cp", "touch", "mkdir", "chmod", "chown", "chgrp",
	"ln", "link", "rename", "dd", "truncate", "tee", "shred", "unlink",
	"mkfifo", "mknod", "install", "mktemp", "split", "patch", "setfacl",
	"setfattr", "chattr",
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
