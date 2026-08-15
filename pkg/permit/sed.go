package permit

import (
	"regexp"
	"strings"

	"github.com/go-analyze/bulk"
)

// sedWrite reports whether command contains an in-place edit. Runs over the raw
// segments so quoted flags (sed "-i") are caught; scripts themselves are left to
// sedReadSafe, which fails safe to the prompt path.
func sedWrite(command string) bool {
	s := scanCommand(command)
	for _, raw := range s.Raw {
		tokens := unwrapLaunchers(tokenizeRaw(raw))
		if stripPath(firstToken(tokens)) != "sed" {
			continue
		}
		for j := 1; j < len(tokens); j++ {
			// -i in a combined boolean-short token (-ni, -Ei, -i.bak). Letters
			// restricted to sed's boolean shorts so an attached -e script is not
			// mistaken for -i.
			if sedShortWriteRe.MatchString(tokens[j]) {
				return true
			}
			// GNU long form, with or without =suffix
			if sedLongWriteRe.MatchString(tokens[j]) {
				return true
			}
		}
	}
	return false
}

var (
	sedShortWriteRe = regexp.MustCompile(`^-[nErsuz]*i`)
	sedLongWriteRe  = regexp.MustCompile(`^--in-place(=|$)`)
)

// sedBoolFlags match read-only short-flag clusters to skip when validating.
var sedBoolFlagRe = regexp.MustCompile(`^-[nErsuz]+$`)

// sedLongReadFlags are the read-only long forms; anything else starting with a
// dash (-f/--file, --, unknown) fails safe to prompt.
var sedLongReadFlags = bulk.SliceToSet([]string{
	"--quiet", "--silent", "--posix", "--regexp-extended", "--separate",
	"--null-data", "--unbuffered", "--sandbox",
})

// sedAddr matches one address: $, a line number (optionally N~M), or /regex/.
const sedAddr = `(?:\$|\d+(?:~\d+)?|/(?:[^/\\]|\\.)*/)`

// sedAddrPrefix is an optional single address or range with trailing !. RE2
// supports \b and non-capturing groups, so this ports from the reference as-is.
var (
	sedAddrStartRe = regexp.MustCompile("^(?:" + sedAddr +
		`(?:\s*,\s*(?:` + sedAddr + `))?\s*!?\s*)?`)
	// sedSimpleCmdRe matches an optional address prefix followed by a
	// non-writing, non-executing single-letter command.
	sedSimpleCmdRe = regexp.MustCompile("^" +
		"(?:" + sedAddr + `(?:\s*,\s*(?:` + sedAddr + `))?\s*!?\s*)?` +
		"[pdq=nNxhHgG]$")
)

// isSedDelimiter reports whether b can separate a substitution: punctuation,
// never a word char, whitespace or backslash.
func isSedDelimiter(b byte) bool {
	if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' ||
		b == '_' || b == '\\' {
		return false
	}
	switch b {
	case ' ', '\t', '\n', '\r':
		return false
	default:
		return true
	}
}

// sedScanToDelim returns the index of the first unescaped delim in s[i:], or -1.
func sedScanToDelim(s string, i int, delim byte) int {
	for i < len(s) {
		if s[i] == '\\' && i+1 < len(s) {
			i += 2 // escaped delimiter is part of the body
			continue
		}
		if s[i] == delim {
			return i
		}
		i++
	}
	return -1
}

// sedAfterAddress strips an optional leading address/range (with !) so the
// hand-written parsers only see the command proper.
func sedAfterAddress(cmd string) string {
	loc := sedAddrStartRe.FindStringIndex(cmd)
	if loc == nil {
		return cmd
	}
	return cmd[loc[1]:]
}

// parseSedSubst validates an s/pat/rep/flags substitution. Flags are restricted
// to [gpiImM0-9]; w and e fail by design.
func parseSedSubst(cmd string) bool {
	n := len(cmd)
	if n < 3 || cmd[0] != 's' {
		return false
	}
	delim := cmd[1]
	if !isSedDelimiter(delim) {
		return false
	}
	d2 := sedScanToDelim(cmd, 2, delim)
	if d2 < 0 {
		return false // no closing delimiter after the pattern
	}
	d3 := sedScanToDelim(cmd, d2+1, delim)
	if d3 < 0 {
		return false // no closing delimiter after the replacement
	}
	for i := d3 + 1; i < n; i++ {
		switch cmd[i] {
		case 'g', 'p', 'i', 'I', 'm', 'M':
			continue
		default:
			if cmd[i] < '0' || cmd[i] > '9' {
				return false
			}
		}
	}
	return true
}

// parseSedTranslit validates a y/.../.../ transliteration; no flags allowed.
func parseSedTranslit(cmd string) bool {
	n := len(cmd)
	if n < 3 || cmd[0] != 'y' {
		return false
	}
	delim := cmd[1]
	if !isSedDelimiter(delim) {
		return false
	}
	d2 := sedScanToDelim(cmd, 2, delim)
	if d2 < 0 {
		return false
	}
	d3 := sedScanToDelim(cmd, d2+1, delim)
	// the command ends at the third delimiter; no flags permitted
	return d3 >= 0 && d3 == n-1
}

// sedCommandReadSafe reports whether a single script command is read-only.
func sedCommandReadSafe(cmd string) bool {
	if sedSimpleCmdRe.MatchString(cmd) {
		return true
	}
	rest := sedAfterAddress(cmd)
	return parseSedSubst(rest) || parseSedTranslit(rest)
}

// sedReadSafe reports whether a raw segment is a verifiably read-only sed call:
// the script is recoverable inline (no -f/--file) and every command passes
// sedCommandReadSafe. Anything unparseable fails safe to the prompt path.
func sedReadSafe(raw string) bool {
	tokens := unwrapLaunchers(tokenizeRaw(raw))
	if stripPath(firstToken(tokens)) != "sed" {
		return false
	}
	var scripts []string
	positional := false
	for j := 1; j < len(tokens); j++ {
		tok := tokens[j]
		if strings.HasPrefix(tok, "-") && tok != "-" {
			switch {
			case tok == "-e" || tok == "--expression":
				j++
				if j >= len(tokens) {
					return false // -e needs a script value
				}
				scripts = append(scripts, tokens[j])
			case strings.HasPrefix(tok, "--expression="):
				scripts = append(scripts, strings.TrimPrefix(tok, "--expression="))
			case strings.HasPrefix(tok, "-e"):
				// attached -escript value
				scripts = append(scripts, tok[2:])
			case sedBoolFlagRe.MatchString(tok) || isSedLongReadFlag(tok):
				// read-only flag; skip
			default:
				// -f/--file (script unverifiable), --, unknown flags → prompt
				return false
			}
			continue
		}
		// first non-flag token is the positional script when none seen yet;
		// later ones are input files.
		if len(scripts) == 0 && !positional {
			scripts = append(scripts, tok)
			positional = true
		}
	}
	if len(scripts) == 0 {
		return false
	}
	sawCommand := false
	for _, part := range strings.FieldsFunc(strings.Join(scripts, "\n"), isSedSplitter) {
		cmd := strings.TrimSpace(part)
		if cmd == "" {
			continue
		}
		sawCommand = true
		if !sedCommandReadSafe(cmd) {
			return false
		}
	}
	return sawCommand
}

// isSedLongReadFlag reports whether tok names a read-only long sed flag.
func isSedLongReadFlag(tok string) bool {
	_, ok := sedLongReadFlags[tok]
	return ok
}

// isSedSplitter separates script commands on ; or newline.
func isSedSplitter(r rune) bool {
	return r == ';' || r == '\n'
}
