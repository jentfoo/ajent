package permit

import (
	"regexp"
	"strings"
)

// Scan is the result of one left-to-right pass over a shell command.
type Scan struct {
	// Segments split on unquoted control operators, quoted regions collapsing to "".
	Segments []string
	// Raw is the index-aligned verbatim counterpart of Segments, sed/awk/rg/sort reading it.
	Raw []string
	// Redirects are the targets of every > or >> found outside quotes, fd merges
	// and /dev/null excluded, raw verbatim text when it differs from the target.
	Redirects []Redirect
	// HasSplitOp reports any &&, ||, |, ;, & or newline outside quotes.
	HasSplitOp bool
	// HasUnsafeOp reports any `, $( or <( outside quotes, or anything unparseable.
	HasUnsafeOp bool
}

// Redirect is one output redirect target. Raw carries the verbatim target only
// when it differs from Target (quoting), empty meaning Target is already raw.
type Redirect struct {
	Target string // where the command writes
	Raw    string
}

// nullRedirectRe matches (&>>|&>|>>|1>>|2>>|1>|2>|>) followed by /dev/null.
var nullRedirectRe = regexp.MustCompile(`^(?:&>>?|[12]?>>?)\s*/dev/null`)

// isWordChar reports whether b can continue a word token, matching the reference's
// [A-Za-z0-9._/-] guard class around /dev/null matches.
func isWordChar(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' ||
		b == '.' || b == '_' || b == '-' || b == '/'
}

// parseRedirectTarget consumes one > or >> at i, returning the redirect and the
// index past its target, ok false when no word follows or the target carries
// shell syntax the scan cannot safely read (an unquoted paren is procsub or a
// subshell, an escaped newline a line continuation).
func parseRedirectTarget(command string, i int) (r Redirect, next int, ok bool) {
	j := i + 1
	if j < len(command) && command[j] == '>' {
		j++ // append form, same target check
	}
	if j < len(command) && command[j] == '&' {
		return r, 0, false // >&n fd merge, not a path
	}
	for j < len(command) && (command[j] == ' ' || command[j] == '\t') {
		j++
	}
	if j >= len(command) || strings.ContainsRune(";&|<>()\n", rune(command[j])) {
		return r, 0, false // no word follows, bash would reject the line
	}
	var target, raw strings.Builder
	var quoted bool
	for j < len(command) {
		c := command[j]
		switch {
		case c == '"' || c == '\'':
			// one quote pair is consumed per iteration, so "a"'b' joins like the shell
			quote := c
			quoted = true
			raw.WriteByte(c)
			j++
			for j < len(command) && command[j] != quote {
				if quote == '"' && command[j] == '\\' && j+1 < len(command) {
					raw.WriteString(command[j : j+2])
					target.WriteByte(command[j+1]) // approximated, see RedirectsUnresolved
					j += 2
					continue
				}
				raw.WriteByte(command[j])
				target.WriteByte(command[j])
				j++
			}
			if j >= len(command) {
				return r, 0, false // unterminated quote
			}
			raw.WriteByte(quote)
			j++
		case c == '\\' && j+1 < len(command):
			if command[j+1] == '\n' {
				return r, 0, false // line continuation hides the real target word
			}
			raw.WriteString(command[j : j+2])
			target.WriteByte(command[j+1])
			j += 2
		case strings.ContainsRune(" \t;&|<>()\n", rune(c)):
			return Redirect{Target: target.String(), Raw: verbatim(raw.String(), quoted)}, j, true
		default:
			raw.WriteByte(c)
			target.WriteByte(c)
			j++
		}
	}
	return Redirect{Target: target.String(), Raw: verbatim(raw.String(), quoted)}, j, true
}

// verbatim returns the raw target text when quoting shaped it, else empty.
func verbatim(raw string, quoted bool) string {
	if !quoted {
		return ""
	}
	return raw
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

// heredoc captures one pending here-document: its terminator word, whether the
// delimiter was quoted (a quoted delimiter disables expansion in the body) and
// whether the <<- form strips body tabs.
type heredoc struct {
	delim  string
	quoted bool
	tabs   bool
}

// parseHeredocMarker returns the here-document pending after the << or <<- at i
// and the index just past its marker, ok false when no delimiter word follows.
func parseHeredocMarker(command string, i int) (hd heredoc, next int, ok bool) {
	j := i + 2
	if j < len(command) && command[j] == '-' {
		hd.tabs = true
		j++
	}
	for j < len(command) && (command[j] == ' ' || command[j] == '\t') {
		j++
	}
	if j >= len(command) || command[j] == '\n' || command[j] == ';' {
		return hd, 0, false // no delimiter word follows: not a here-document
	}
	var b strings.Builder
	if q := command[j]; q == '\'' || q == '"' {
		hd.quoted = true
		j++
		for j < len(command) && command[j] != q && command[j] != '\n' {
			b.WriteByte(command[j])
			j++
		}
		if j >= len(command) || command[j] != q {
			return hd, 0, false // unterminated or newline-crossing quote: no marker here
		}
		j++
	} else {
		for j < len(command) && isWordChar(command[j]) {
			b.WriteByte(command[j])
			j++
		}
	}
	hd.delim = b.String()
	if hd.delim == "" {
		return hd, 0, false
	}
	return hd, j, true
}

// consumeHeredocBodies skips the pending docs' bodies starting at pos, returning
// the index after the last terminator. Body lines are the reading command's data,
// never shell, so they contribute no segments. An unquoted delimiter leaves
// expansion alive, so a body carrying $( or ` reports unsafe, as does a missing
// terminator.
func consumeHeredocBodies(command string, pos int, docs []heredoc) (next int, unsafe bool) {
	for _, d := range docs {
		var terminated bool
		i := pos
		for i < len(command) {
			j := strings.IndexByte(command[i:], '\n')
			var line string
			if j < 0 {
				line = command[i:]
				i = len(command)
			} else {
				line = command[i : i+j]
				i += j + 1
			}
			cand := line
			if d.tabs {
				cand = strings.TrimLeft(cand, "\t")
			}
			if cand == d.delim {
				terminated = true
				break
			}
			if !d.quoted && (strings.Contains(line, "$(") || strings.Contains(line, "`")) {
				unsafe = true // the body expands, substitution executes shell
			}
		}
		if !terminated {
			return len(command), true // bash stops reading at EOF, fail safe
		}
		pos = i
	}
	return pos, unsafe
}

// scanCommand walks command once tracking quote state so shell operators inside
// string literals are never mistaken for control flow. Here-document bodies are
// skipped as the reading command's data, never shell. Branch order is
// load-bearing.
func scanCommand(command string) Scan {
	var segments, raw []string
	buf := strings.Builder{}
	rawBuf := strings.Builder{}
	var i int
	n := len(command)
	var hasSplitOp, hasUnsafeOp bool
	var redirects []Redirect
	var heredocs []heredoc
	var depth int // open (, $( or <( nesting: a << inside is a shift, not a marker

	pushSegment := func() {
		// keyed on the collapsed trim so Segments/Raw stay index-aligned, and rawBuf
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
				hasUnsafeOp = true // unterminated quote, bash would reject the tail anyway
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

		// /dev/null redirects discard output, and must precede the `>`/`&` branches so
		// &>/dev/null is neither split nor flagged unsafe.
		if nullLen := matchNullRedirect(command, i); nullLen > 0 {
			rawBuf.WriteString(command[i : i+nullLen])
			i += nullLen
			continue
		}

		// paren nesting so arithmetic shifts ($((1<<2))) never read as markers. A
		// bare ( opens a subshell: its contents execute, so like $( it fails unsafe
		// rather than being scanned as this line's own commands.
		if ch == '(' {
			hasUnsafeOp = true // subshell spawn
			depth++
		} else if ch == ')' && depth > 0 {
			depth--
		}

		// <<< here-string: feeds a literal word on stdin, never a here-document.
		// Handled ahead of << so the marker branch cannot start at its second <.
		if ch == '<' && i+2 < n && command[i+1] == '<' && command[i+2] == '<' {
			buf.WriteString("<<<")
			rawBuf.WriteString("<<<")
			i += 3
			continue
		}

		// here-document marker: the body is data, never shell, so it must not become
		// segments. <<< here-strings stay excluded: their word is scanned normally.
		if ch == '<' && depth == 0 && i+1 < n && command[i+1] == '<' {
			if hd, next, ok := parseHeredocMarker(command, i); ok {
				heredocs = append(heredocs, hd)
				marker := "<<"
				if hd.tabs {
					marker += "-"
				}
				if hd.quoted {
					marker += `""` // collapsed form keeps quote state invisible to callers
				} else {
					marker += hd.delim
				}
				buf.WriteString(marker)
				rawBuf.WriteString(command[i:next])
				i = next
				continue
			}
		}

		switch ch {
		case '>':
			r, next, ok := parseRedirectTarget(command, i)
			if !ok {
				hasUnsafeOp = true // unparseable target, bash may still read it
				break
			}
			redirects = append(redirects, r)
			if depth == 0 {
				i = next // parsed target chars drop from the segment
				continue
			}
		case '`':
			hasUnsafeOp = true
		case '$':
			if i+1 < n && command[i+1] == '(' {
				// $(...) expands and executes, treat as unsafe
				hasUnsafeOp = true
				depth++
				buf.WriteString("$(")
				rawBuf.WriteString("$(")
				i += 2
				continue
			}
		case '<':
			if i+1 < n && command[i+1] == '(' {
				// process substitution executes its contents to feed the read
				hasUnsafeOp = true
				depth++
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

		// control operators split segments only outside a subshell: inside parens the
		// whole group is one segment, so (a && b) keeps its shape. An unbalanced
		// close paren cannot name a command either, failing safe as unsafe.
		if (ch == '|' || ch == ';' || ch == '\n' || ch == '&') && depth == 0 {
			hasSplitOp = true
			pushSegment()
			if ch == '\n' && len(heredocs) > 0 {
				var hu bool
				i, hu = consumeHeredocBodies(command, i+1, heredocs)
				if hu {
					hasUnsafeOp = true
				}
				heredocs = heredocs[:0]
			} else {
				i++
			}
			continue
		}
		if ch == ')' && depth == 0 {
			hasUnsafeOp = true // unbalanced: bash rejects the line
		}
		if ch == '`' || ch == '>' {
			// ` always; > only inside a substitution or unparseable, both unsafe
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
	if len(heredocs) > 0 {
		hasUnsafeOp = true // dangling marker, bash reads stdin unboundedly
	}
	return Scan{
		Segments:    segments,
		Raw:         raw,
		Redirects:   redirects,
		HasSplitOp:  hasSplitOp,
		HasUnsafeOp: hasUnsafeOp,
	}
}

// RedirectsUnresolved reports whether any redirect target carries expansion the
// shell resolves after analysis: $, backtick, glob or brace expansion could
// write somewhere the check never saw. Quoted targets count as unresolved too:
// " escapes inside them are only approximated, never a trusted literal path.
func (s Scan) RedirectsUnresolved() bool {
	for _, r := range s.Redirects {
		if r.Raw != "" || strings.ContainsAny(r.Target, "~$`{}*?[") {
			return true
		}
	}
	return false
}

// RedirectsAll reports whether ok holds for every redirect target.
func (s Scan) RedirectsAll(ok func(target string) bool) bool {
	for _, r := range s.Redirects {
		if !ok(r.Target) {
			return false
		}
	}
	return true
}

// compound reports whether command carries pipes, redirects or substitution that
// defeat per-command session memory. A leading env assignment also counts: it can
// hijack what the head executes, so such a line is never treated as a simple,
// nameable command. An unresolved redirect target counts too: a grant can never
// name what the shell expands at run time.
func compound(command string) bool {
	s := scanCommand(command)
	if s.HasSplitOp || s.HasUnsafeOp || s.RedirectsUnresolved() {
		return true
	}
	for i, seg := range s.Segments {
		var raw string
		if i < len(s.Raw) {
			raw = s.Raw[i]
		}
		for _, re := range findUnsafeFlags {
			if re.MatchString(seg) || re.MatchString(raw) {
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
// read-only and no redirect can write somewhere unseen. Pipelines are tolerated
// (splitOp alone isn't fatal), an unsafe op or unresolved redirect target
// disqualifying outright, and each segment must clear find flags plus the
// sed/git/allowlist checks. Resolved targets are never read-only here: they
// write, and only a write scope may verify where.
func allSegmentsReadOnly(s Scan) bool {
	if s.HasUnsafeOp || len(s.Segments) == 0 || len(s.Redirects) > 0 {
		return false // unparseable or writing: fail safe to the prompt path
	}
	return forEachSegment(s, segmentIsReadOnly)
}

// forEachSegment reports whether ok holds for every segment paired with its
// verbatim raw text, which Segments and Raw keep index-aligned from pushSegment.
func forEachSegment(s Scan, ok func(seg, raw string) bool) bool {
	for i, seg := range s.Segments {
		var raw string
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
// names a verifiably read-only command. find's flags are matched against both the
// collapsed and verbatim text so a quoted "-delete" can never slip past, and any
// env assignment defeats the verdict since the shell would treat it as one too.
func segmentIsReadOnly(seg, raw string) bool {
	for _, re := range findUnsafeFlags {
		if re.MatchString(seg) || re.MatchString(raw) {
			return false
		}
	}
	for _, tok := range tokenizeRaw(raw) {
		if envAssignRe.MatchString(tok) {
			return false // an argument-position assignment is still shell state
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
	case "awk":
		return awkReadSafe(raw) // quoted scripts need the verbatim text, like sed
	case "rg":
		return rgReadOnly(unwrapLaunchers(tokenizeRaw(raw)))
	case "sort":
		return sortReadOnly(unwrapLaunchers(tokenizeRaw(raw)))
	default:
		_, ok := readOnlyCommands[head]
		return ok
	}
}
