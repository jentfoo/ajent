package permit

import (
	"regexp"
	"strings"
)

// Scan is the result of one left-to-right pass over a shell command.
type Scan struct {
	// Segments split on unquoted control operators; quoted regions collapse to "".
	Segments []string
	// Raw is the index-aligned verbatim counterpart of Segments (only sed reads it).
	Raw []string
	// HasSplitOp reports any &&, ||, |, ;, & or newline outside quotes.
	HasSplitOp bool
	// HasUnsafeOp reports any >, `, $( or <( outside quotes except discarding redirects.
	HasUnsafeOp bool
}

// nullRedirectRe matches (&>>|&>|>>|1>>|2>>|1>|2>|>) followed by /dev/null.
var nullRedirectRe = regexp.MustCompile(`^(?:&>>?|[12]?>>?)\s*/dev/null`)

// isWordChar reports whether b can continue a word token, matching the reference's
// [A-Za-z0-9._/-] guard class around /dev/null matches.
func isWordChar(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' ||
		b == '.' || b == '_' || b == '-' || b == '/'
}

// matchNullRedirect returns bytes consumed by a /dev/null redirect at i, or 0.
func matchNullRedirect(command string, i int) int {
	m := nullRedirectRe.FindStringIndex(command[i:])
	if len(m) == 0 {
		return 0
	}
	consumed := m[1]
	// digit-fd forms only match at a word start so cat file1>/dev/null isn't eaten
	if command[i] == '1' || command[i] == '2' {
		if i > 0 && isWordChar(command[i-1]) {
			return 0
		}
	}
	// following byte must not continue the name, else /dev/nullfoo is a real file
	if next := i + consumed; next < len(command) && isWordChar(command[next]) {
		return 0
	}
	return consumed
}

// scanCommand walks command once tracking quote state so shell operators inside
// string literals are never mistaken for control flow. Branch order is load-bearing.
func scanCommand(command string) Scan {
	var segments, raw []string
	buf := strings.Builder{}
	rawBuf := strings.Builder{}
	i := 0
	n := len(command)
	var hasSplitOp, hasUnsafeOp bool

	pushSegment := func() {
		// keyed on the collapsed trim so Segments/Raw stay index-aligned; rawBuf
		// always resets so stale verbatim text never leaks forward.
		collapsed := strings.TrimSpace(buf.String())
		if collapsed != "" {
			segments = append(segments, collapsed)
			raw = append(raw, strings.TrimSpace(rawBuf.String()))
		}
		buf.Reset()
		rawBuf.Reset()
	}

	for i < n {
		ch := command[i]

		// backslash escape appends both chars
		if ch == '\\' && i+1 < n {
			buf.WriteByte(ch)
			buf.WriteByte(command[i+1])
			rawBuf.WriteByte(ch)
			rawBuf.WriteByte(command[i+1])
			i += 2
			continue
		}

		// quoted region: collapsed to "" in segments, verbatim (incl. quotes) in raw
		if ch == '"' || ch == '\'' {
			quote := ch
			start := i
			var closed bool
			for i++; i < n; {
				if command[i] == quote {
					closed = true
					i++
					break
				}
				switch quote {
				case '"':
					switch {
					case command[i] == '\\' && i+1 < n:
						i += 2 // \X skips two inside double quotes
					case command[i] == '$' && i+1 < n && command[i+1] == '(':
						hasUnsafeOp = true
						i += 2
					default:
						if command[i] == '`' {
							hasUnsafeOp = true
						}
						i++
					}
				case '\'':
					i++ // single quotes are literal, no escapes or expansion
				default:
					i++
				}
			}
			if !closed {
				hasUnsafeOp = true // unterminated quote; bash would reject the tail anyway
			}
			buf.WriteString(`""`)
			rawBuf.WriteString(command[start:i])
			continue
		}

		// 2>&1 discards stderr only when it stands alone, not as fd redirect (2>&12)
		if strings.HasPrefix(command[i:], "2>&1") {
			var next byte
			if i+4 < n {
				next = command[i+4]
			}
			if next < '0' || next > '9' {
				rawBuf.WriteString("2>&1")
				i += 4
				continue
			}
		}

		// /dev/null redirects discard output; must precede the `>`/`&` branches so
		// &>/dev/null is neither split nor flagged unsafe.
		if nullLen := matchNullRedirect(command, i); nullLen > 0 {
			rawBuf.WriteString(command[i : i+nullLen])
			i += nullLen
			continue
		}

		switch ch {
		case '>', '`':
			hasUnsafeOp = true
		case '$':
			if i+1 < n && command[i+1] == '(' {
				// $(...) expands and executes; treat as unsafe
				hasUnsafeOp = true
				buf.WriteString("$(")
				rawBuf.WriteString("$(")
				i += 2
				continue
			}
		case '<':
			if i+1 < n && command[i+1] == '(' {
				// process substitution executes its contents to feed the read
				hasUnsafeOp = true
				buf.WriteString("<(")
				rawBuf.WriteString("<(")
				i += 2
				continue
			}
		case '&', '|':
			if i+1 < n && (command[i:i+2] == "&&" || command[i:i+2] == "||") {
				hasSplitOp = true
				pushSegment()
				i += 2
				continue
			}
			// single & / | falls through to the split below
		case ';', '\n':
		default:
		}

		if ch == '|' || ch == ';' || ch == '\n' || ch == '&' {
			hasSplitOp = true
			pushSegment()
			i++
			continue
		}
		if ch == '>' || ch == '`' {
			buf.WriteByte(ch)
			rawBuf.WriteByte(ch)
			i++
			continue
		}

		buf.WriteByte(ch)
		rawBuf.WriteByte(ch)
		i++
	}

	pushSegment()
	return Scan{Segments: segments, Raw: raw, HasSplitOp: hasSplitOp, HasUnsafeOp: hasUnsafeOp}
}

// compound reports whether command carries pipes, redirects or substitution that
// defeat per-command session memory. A leading env assignment also counts: it can
// hijack what the head executes, so such a line is never treated as a simple,
// nameable command.
func compound(command string) bool {
	s := scanCommand(command)
	if s.HasSplitOp || s.HasUnsafeOp {
		return true
	}
	for _, seg := range s.Segments {
		for _, re := range findUnsafeFlags {
			if re.MatchString(seg) {
				return true
			}
		}
		if toks := segmentTokens(seg); len(toks) > 0 && envAssignRe.MatchString(firstToken(toks)) {
			return true // leading assignment: not a nameable simple command
		}
	}
	return false
}

// allSegmentsReadOnly reports whether every collapsed segment is verifiably
// read-only. Pipelines are tolerated (splitOp alone isn't fatal); an unsafe op
// disqualifies outright, and each segment must clear find flags plus the
// sed/git/allowlist checks.
func allSegmentsReadOnly(s Scan) bool {
	if s.HasUnsafeOp || len(s.Segments) == 0 {
		return false // unparseable fails safe to the prompt path
	}
	return forEachSegment(s, segmentIsReadOnly)
}

// forEachSegment reports whether ok holds for every segment paired with its
// verbatim raw text, which Segments and Raw keep index-aligned from pushSegment.
func forEachSegment(s Scan, ok func(seg, raw string) bool) bool {
	for i, seg := range s.Segments {
		raw := ""
		if i < len(s.Raw) {
			raw = s.Raw[i]
		}
		if !ok(seg, raw) {
			return false
		}
	}
	return true
}

// segmentIsReadOnly reports whether one collapsed segment (with its verbatim raw)
// names a verifiably read-only command.
func segmentIsReadOnly(seg, raw string) bool {
	for _, re := range findUnsafeFlags {
		if re.MatchString(seg) {
			return false
		}
	}
	tokens := segmentTokens(seg)
	head, ok := headOf(seg)
	if !ok || head == "" { // env-prefixed or unnameable: never read-only
		return false
	}
	switch head {
	case "sed":
		return sedReadSafe(raw) // quoted flags need the verbatim text
	case "git":
		return gitReadOnly(tokens)
	default:
		_, ok := readOnlyCommands[head]
		return ok
	}
}
