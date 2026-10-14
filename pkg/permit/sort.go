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

// sortLongWrite reports whether tok is one of sort's write/exec long options or
// a getopt_long abbreviation of one. GNU resolves any proper prefix past the
// dashes (--outp=/x, --o f, --temp D) to the option, and whether an
// abbreviation is ambiguous depends on the installed sort's option table, so
// every prefix of these flags is treated as the flag itself. A bare --
// terminates options instead.
func sortLongWrite(tok string) bool {
	if !strings.HasPrefix(tok, "--") {
		return false
	}
	name := tok
	if i := strings.IndexByte(name, '='); i >= 0 {
		name = name[:i]
	}
	if len(name) < 3 {
		return false
	}
	return slices.ContainsFunc(sortLongWriteFlags, func(flag string) bool {
		return strings.HasPrefix(flag, name)
	})
}

// sortWriteFlag reports whether a short token is -o exact, an attached -ofile,
// or a getopt cluster containing the o rune (-ro out.txt). Long forms live in
// sortLongWrite.
func sortWriteFlag(tok string) bool {
	if !strings.HasPrefix(tok, "-") || strings.HasPrefix(tok, "--") {
		return false
	}
	return tok == "-o" || (len(tok) > 2 && tok[1] == 'o') ||
		strings.ContainsRune(tok[1:], 'o')
}

// sortTmpFlag reports whether a short token writes scratch output outside
// TMPDIR, which -T does during the sort. Long forms live in sortLongWrite.
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
		return sortWriteFlag(t) || sortTmpFlag(t) || sortLongWrite(t)
	})
}
