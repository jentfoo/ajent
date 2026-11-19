package permit

import (
	"slices"
	"strings"
)

// sortLongWriteFlags are the long options that write a file, run a command, or
// move scratch output outside TMPDIR.
var sortLongWriteFlags = []string{
	"--output", "--compress-program", "--temporary-directory",
}

// sortWriteFlag reports whether a short token is -o exact, an attached -ofile,
// or a getopt cluster containing the o rune (-ro out.txt). Long forms go
// through abbrevLong.
func sortWriteFlag(tok string) bool {
	if !strings.HasPrefix(tok, "-") || strings.HasPrefix(tok, "--") {
		return false
	}
	return tok == "-o" || (len(tok) > 2 && tok[1] == 'o') ||
		strings.ContainsRune(tok[1:], 'o')
}

// sortTmpFlag reports whether a short token writes scratch output outside
// TMPDIR, which -T does during the sort. Long forms go through abbrevLong.
func sortTmpFlag(tok string) bool {
	if !strings.HasPrefix(tok, "-") || strings.HasPrefix(tok, "--") {
		return false
	}
	return tok == "-T" || (len(tok) > 2 && tok[1] == 'T') ||
		strings.ContainsRune(tok[1:], 'T')
}

// sortReadOnly reports whether a sort invocation is verifiably free of file
// overwrites and command execution. -o/--output writes an arbitrary path,
// --compress-program runs a command, and -T/--temporary-directory writes scratch
// files outside TMPDIR, everything else (plain f, -u, -r, -n, -S) being read-only.
// The three write/exec long options match their getopt_long abbreviations too,
// benign long flags (--stable, --numeric-sort) never prompting.
func sortReadOnly(tokens []string) bool {
	return !slices.ContainsFunc(tokens, func(t string) bool {
		return sortWriteFlag(t) || sortTmpFlag(t) || abbrevLong(t, sortLongWriteFlags...)
	})
}
