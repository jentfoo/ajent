package permit

import (
	"regexp"
	"slices"
	"strings"

	"github.com/go-analyze/bulk"
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
	var i int
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
	timeoutBoolOpts    = []string{"--preserve-status", "--foreground", "-v", gitFlagVerbose}
	timeoutDurationRe  = regexp.MustCompile(`^\d+(\.\d+)?[smhd]?$`)
	timeoutAttachedVal = regexp.MustCompile(`^--(kill-after|signal)=`)
	timeoutShortAttach = regexp.MustCompile(`^-[ks].+`)
)

// unwrapLaunchers strips side-effect-free launcher prefixes (nohup, timeout) so
// classification sees the wrapped command. sudo/env/xargs/nice/stdbuf change
// privilege or environment and are never unwrapped, a confused parse returning
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

// shellKeywords are control-flow words, never command names: a segment headed by
// one is shell syntax whose inner commands vary, so it has no nameable head.
// `time` stays nameable: it heads a plain command (its own binary) and timeout's
// unwrap can expose one, while its keyword form only matters before a pipeline,
// which a segment never holds.
var shellKeywords = bulk.SliceToSet([]string{
	"if", "then", "elif", "else", "fi", "do", "done", "while", "until",
	"for", "case", "esac", "in", "select", "function", "{", "}", "!",
})

// nestedInterpreters run their arguments as a fresh shell line, so what actually
// executes never appears in this line's own tokens. Such a head is unnameable:
// never read-only, never grant-matchable, and (barrier.go) its -c payload is
// scanned for the deny list instead.
var nestedInterpreters = bulk.SliceToSet([]string{"sh", "bash", "zsh", "dash", "eval"})

// interpreterPayloads returns the shell text an sh-like segment runs: the
// arguments following -c (a bare `sh` reads stdin, yielding nothing).
func interpreterPayloads(raw string) []string {
	toks := tokenizeRaw(raw)
	if stripPath(firstToken(toks)) == "eval" {
		// eval concatenates its arguments with spaces, so the words rejoin into one line
		return []string{strings.Join(toks[1:], " ")}
	}
	for i, tok := range toks[1:] {
		if tok == "-c" && i+2 < len(toks) {
			return toks[i+2:]
		}
	}
	return nil
}

// headOf returns the command name a segment runs after unwrapping launchers, or
// ("",false) when none can be named reliably. A leading VAR= assignment is never
// stripped: PATH/LD_PRELOAD/BASH_ENV/ENV, plus any other var a binary reads, can
// hijack what the head actually executes, so such a segment has no trustworthy name
// and must fail closed (never read-only, never matches an existing grant). A shell
// keyword or nested interpreter head likewise names nothing: the real command
// lives in its arguments.
func headOf(seg string) (string, bool) {
	toks := segmentTokens(seg)
	if len(toks) == 0 || envAssignRe.MatchString(firstToken(toks)) {
		return "", false
	}
	h := stripPath(firstToken(toks))
	if h == "" {
		return "", false
	}
	if _, kw := shellKeywords[h]; kw {
		return "", false
	}
	if _, ok := nestedInterpreters[h]; ok {
		return "", false
	}
	return h, true
}

// headKey returns the session-grant key a segment's head stores under: the
// command name, narrowed by one subcommand word when the head takes
// subcommands, so a `go test` grant never covers `go run`. Empty when headOf
// would refuse the head.
func headKey(seg string) (string, bool) {
	h, ok := headOf(seg)
	if !ok {
		return "", false
	}
	if _, sub := subcommandHeads[h]; sub {
		toks := segmentTokens(seg)
		if len(toks) > 1 {
			if s := toks[1]; subcommandWordRe.MatchString(s) {
				h += " " + s
			}
		}
	}
	return h, true
}

// subcommandHeads name commands whose first operand is a subcommand verb, the
// only heads a grant narrows on. Operand commands (rm, ifconfig) stay
// head-granular: their second word names a file, not a capability.
var subcommandHeads = bulk.SliceToSet([]string{
	"git", "go", "make", "docker", "kubectl", "npm", "cargo",
})

// subcommandWordRe matches one plain word, the only argument shape allowed to
// narrow a grant: paths, flags and punctuation carry no subcommand meaning.
var subcommandWordRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// segmentTokens returns the effective head-walkable tokens of a collapsed segment.
func segmentTokens(seg string) []string {
	return unwrapLaunchers(strings.Fields(seg))
}
