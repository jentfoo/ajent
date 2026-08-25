package permit

import (
	"regexp"
	"slices"
	"strings"
)

// tokenizeRaw splits a raw segment on unquoted whitespace, stripping quote
// chars and joining adjacent quoted/unquoted pieces (-e's/a/b/' -> -es/a/b/).
func tokenizeRaw(segment string) []string {
	var tokens []string
	b := strings.Builder{}
	var started bool
	flush := func() {
		if !started {
			return
		}
		tokens = append(tokens, b.String())
		b.Reset()
		started = false
	}
	i := 0
	n := len(segment)
	for i < n {
		ch := segment[i]
		if ch == ' ' || ch == '\t' {
			flush()
			i++
			continue
		}
		started = true
		if ch == '\\' && i+1 < n {
			b.WriteByte(segment[i+1])
			i += 2
			continue
		}
		if ch == '"' || ch == '\'' {
			quote := ch
			i++
			for i < n && segment[i] != quote {
				if quote == '"' && segment[i] == '\\' && i+1 < n {
					esc := segment[i+1]
					if esc == '"' || esc == '\\' || esc == '$' || esc == '`' {
						b.WriteByte(esc)
					} else {
						// other backslashes stay literal so sed regex escapes survive
						b.WriteString("\\")
						b.WriteByte(esc)
					}
					i += 2
					continue
				}
				b.WriteByte(segment[i])
				i++
			}
			i++ // closing quote, or past end when unterminated
			continue
		}
		b.WriteByte(ch)
		i++
	}
	flush()
	return tokens
}

// stripPath returns everything after the last / in tok, else trims a .sh suffix.
func stripPath(tok string) string {
	if idx := strings.LastIndexByte(tok, '/'); idx >= 0 {
		return tok[idx+1:]
	}
	return strings.TrimSuffix(tok, ".sh")
}

var (
	timeoutValueOpts   = []string{"-k", "--kill-after", "-s", "--signal"}
	timeoutBoolOpts    = []string{"--preserve-status", "--foreground", "-v", "--verbose"}
	timeoutDurationRe  = regexp.MustCompile(`^\d+(\.\d+)?[smhd]?$`)
	timeoutAttachedVal = regexp.MustCompile(`^--(kill-after|signal)=`)
	timeoutShortAttach = regexp.MustCompile(`^-[ks].+`)
)

// unwrapLaunchers strips side-effect-free launcher prefixes (nohup, timeout) so
// classification sees the wrapped command. sudo/env/xargs/nice/stdbuf change
// privilege or environment and are never unwrapped; a confused parse returns
// the original tokens to fail safe toward prompting.
func unwrapLaunchers(tokens []string) []string {
	cmd := stripPath(firstToken(tokens))
	if cmd == "nohup" {
		return unwrapLaunchers(tokens[1:])
	}
	if cmd != "timeout" {
		return tokens
	}
	j := 1
	for j < len(tokens) && strings.HasPrefix(tokens[j], "-") {
		tok := tokens[j]
		switch {
		case slices.Contains(timeoutBoolOpts, tok):
			j++
		case slices.Contains(timeoutValueOpts, tok):
			j += 2 // separate value form: -k 5, --signal TERM
		case timeoutAttachedVal.MatchString(tok) || timeoutShortAttach.MatchString(tok):
			j++ // attached value form: --signal=TERM, -sTERM, -k5
		default:
			return tokens
		}
	}
	if j >= len(tokens) || !timeoutDurationRe.MatchString(firstToken(tokens[j:])) {
		return tokens
	}
	if j+1 >= len(tokens) {
		return tokens
	}
	return unwrapLaunchers(tokens[j+1:])
}

// firstToken returns the empty string when tokens is nil or empty.
func firstToken(tokens []string) string {
	for _, tok := range tokens {
		return tok
	}
	return ""
}

// envAssignRe matches a leading KEY=VALUE environment assignment.
var envAssignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// headOf returns the command name a segment runs after unwrapping launchers, or
// ("",false) when none can be named reliably. A leading VAR= assignment is never
// stripped: PATH/LD_PRELOAD/BASH_ENV/ENV — or any var a binary reads — can hijack
// what the head actually executes, so such a segment has no trustworthy name and
// must fail closed (never read-only, never matches an existing grant).
func headOf(seg string) (string, bool) {
	toks := segmentTokens(seg)
	if len(toks) == 0 || envAssignRe.MatchString(firstToken(toks)) {
		return "", false
	}
	h := stripPath(firstToken(toks))
	return h, h != ""
}

// segmentTokens returns the effective head-walkable tokens of a collapsed segment.
func segmentTokens(seg string) []string {
	return unwrapLaunchers(strings.Fields(seg))
}
