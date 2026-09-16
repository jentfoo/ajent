package permit

import (
	"regexp"
	"slices"
	"strings"

	"github.com/go-analyze/bulk"
)

// awkInertLongFlags are long options with no exec or write surface; anything else starting
// with a dash (-f/--file, --profile, --pretty-print, -W, unknown) fails safe to the prompt path.
var awkInertLongFlags = bulk.SliceToSet([]string{
	"--posix", "--traditional", "--re-interval", "--lint", "--lint-old",
})

var (
	// awkSystemCallRe matches an indirect system() call, which runs a command.
	awkSystemCallRe = regexp.MustCompile(`\bsystem\s*\(`)
	// awkGetlinePipeRe matches "cmd" | getline and the gawk coprocess |& form,
	// both of which run cmd to feed the read.
	awkGetlinePipeRe = regexp.MustCompile(`\|[&]?\s*\bgetline\b`)
	// awkAtDirectiveRe matches gawk directives (@include/@load/@namespace load
	// code or libraries) and indirect calls @ident( that invoke a function by name.
	awkAtDirectiveRe = regexp.MustCompile(`@(?:include|load|namespace)\b|@[A-Za-z_][A-Za-z0-9_]*\(`)
)

// isAwkValueFlag reports whether tok names an exact value flag taking the next token as its value.
func isAwkValueFlag(tok string) bool {
	return slices.Contains([]string{"-F", "--field-separator", "-v", "--assign"}, tok)
}

// awkAttachedValue reports whether tok carries a field separator or assignment
// attached: -F, and -vx=1 plus the --name=value long forms.
func awkAttachedValue(tok string) bool {
	if strings.HasPrefix(tok, "-") && !strings.HasPrefix(tok, "--") && len(tok) > 2 {
		switch tok[1] {
		case 'F', 'v':
			return true
		}
		return false
	}
	return strings.HasPrefix(tok, "--field-separator=") || strings.HasPrefix(tok, "--assign=")
}

// awkRegexOpens reports whether a slash at the current scan position opens an
// operand-position regex constant rather than division. Mirrors awk's lexer: only
// after an operator, opening bracket/comma/semicolon/backslash or nothing yet;
// "return" and "in" also put it in operand position.
func awkRegexOpens(out string) bool {
	i := len(out) - 1
	for i >= 0 && (out[i] == ' ' || out[i] == '\t' || out[i] == '\n') {
		i--
	}
	if i < 0 {
		return true // nothing emitted yet: pattern at the start of a script or action
	}
	switch out[i] {
	case '(', '{', '[', ',', ';', '=', '!', '<', '>', '+', '-', '*', '%', '^', '~', '&', '|', '?', ':', '\\':
		return true
	}
	j := i
	for j >= 0 && awkIsWordChar(out[j]) {
		j--
	}
	switch out[j+1 : i+1] {
	case "return", "in":
		return true
	default:
		return false
	}
}

// awkStripStrings returns script with every string literal and regex constant
// replaced by one space, so exec/write vectors inside literals never match.
func awkStripStrings(script string) string {
	var b strings.Builder
	var i int
	n := len(script)
	for i < n {
		ch := script[i]
		if ch == '\\' && i+1 < n {
			b.WriteByte(ch)
			b.WriteByte(script[i+1])
			i += 2 // \X skips two outside quotes; keeps \" visible to later checks
			continue
		}
		if ch == '"' || ch == '\'' {
			quote := ch
			i++
			for i < n && script[i] != quote {
				if quote == '"' && script[i] == '\\' && i+1 < n {
					i += 2 // \X skips two inside double quotes
					continue
				}
				i++
			}
			if i < n {
				i++ // closing quote
			}
			b.WriteByte(' ')
			continue
		}
		if ch == '/' && awkRegexOpens(b.String()) {
			i++ // consume the opening slash; an unterminated one rescan its contents below
			end := -1
			for k := i; k < n; k++ {
				if script[k] == '\\' && k+1 < n {
					k++
					continue
				}
				if script[k] == '/' {
					end = k
					break
				}
			}
			if end < 0 {
				b.WriteByte('/') // unterminated: emit opener, contents stay visible below
				continue
			}
			b.WriteByte(' ')
			i = end + 1
			continue
		}
		b.WriteByte(ch)
		i++
	}
	return b.String()
}

// awkIsWordChar reports whether b can continue an identifier.
func awkIsWordChar(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' ||
		b >= '0' && b <= '9' || b == '_'
}

// awkKeywordAt reports whether kw starts at i in code with a word boundary on both sides.
func awkKeywordAt(code string, i int, kw string) bool {
	if !strings.HasPrefix(code[i:], kw) {
		return false
	}
	end := i + len(kw)
	if i > 0 && awkIsWordChar(code[i-1]) || end < len(code) && awkIsWordChar(code[end]) {
		return false
	}
	return true
}

// awkScriptReadSafe reports whether the collected script text is verifiably free
// of exec and file-write vectors. False positives (prompting a safe call) are
// acceptable; false negatives are not.
func awkScriptReadSafe(script string) bool {
	code := awkStripStrings(script)
	if awkSystemCallRe.MatchString(code) || awkGetlinePipeRe.MatchString(code) ||
		awkAtDirectiveRe.MatchString(code) {
		return false
	}
	var sawPrint bool
	var depth int
	for i := 0; i < len(code); i++ {
		switch code[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ';', '\n':
			if depth == 0 {
				sawPrint = false // statement boundary
			}
		default:
			if !sawPrint && (awkKeywordAt(code, i, "print") || awkKeywordAt(code, i, "printf")) {
				sawPrint = true
			} else if depth == 0 && sawPrint && (code[i] == '>' || code[i] == '|') {
				return false // output redirect or pipe after a print at top level
			}
		}
	}
	return true
}

// awkReadSafe reports whether a raw segment is a verifiably read-only awk call:
// the script is inline (no -f/--file) and free of system(), command pipes,
// output redirects and @ directives. Anything unparseable fails safe to prompt.
func awkReadSafe(raw string) bool {
	tokens := unwrapLaunchers(tokenizeRaw(raw))
	if stripPath(firstToken(tokens)) != "awk" {
		return false
	}
	var scripts []string
	var positional bool
	for j := 1; j < len(tokens); j++ {
		tok := tokens[j]
		if strings.HasPrefix(tok, "-") && tok != "-" {
			switch {
			case isAwkValueFlag(tok):
				j++
				if j >= len(tokens) {
					return false // value flag needs a value
				}
			case awkAttachedValue(tok):
				// field separators and assignments cannot execute; skip the value
			case tok == "-e" || tok == "--expression":
				j++
				if j >= len(tokens) {
					return false // -e needs a script value
				}
				scripts = append(scripts, tokens[j])
			case strings.HasPrefix(tok, "--expression="):
				scripts = append(scripts, strings.TrimPrefix(tok, "--expression="))
			case strings.HasPrefix(tok, "-e") && len(tok) > 2:
				scripts = append(scripts, tok[2:]) // attached -escript value
			default:
				if _, ok := awkInertLongFlags[tok]; !ok {
					return false // -f/--file script-from-file, unknown => prompt
				}
			}
			continue
		}
		// first non-flag token is the positional script when none seen yet; later ones are input files
		if len(scripts) == 0 && !positional {
			scripts = append(scripts, tok)
			positional = true
		}
	}
	if len(scripts) == 0 {
		return false // bare awk with no script is unverifiable
	}
	return awkScriptReadSafe(strings.Join(scripts, "\n"))
}
