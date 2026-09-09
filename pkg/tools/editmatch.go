package tools

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jentfoo/ajent/pkg/strutil"
)

// matchTier names how an op's oldText was located in the file. Anything above
// tierExact means the model's text was not byte-exact, which the result says so
// the next edit can be written correctly.
type matchTier uint8

const (
	tierExact  matchTier = iota // byte for byte
	tierCanon                   // unicode lookalikes and trailing whitespace
	tierIndent                  // uniform indentation shift
	tierFuzzy                   // short runs mis-transcribed where the edit rewrites
)

const (
	// fuzzyLineLimit bounds how far one line may drift and still heal. Past a few
	// characters a difference is different intent, not a mis-transcription.
	fuzzyLineLimit = 4
	// fuzzyDriftLines bounds how many lines of one edit may drift. Without it a
	// large block accumulates unlimited drift, which is evidence of the wrong
	// region rather than of a mis-transcription.
	fuzzyDriftLines = 8
	// fuzzyRivalMargin is how far clear of the runner-up a match must be. A rival
	// under fuzzyLineLimit already refuses, so this only separates a thin match.
	fuzzyRivalMargin = 2
)

// lineRange is a half-open span of 0-based lines.
type lineRange struct{ from, to int }

// lineShift records that lines from at onward moved by delta, at being a 0-based
// line in the text an apply ran against.
type lineShift struct{ at, delta int }

// shiftRanges returns prior moved into the line space the apply behind shifts
// produced, so ranges recorded before it still name the same text.
func shiftRanges(prior []lineRange, shifts []lineShift) []lineRange {
	if len(prior) == 0 || len(shifts) == 0 {
		return prior
	}
	out := make([]lineRange, 0, len(prior))
	for _, r := range prior {
		from, to := shiftLine(shifts, r.from), shiftLine(shifts, r.to)
		out = append(out, lineRange{from, max(to, from+1)})
	}
	return out
}

// shiftLine returns line moved by every shift landing at or before it.
func shiftLine(shifts []lineShift, line int) int {
	n := line
	for _, s := range shifts {
		if s.at <= line {
			n += s.delta
		}
	}
	return n
}

// match is one located region of the LF buffer with the text to write there.
// The replacement is the op's newText unless a tier reindented the match, in
// which case it carries the same shift so the file's indentation survives.
type match struct {
	s, e int
	repl string
}

// findMatches locates every occurrence of old in buf, escalating through the
// match tiers until one yields a result. Escalation happens only on zero
// matches: a tier that finds several leaves the ambiguity for the caller to
// reject rather than guessing which was meant.
func findMatches(buf, old, replacement string, edited []lineRange) ([]match, matchTier) {
	if ms := exactMatches(buf, old, replacement); len(ms) > 0 {
		return ms, tierExact
	}
	if old == replacement {
		// no tier can heal a self-replacement: without a rewritten span there is
		// nothing to prove the drift was the intended change rather than a typo
		return nil, tierExact
	}
	if !nonBlank(old) {
		return nil, tierExact // folds to bare newlines, which match every line boundary
	}
	cbuf, cmap := canonical(buf)
	cold, _ := canonical(old)
	// skip canon when old==new under folding: matching rewrites already-correct
	// text with itself, so let it fail instead. Indent below still runs.
	if !canonEq(old, replacement) {
		if ms := canonMatches(buf, cbuf, cmap, cold, old, replacement); len(ms) > 0 {
			return preserveQuoted(buf, ms), tierCanon
		}
	}
	if ms := indentMatches(buf, cbuf, cmap, old, replacement); len(ms) > 0 {
		return preserveQuoted(buf, ms), tierIndent
	}
	if ms := fuzzyMatches(buf, cbuf, cmap, old, replacement, edited); len(ms) > 0 {
		return preserveQuoted(buf, ms), tierFuzzy
	}
	return nil, tierExact
}

// preserveQuoted returns ms with each replacement's quoted context taken from buf
// instead of the model's transcription. Only the run the edit rewrites comes from
// newText, so lookalike characters the model flattened survive outside the change.
func preserveQuoted(buf string, ms []match) []match {
	for i, m := range ms {
		site := buf[m.s:m.e]
		csite, smap := canonical(site)
		crepl, rmap := canonical(m.repl)
		if csite == crepl {
			continue // the edit exists to change what the folding ignores; write it whole
		}
		p, s := commonAffixes(csite, crepl)
		if p == 0 && s == 0 {
			continue // nothing quoted in common, the edit replaces the whole site
		}
		head := smap[p]
		if p == len(csite) { // no byte p to anchor on, stop at the last kept rune
			_, n := utf8.DecodeRuneInString(site[smap[p-1]:])
			head = smap[p-1] + n
		}
		ms[i].repl = restoreQuotedLines(site,
			site[:head]+m.repl[rmap[p]:rmap[len(crepl)-s]]+site[smap[len(csite)-s]:])
	}
	return ms
}

// restoreQuotedLines returns repl with every line the edit leaves canonically
// untouched taken from site, recovering whitespace a reindent flattened. Line
// counts must agree for the lines to pair up, otherwise repl is returned as is.
func restoreQuotedLines(site, repl string) string {
	sl, rl := strings.Split(site, "\n"), strings.Split(repl, "\n")
	if len(sl) != len(rl) {
		return repl
	}
	var swapped bool
	for j := range rl {
		if rl[j] != sl[j] && canonEq(rl[j], sl[j]) {
			rl[j], swapped = sl[j], true
		}
	}
	if !swapped {
		return repl
	}
	return strings.Join(rl, "\n")
}

// exactMatches returns every byte-for-byte occurrence of old in buf.
func exactMatches(buf, old, replacement string) []match {
	var ms []match
	for s := 0; ; {
		j := strings.Index(buf[s:], old)
		if j < 0 {
			return ms
		}
		s += j
		ms = append(ms, match{s: s, e: s + len(old), repl: replacement})
		s += len(old)
	}
}

// canonMatches finds old in buf under canonicalization, mapping each canonical
// hit back to the byte range of the original it covers.
func canonMatches(buf, cbuf string, cmap []int, cold, old, replacement string) []match {
	if cold == "" {
		return nil
	}
	var ms []match
	for c := 0; ; {
		j := strings.Index(cbuf[c:], cold)
		if j < 0 {
			return ms
		}
		c += j
		s, e := cmap[c], cmap[c+len(cold)]
		if canonEq(buf[s:e], old) { // never write a span the map cannot prove
			ms = append(ms, match{s: s, e: e, repl: replacement})
		}
		c += len(cold)
	}
}

// indentMatches finds regions equal to old with every non-blank line's leading
// whitespace swapped for one candidate prefix: the whole-block-shifted-one-level
// case, which is the only indentation difference that can be applied provably.
// Anything less regular (a tab/space conversion of nested lines, mixed depths)
// matches nothing and falls through to the diagnostics, which explain it.
func indentMatches(buf, cbuf string, cmap []int, old, replacement string) []match {
	oldLines := strings.Split(old, "\n")
	base := leadingWS(oldLines[0])
	if !sharedIndent(oldLines, base) || !sharedIndent(strings.Split(replacement, "\n"), base) {
		return nil // the swap would not be total, so it cannot be proven
	}
	cbase := canonIndent(base)
	body := canonLines(reindent(old, base, ""))
	if len(body) == 0 || body[0] == "" {
		return nil // no anchor: a leading blank line matches every indent
	}

	var ms []match
	for c := 0; c <= len(cbuf); {
		cp := leadingWS(cbuf[c:])
		if cp != cbase { // equal prefixes are what the tiers above already tried
			if want := reindentLines(body, cp); strings.HasPrefix(cbuf[c:], want) {
				s, e := cmap[c], cmap[c+len(want)]
				p := leadingWS(buf[s:]) // the file's own prefix, not the canonical one
				if canonEq(buf[s:e], reindent(old, base, p)) {
					ms = append(ms, match{s: s, e: e, repl: reindent(replacement, base, p)})
				}
			}
		}
		j := strings.IndexByte(cbuf[c:], '\n')
		if j < 0 {
			break
		}
		c += j + 1
	}
	return ms
}

// fuzzyMatches returns the single region of buf whose lines differ from old only
// where replacement rewrites them, by at most fuzzyLineLimit characters each. It
// returns none when no region qualifies, when the runner-up is nearly as close, or
// when the region covers a line listed in edited.
func fuzzyMatches(buf, cbuf string, cmap []int, old, replacement string, edited []lineRange) []match {
	cold, _ := canonical(old)
	crep, _ := canonical(replacement)
	oldLines := strings.Split(cold, "\n")
	repLines := strings.Split(crep, "\n")
	base := leadingWS(oldLines[0])
	// a candidate at a different indent is only comparable when the whole block
	// shifts together, exactly as the indent tier requires
	shift := sharedIndent(oldLines, base) && sharedIndent(repLines, base)
	var oldBody, repBody []string // the block dedented, for shifted candidates only
	if shift {
		oldBody, repBody = canonLines(reindent(cold, base, "")), canonLines(reindent(crep, base, ""))
	}

	clines := strings.Split(cbuf, "\n")
	starts := lineStarts(clines)
	n := len(oldLines)

	best, runner, at := fuzzyLineLimit+fuzzyRivalMargin+1, fuzzyLineLimit+fuzzyRivalMargin+1, -1
	for i := 0; i+n <= len(clines); i++ {
		want, wantRep := oldLines, repLines
		if cp := leadingWS(clines[i]); cp != base {
			if !shift {
				continue
			}
			want = strings.Split(reindentLines(oldBody, cp), "\n")
			wantRep = strings.Split(reindentLines(repBody, cp), "\n")
		}
		drift, ok := fuzzyWindow(want, wantRep, clines[i:i+n])
		if !ok {
			continue
		}
		if slices.ContainsFunc(edited, func(r lineRange) bool { return i < r.to && r.from < i+n }) {
			continue // an earlier edit wrote here; healing could revert its work
		}
		if drift < best {
			best, runner, at = drift, best, i
		} else if drift < runner {
			runner = drift
		}
	}
	// the match has to be the clear best: a second region under the limit is a
	// guess outright, and one just past it only separates a match already thin
	if at < 0 || best > fuzzyLineLimit ||
		runner <= fuzzyLineLimit || runner-best <= fuzzyRivalMargin {
		return nil
	}
	window := strings.Join(clines[at:at+n], "\n")
	s, e := cmap[starts[at]], cmap[starts[at]+len(window)]
	return []match{{s: s, e: e, repl: reindent(replacement, base, leadingWS(buf[s:]))}}
}

// fuzzyWindow returns the longest run by which window differs from old, given
// every differing line is one rep rewrites and differs only where rep rewrites it.
// ok is false when no drift is allowed to stand. old, rep and window are lines in
// canonical space at window's indentation.
func fuzzyWindow(old, rep, window []string) (int, bool) {
	if len(old) != len(rep) {
		// only equal counts pair the lines up, and without that pairing there is
		// no way to hold a line's drift to the part rep rewrites
		return 0, false
	}
	lo, hi := rewrittenLines(old, rep)
	if lo >= hi {
		return 0, false // the edit rewrites no line, so there is nothing to heal within
	}
	var worst, lines int
	for j := range old {
		if old[j] == window[j] {
			continue
		}
		if lines++; lines > fuzzyDriftLines {
			return 0, false // too much of the block is guesswork to trust the match
		}
		if j < lo || j >= hi {
			return 0, false // rep leaves this line alone, so applying would revert it
		}
		if trimIndent(old[j]) == trimIndent(window[j]) {
			return 0, false // an irregular indent; the indent tier proves that or refuses it
		}
		p, s := commonAffixes(old[j], window[j])
		ae, be := len(old[j])-s, len(window[j])-s
		if p+s < fuzzyLineLimit {
			return 0, false // too little context to anchor on: "bar" would heal onto "foo"
		}
		rlo, rhi, ok := rewrittenSpan(old[j], rep[j])
		if !ok || p < rlo || ae > rhi {
			return 0, false // the drift is in the part of the line rep leaves alone
		}
		// the span checks above are measured on old, but the apply writes rep over
		// window; punctuation only window holds would be deleted unnoticed
		if t := trailingPunct(window[j]); t != "" && !strings.HasSuffix(rep[j], t) {
			return 0, false
		}
		worst = max(worst, max(ae-p, be-p))
	}
	if lines == 0 || worst > fuzzyLineLimit+fuzzyRivalMargin {
		return 0, false
	}
	return worst, true
}

// rewrittenLines returns the half-open range of old's lines that rep rewrites:
// what is left after trimming the leading and trailing lines the two share.
func rewrittenLines(old, rep []string) (int, int) {
	lo := 0
	for lo < len(old) && lo < len(rep) && old[lo] == rep[lo] {
		lo++
	}
	n := 0
	for n < len(old)-lo && n < len(rep)-lo && old[len(old)-1-n] == rep[len(rep)-1-n] {
		n++
	}
	return lo, len(old) - n
}

// rewrittenSpan returns the range of line that rep rewrites, widened to whole
// identifier and number tokens. ok is false when rep leaves line alone.
func rewrittenSpan(line, rep string) (int, int, bool) {
	p, s := commonAffixes(line, rep)
	lo, hi := p, len(line)-s
	if lo == hi && line == rep {
		return 0, 0, false
	}
	// a value and its replacement often share an edge character (256 and 4096
	// both end in 6), which would split the token and leave the guard comparing
	// halves of a number
	for lo > 0 && isTokenByte(line[lo-1]) {
		lo--
	}
	for hi < len(line) && isTokenByte(line[hi]) {
		hi++
	}
	return lo, hi, true
}

// trimIndent returns s without its leading whitespace.
func trimIndent(s string) string { return s[len(leadingWS(s)):] }

// trailingPunct returns the run of non-token characters ending s, the structure
// a replacement has to carry over. Callers pass canonical lines, which hold no
// trailing whitespace.
func trailingPunct(s string) string {
	i := len(s)
	for i > 0 && !isTokenByte(s[i-1]) {
		i--
	}
	return s[i:]
}

// isTokenByte reports whether c can sit inside an identifier or number.
func isTokenByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// driftRun returns the differing run of a and b, each as its own side holds it.
// ok is false when they differ nowhere.
func driftRun(a, b string) (string, string, bool) {
	p, s := commonAffixes(a, b)
	ae, be := len(a)-s, len(b)-s
	if p == ae && p == be {
		return "", "", false
	}
	// a shared edge digit would otherwise quote back halves of a number
	for p > 0 && (isTokenByte(a[p-1]) || !runeStart(a, p) || !runeStart(b, p)) {
		p--
	}
	for ae < len(a) && (isTokenByte(a[ae]) || !runeStart(a, ae)) {
		ae++
	}
	for be < len(b) && (isTokenByte(b[be]) || !runeStart(b, be)) {
		be++
	}
	return a[p:ae], b[p:be], true
}

// runeStart reports whether i begins a rune in s, or is its end.
func runeStart(s string, i int) bool { return i >= len(s) || utf8.RuneStart(s[i]) }

// maxDriftQuotes bounds how many differing runs one note names; past a few the
// diff says more than another quotation.
const maxDriftQuotes = 3

// driftNote names the runs by which old and matched differ, empty when none do.
func driftNote(old, matched string) string {
	ol, ml := strings.Split(old, "\n"), strings.Split(matched, "\n")
	var parts []string
	for j := 0; j < len(ol) && j < len(ml) && len(parts) < maxDriftQuotes; j++ {
		if wrote, has, ok := driftRun(ol[j], ml[j]); ok {
			parts = append(parts, fmt.Sprintf("%q where you wrote %q", has, wrote))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "the file has " + strings.Join(parts, ", and ")
}

// commonAffixes returns the lengths of a and b's common prefix and common
// suffix, which never overlap.
func commonAffixes(a, b string) (int, int) {
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	return p, s
}

// canonical rewrites s for tolerant matching: unicode punctuation and spacing
// lookalikes become their ASCII substitutes, zero-width characters drop, and
// whitespace runs before a newline or at end of text are removed. The map holds,
// per result byte offset, the offset in s it came from, plus len(s) so the end of
// a match maps back too.
func canonical(s string) (string, []int) {
	var b strings.Builder
	b.Grow(len(s))
	pos := make([]int, 0, len(s)+1)
	var ws strings.Builder // whitespace held back until content proves it interior
	var wsPos []int

	flush := func() {
		if ws.Len() == 0 {
			return
		}
		b.WriteString(ws.String())
		pos = append(pos, wsPos...)
		ws.Reset()
		wsPos = wsPos[:0]
	}
	drop := func() {
		ws.Reset()
		wsPos = wsPos[:0]
	}

	for i, r := range s {
		switch c := canonRune(r); {
		case zeroWidth(r):
		case c == '\n':
			drop() // the run before a newline is trailing
			b.WriteByte('\n')
			pos = append(pos, i)
		case unicode.IsSpace(c):
			start := ws.Len()
			ws.WriteRune(c)
			for k := start; k < ws.Len(); k++ {
				wsPos = append(wsPos, i)
			}
		default:
			flush()
			start := b.Len()
			b.WriteRune(c)
			for k := start; k < b.Len(); k++ {
				pos = append(pos, i) // every byte of a rune maps to the rune's start
			}
		}
	}
	return b.String(), append(pos, len(s)) // a trailing run is dropped with the text
}

// canonEq reports whether a and b are the same text under canonicalization.
func canonEq(a, b string) bool {
	ca, _ := canonical(a)
	cb, _ := canonical(b)
	return ca == cb
}

// canonRune maps a unicode lookalike onto the ASCII character it stands in for.
func canonRune(r rune) rune {
	switch r {
	case '\u2018', '\u2019', '\u201a', '\u201b', '\u2032': // curly and prime single quotes
		return '\''
	case '\u201c', '\u201d', '\u201e', '\u201f', '\u2033': // curly and prime double quotes
		return '"'
	case '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2015', '\u2212': // dashes and minus
		return '-'
	case '\u00a0', '\u1680', '\u202f', '\u205f', '\u3000': // nbsp, ogham, narrow, medial, ideographic
		return ' '
	}
	if r >= '\u2000' && r <= '\u200a' { // en, em, thin and hair spaces
		return ' '
	}
	return r
}

// zeroWidth reports whether r carries no meaning for matching.
func zeroWidth(r rune) bool {
	switch r {
	case '\u200b', '\u200c', '\u200d', '\ufeff': // zero-width space, non-joiner, joiner, BOM
		return true
	}
	return false
}

// canonIndent returns base folded the way canonical folds interior whitespace.
// canonical itself strips a whitespace-only string, leaving every base equal to "".
func canonIndent(base string) string {
	var b strings.Builder
	for _, r := range base {
		if !zeroWidth(r) {
			b.WriteRune(canonRune(r))
		}
	}
	return b.String()
}

// sharedIndent reports whether every non-blank line starts with base.
func sharedIndent(lines []string, base string) bool {
	for _, l := range lines {
		if nonBlank(l) && !strings.HasPrefix(l, base) {
			return false
		}
	}
	return true
}

// reindent swaps base for prefix on every non-blank line of s; blank lines
// stay empty and lines lacking base keep their own indentation.
func reindent(s, base, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		switch {
		case !nonBlank(l):
			lines[i] = ""
		case strings.HasPrefix(l, base):
			lines[i] = prefix + l[len(base):]
		}
	}
	return strings.Join(lines, "\n")
}

// canonLines splits s into canonical-space lines, for reassembly under a
// candidate indent prefix.
func canonLines(s string) []string {
	c, _ := canonical(s)
	return strings.Split(c, "\n")
}

// reindentLines joins body under prefix, leaving blank lines empty.
func reindentLines(body []string, prefix string) string {
	out := make([]string, len(body))
	for i, l := range body {
		if l == "" {
			out[i] = ""
		} else {
			out[i] = prefix + l
		}
	}
	return strings.Join(out, "\n")
}

// tierNote explains in one line why an applied edit was not byte-exact, so the
// model writes the next one correctly instead of repeating the difference.
func tierNote(idx int, tier matchTier, old, matched string) string {
	switch tier {
	case tierFuzzy:
		if d := driftNote(old, matched); d != "" {
			return fmt.Sprintf("edit %d: applied after correcting your oldText; %s, so confirm the diff is what you intended",
				idx, d)
		}
		return fmt.Sprintf("edit %d: applied after correcting your oldText to the file's text; confirm the diff is what you intended", idx)
	case tierIndent:
		have := describeIndent(leadingWS(strutil.FirstLine(matched)))
		want := describeIndent(leadingWS(strutil.FirstLine(old)))
		return fmt.Sprintf("edit %d: applied after shifting indentation; the file indents this block with %s where your text used %s, and the file's indentation was kept",
			idx, have, want)
	case tierCanon:
		// a lookalike survives a trailing trim; stripSpace would fold it away
		if stripTrailingWS(old) == stripTrailingWS(matched) {
			return fmt.Sprintf("edit %d: applied after ignoring trailing whitespace; your oldText did not match the file byte for byte",
				idx)
		}
		if d := driftNote(old, matched); d != "" {
			return fmt.Sprintf("edit %d: applied after normalizing unicode characters; %s", idx, d)
		}
		return fmt.Sprintf("edit %d: applied after normalizing unicode punctuation; the file uses different quote, dash or space characters than your oldText",
			idx)
	}
	return ""
}

// describeIndent renders an indentation prefix, naming an absent one.
func describeIndent(ws string) string {
	if ws == "" {
		return "no indentation"
	}
	return describeRun(ws)
}
