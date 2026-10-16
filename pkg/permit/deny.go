package permit

import (
	"slices"
	"strings"

	"github.com/go-analyze/bulk"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/tools"
)

// maxDenyDepth bounds the deny scan's recursion through nested bodies, so a
// pathologically nested line cannot exhaust the stack. Honest shell nests
// far shallower than this.
const maxDenyDepth = 64

// DenyMatches reports whether call is named by a configured denied command:
// an exact tool name for any non-bash tool, or for bash a token-boundary
// match of the command line with head-hiding shapes (quoting, env prefixes,
// keywords, launchers, subshells, substitution, interpreter payloads,
// heredoc bodies) resolved first. Unlike safeMatches it may name core
// writers, denying one being a legitimate safety gate.
func DenyMatches(call agent.ToolCall, cmds []string) bool {
	for _, e := range cmds {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if call.Name != tools.ToolBash && toolNameCovered(call.Name, e) {
			return true
		}
	}
	if call.Name != tools.ToolBash {
		return false
	}
	return deniedScan(scanCommand(bashCommand(call.Input)), cmds, 0)
}

// deniedScan reports whether any segment of one scan names a denied command,
// by collapsed text, resolved argv, interpreter payload, heredoc body, or
// the shell bodies a group, substitution or case pattern embeds.
func deniedScan(s Scan, cmds []string, depth int) bool {
	if depth > maxDenyDepth {
		return false
	}
	for i, seg := range s.Segments {
		var raw string
		if i < len(s.Raw) {
			raw = s.Raw[i]
		}
		if entryCovered(seg, cmds) || denyArgvCovers(raw, cmds) {
			return true
		}
		if deniedPayload(raw, cmds, depth+1) {
			return true // the interpreter's own tokens hide what it runs
		}
		if i < len(s.HeredocBodies) {
			body := s.HeredocBodies[i]
			if deniedHeredoc(raw, body, cmds, depth+1) {
				return true // stdin is shell the line's own tokens never name
			}
			if heredocExpands(s, i) && deniedBodies(body, cmds, depth+1, bodySubst) {
				return true // unquoted delimiter: the body's substitution executes
			}
		}
		if deniedBodies(raw, cmds, depth+1, bodyShell) {
			return true // group, substitution and case bodies run shell of their own
		}
	}
	return false
}

// denyArgvCovers reports whether raw runs a configured entry once resolved
// through resolveDenyArgv or found past a launcher prefix. Deny-side only:
// a safe listing never blesses these shapes.
func denyArgvCovers(raw string, cmds []string) bool {
	if raw == "" {
		return false
	}
	toks := tokenizeRaw(stripDollarQuotes(raw))
	if argvCovers(toks, cmds) {
		return true // verbatim argv, so an entry naming a skipped word still matches
	}
	resolved := resolveDenyArgv(toks)
	return argvCovers(resolved, cmds) || launcherArgvCovers(resolved, cmds)
}

// resolveDenyArgv narrows toks to the head the deny list matches: leading
// shell keywords, KEY=VALUE assignments and launcher prefixes (nohup,
// timeout) skipped.
func resolveDenyArgv(toks []string) []string {
	toks = skipEnvAssigns(skipKeywords(toks))
	return skipEnvAssigns(unwrapLaunchers(toks))
}

// denyLaunchers precede a command without replacing the shell line. The
// allow side never unwraps them, the deny side scanning past them instead.
var denyLaunchers = bulk.SliceToSet([]string{"env", "sudo", "nice", "stdbuf", "xargs", "time"})

// launcherArgvCovers reports whether a launcher-headed argv holds a
// configured entry at any suffix start. Over-matches by design: options and
// value operands can sit between a launcher and the command it wraps.
func launcherArgvCovers(toks []string, cmds []string) bool {
	if _, ok := denyLaunchers[firstToken(toks)]; !ok {
		return false
	}
	for i := 1; i < len(toks); i++ {
		if argvCovers(toks[i:], cmds) {
			return true
		}
	}
	return false
}

// stripDollarQuotes drops the $ from a word-start $'…' or $"…" region: bash
// runs the dequoted word, while tokenizeRaw keeps the $ attached to it.
func stripDollarQuotes(raw string) string {
	if !strings.Contains(raw, "$") {
		return raw
	}
	var b strings.Builder
	b.Grow(len(raw))
	var i int
	n := len(raw)
	for i < n {
		ch := raw[i]
		switch {
		case ch == '\\' && i+1 < n:
			b.WriteString(raw[i : i+2])
			i += 2
		case ch == '\'' || ch == '"':
			end := skipQuoted(raw, i)
			b.WriteString(raw[i:end])
			i = end
		case ch == '$' && i+1 < n && (raw[i+1] == '\'' || raw[i+1] == '"') &&
			(i == 0 || raw[i-1] == ' ' || raw[i-1] == '\t'):
			i++ // word-start $-quote: the region alone forms the word
		default:
			b.WriteByte(ch)
			i++
		}
	}
	return b.String()
}

// bodyMode selects which embedded regions count as bodies: substitution
// regions execute wherever expansion is live, subshell groups and case tails
// only where the text is shell.
type bodyMode bool

const (
	bodySubst bodyMode = false // expansion regions only: $(…), <(…), backticks
	bodyShell bodyMode = true  // every region: subshells and case tails too
)

// deniedBodies reports whether any shell body raw embeds names a denied
// command: subshells, substitution, process substitution and case tails
// all execute text the enclosing tokens never name.
func deniedBodies(raw string, cmds []string, depth int, mode bodyMode) bool {
	return slices.ContainsFunc(shellBodies(raw, mode), func(body string) bool {
		return deniedScan(scanCommand(strings.TrimSpace(body)), cmds, depth)
	})
}

// shellBodies returns the shell text raw embeds under mode, quotes
// respected: single quotes are literal, double quotes keep substituting.
// The tail past a bare group closes with it: subshell, case-pattern and
// function-body positions are command positions. The tail past a $( or <(
// group is not, staying argument text.
func shellBodies(raw string, mode bodyMode) []string {
	var bodies []string
	var i int
	n := len(raw)
	for i < n {
		ch := raw[i]
		switch {
		case ch == '\\' && i+1 < n:
			i += 2 // escaped char, literal in every context modeled here
		case ch == '\'':
			i = skipQuoted(raw, i)
		case ch == '"':
			var inner []string
			inner, i = doubleQuoteBodies(raw, i)
			bodies = append(bodies, inner...)
		case ch == '(':
			subst := i > 0 && (raw[i-1] == '$' || raw[i-1] == '<')
			if mode == bodyShell || subst {
				if body, next, ok := parenBody(raw, i); ok {
					bodies = append(bodies, body)
					if mode == bodyShell && !subst {
						bodies = append(bodies, raw[next:])
					}
					i = next
					continue
				}
			}
			i++
		case ch == ')' && mode == bodyShell && i+1 < n:
			// unbalanced ) closes a case pattern or function header
			bodies = append(bodies, raw[i+1:])
			i++
		case ch == '`':
			if body, next, ok := backtickBody(raw, i); ok {
				bodies = append(bodies, body)
				i = next
				continue
			}
			i++
		default:
			i++
		}
	}
	return bodies
}

// doubleQuoteBodies returns the substitution bodies inside the double-quoted
// region opened at i, with the index past its closing quote.
func doubleQuoteBodies(s string, i int) ([]string, int) {
	var bodies []string
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '"':
			return bodies, j + 1
		case '`':
			if body, next, ok := backtickBody(s, j); ok {
				bodies = append(bodies, body)
				j = next - 1
			}
		case '(':
			if j > i+1 && s[j-1] == '$' { // only $( substitutes inside double quotes
				if body, next, ok := parenBody(s, j); ok {
					bodies = append(bodies, body)
					j = next - 1
				}
			}
		}
	}
	return bodies, len(s) // unterminated
}

// parenBody returns the text inside the balanced group opening at open, with
// the index past its close, ok false when it never closes.
func parenBody(s string, open int) (string, int, bool) {
	var depth int
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '\'', '"':
			i = skipQuoted(s, i) - 1
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[open+1 : i], i + 1, true
			}
		}
	}
	return "", open, false
}

// backtickBody returns the text between the backtick at tick and its pair,
// with the index past it, ok false when unpaired.
func backtickBody(s string, tick int) (string, int, bool) {
	for i := tick + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++ // \` never closes the region
		case '`':
			return s[tick+1 : i], i + 1, true
		}
	}
	return "", tick, false
}

// deniedPayload reports whether a nested-interpreter segment (sh -c, eval)
// wraps a command the deny list names, the payload pulled from the resolved
// tokens and rescanned.
func deniedPayload(raw string, cmds []string, depth int) bool {
	return slices.ContainsFunc(interpreterPayloads(raw), func(p string) bool {
		return deniedScan(scanCommand(strings.TrimSpace(p)), cmds, depth)
	})
}

// interpreterPayloads returns the shell text an interpreter segment runs:
// the words past -c, or eval's arguments joined into one line. Under a
// launcher head every position past it is probed, options, assignments and
// value operands all able to sit between the two. A bare interpreter reads
// stdin, yielding nothing here.
func interpreterPayloads(raw string) []string {
	toks := resolveDenyArgv(tokenizeRaw(stripDollarQuotes(raw)))
	if len(toks) == 0 {
		return nil
	}
	starts := []int{0}
	if _, ok := denyLaunchers[toks[0]]; ok {
		for j := 1; j < len(toks); j++ {
			starts = append(starts, j)
		}
	}
	var payloads []string
	for _, s := range starts {
		head := stripPath(toks[s])
		if head == "eval" {
			// eval joins its arguments with spaces, the words rejoining into one line
			payloads = append(payloads, strings.Join(toks[s+1:], " "))
			continue
		}
		if _, ok := nestedInterpreters[head]; !ok {
			continue
		}
		rest := toks[s+1:]
		for i, tok := range rest {
			if tok == "-c" && i+1 < len(rest) {
				payloads = append(payloads, rest[i+1:]...)
				break
			}
		}
	}
	return payloads
}

// deniedHeredoc reports whether a segment reading heredoc body text runs an
// interpreter whose body names a denied command, the stdin twin of a -c
// payload. Other readers keep their bodies inert, and a quoted delimiter
// changes nothing, the body running either way.
func deniedHeredoc(raw, body string, cmds []string, depth int) bool {
	toks := resolveDenyArgv(tokenizeRaw(stripDollarQuotes(raw)))
	if len(toks) == 0 {
		return false
	}
	if _, ok := nestedInterpreters[stripPath(toks[0])]; !ok {
		return false
	}
	return deniedScan(scanCommand(body), cmds, depth)
}

// heredocExpands reports whether the body feeding segment i had an unquoted
// delimiter, leaving expansion live in its text.
func heredocExpands(s Scan, i int) bool {
	return i >= len(s.HeredocQuoted) || !s.HeredocQuoted[i]
}
