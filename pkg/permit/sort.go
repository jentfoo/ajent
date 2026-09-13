package permit

import (
	"slices"
	"strings"
)

// sortWriteFlag reports whether a short token is -o exact, an attached -ofile,
// or a getopt cluster containing the o rune (-ro out.txt).
func sortWriteFlag(tok string) bool {
	if tok == "--output" || strings.HasPrefix(tok, "--output=") {
		return true
	}
	if !strings.HasPrefix(tok, "-") || strings.HasPrefix(tok, "--") {
		return false
	}
	return tok == "-o" || (len(tok) > 2 && tok[1] == 'o') ||
		strings.ContainsRune(tok[1:], 'o')
}

// sortTmpFlag reports whether a token writes scratch output outside TMPDIR,
// which -T/--temporary-directory does during the sort.
func sortTmpFlag(tok string) bool {
	if tok == "--temporary-directory" || strings.HasPrefix(tok, "--temporary-directory=") {
		return true
	}
	if !strings.HasPrefix(tok, "-") || strings.HasPrefix(tok, "--") {
		return false
	}
	return tok == "-T" || (len(tok) > 2 && tok[1] == 'T') ||
		strings.ContainsRune(tok[1:], 'T')
}

// sortReadOnly reports whether a sort invocation is verifiably free of file
// overwrites and command execution. -o/--output writes an arbitrary path,
// --compress-program runs a command, and -T/--temporary-directory writes scratch
// files outside TMPDIR; everything else (plain f, -u, -r, -n, -S) is read-only.
func sortReadOnly(tokens []string) bool {
	return !slices.ContainsFunc(tokens, func(t string) bool {
		return sortWriteFlag(t) || strings.HasPrefix(t, "--compress-program") ||
			sortTmpFlag(t)
	})
}
