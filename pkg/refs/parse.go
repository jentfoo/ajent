// Package refs parses @file references in a message and expands them into real
// tool-call + result pairs behind the user's text: a small file is injected
// via read, a directory via ls, and a large or non-text file is annotated in
// place with its shape so the model can choose whether to read it. Behind, so
// that rewinding onto the message drops the reads it asked for along with it.
package refs

import (
	"strings"
)

// Ref is one @path token in a message. Path is the path the token names:
// escapes removed, without @ or annotation. Note carries any existing
// `(800 lines, 64kb)` measurement absorbed into the token, which is what makes
// re-expansion idempotent: expansion recognises it and replaces it rather than
// appending a second measurement.
type Ref struct {
	Path  string // the named path: escapes removed, no @ or annotation
	Start int    // byte offset of the leading @ in the message
	End   int    // byte offset just past the token (path or annotation)
	Note  string // any absorbed trailing (...), empty when none
}

// Parse returns every @path reference in text. @ matches only at a word
// boundary (start of line, or after whitespace, an opener, a backtick or a
// quote) so `email@example.com` is prose. The path token stops at whitespace or
// trailing punctuation, except that a `]` ends the token only when no `[` before it
// is open, keeping bracketed names like weird[1] whole, and a `\` escapes the
// next byte so a name like `my\ notes.txt` stays one token. A trailing `(...)`
// measurement is absorbed into Note so re-expansion replaces it.
func Parse(text string) []Ref {
	var out []Ref
	for i := 0; i < len(text); i++ {
		if text[i] != '@' {
			continue
		}
		if !atBoundary(text, i) {
			continue // @ mid-word is prose (email@example.com)
		}
		// require a path-like char after @: not whitespace, not end
		if i+1 >= len(text) || isSpaceOrPunct(text[i+1]) {
			continue
		}
		start := i
		pathStart := i + 1
		j := pathStart
		// a ']' stays inside the token while an earlier '[' is open, so bracketed
		// names survive; with none open it still ends one ([see @f.go])
		var depth int // unmatched '[' seen so far in the token
		for j < len(text) {
			c := text[j]
			if c == '\\' && j+1 < len(text) {
				j += 2 // an escape pair rides inside the token, never ending it
				continue
			}
			if c == '[' {
				depth++
			} else if c == ']' && depth > 0 {
				depth--
			} else if isSpaceOrPunct(c) {
				break
			}
			j++
		}
		path := unescapePath(text[pathStart:j])
		var note string
		// absorb a trailing (...) measurement, allowing a single space before it
		noteStart := j
		if noteStart < len(text) && text[noteStart] == ' ' && noteStart+1 < len(text) && text[noteStart+1] == '(' {
			noteStart++ // the leading space is part of the annotation span
		}
		if n, ok := absorbNote(text, noteStart); ok {
			note = text[j:n]
			j = n
		}
		if path == "" {
			continue
		}
		out = append(out, Ref{Path: path, Start: start, End: j, Note: note})
		i = j - 1
	}
	return out
}

// atBoundary reports whether the @ at pos is at a word boundary: start of line
// or preceded by whitespace, `(`, `[`, a backtick or a quote.
func atBoundary(text string, pos int) bool {
	if pos == 0 {
		return true
	}
	prev := text[pos-1]
	switch prev {
	case ' ', '\t', '\n', '\r', '(', '[', '`', '"', '\'', '{':
		return true
	}
	return false
}

// isSpaceOrPunct reports whether c ends a path token. CR counts so an interior
// CRLF line ending never lands inside a parsed path.
func isSpaceOrPunct(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', ',', ';', ')', ']', '}', '`', '"', '\'', '!', '?':
		return true
	}
	return false
}

// escapePath returns path with a `\` before every byte that would end or
// corrupt a bare token, so Parse re-expands the result to path alone. A `]`
// with an open `[` before it stays bare, matching Parse's bracket rule. Plain
// names return unchanged.
func escapePath(path string) string {
	// every isSpaceOrPunct terminator plus the backslash itself
	const escapable = " \t\n\r,;)]}`\"'!?\\"
	if !strings.ContainsAny(path, escapable) {
		return path
	}
	var b strings.Builder
	b.Grow(2 * len(path))
	var depth int // unmatched '[' seen so far, as Parse counts them
	for i := 0; i < len(path); i++ {
		c := path[i]
		switch {
		case c == '[':
			depth++
		case c == ']' && depth > 0:
			depth--
		case c == '\\' || isSpaceOrPunct(c):
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	return b.String()
}

// unescapePath removes the escapes escapePath adds: `\x` becomes `x`, so an
// escaped token reads as the path it names. A lone trailing backslash stays
// literal, and text without one returns unchanged.
func unescapePath(path string) string {
	if !strings.ContainsRune(path, '\\') {
		return path
	}
	var b strings.Builder
	b.Grow(len(path))
	for i := 0; i < len(path); i++ {
		if path[i] == '\\' && i+1 < len(path) {
			i++ // an escape pair lands as the escaped byte alone
		}
		b.WriteByte(path[i])
	}
	return b.String()
}

// absorbNote returns the end index of a trailing `(...)` annotation immediately
// after pos, when one is present. It matches balanced parens so a measurement
// containing a parenthesised unit still terminates. ok is false when there is
// no annotation or it does not look like a measurement.
func absorbNote(text string, pos int) (end int, ok bool) {
	if pos >= len(text) || text[pos] != '(' {
		return pos, false
	}
	var depth int
	for j := pos; j < len(text); j++ {
		switch text[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				inner := text[pos+1 : j]
				if looksLikeNote(inner) {
					return j + 1, true
				}
				return pos, false
			}
		case '\n', '\r':
			return pos, false // a line break inside the parens is not a measurement
		}
	}
	return pos, false
}

// looksLikeNote reports whether inner matches a measurement shape like
// (N lines, SIZE), (binary, SIZE), (special) or (dir). Strict enough that ordinary
// parenthetical prose after a path is never treated as an annotation.
func looksLikeNote(inner string) bool {
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return false
	}
	// a measurement contains a digit (a size) or one of the kind words
	if !strings.ContainsAny(inner, "0123456789") &&
		!strings.Contains(inner, "binary") &&
		!strings.Contains(inner, "image") &&
		!strings.Contains(inner, "special") &&
		!strings.Contains(inner, "dir") &&
		!strings.Contains(inner, "lines") {
		return false
	}
	// allowed tokens: digits, the units b/kb/mb, the words, commas, dots, spaces
	for _, tok := range strings.FieldsFunc(inner, func(r rune) bool {
		return r == ' ' || r == ',' || r == '.'
	}) {
		if isSizeToken(tok) || tok == "lines" || tok == "binary" || tok == "image" ||
			tok == "special" || tok == "dir" || tok == "b" || tok == "kb" || tok == "mb" {
			continue
		}
		return false
	}
	return true
}

// isSizeToken reports whether tok is a number optionally suffixed by b/kb/mb.
func isSizeToken(tok string) bool {
	for _, suf := range []string{"", "b", "kb", "mb"} {
		if strings.HasSuffix(tok, suf) && isDigitToken(strings.TrimSuffix(tok, suf)) {
			return true
		}
	}
	return false
}

// isDigitToken reports whether tok is a run of digits.
func isDigitToken(tok string) bool {
	for _, r := range tok {
		if r < '0' || r > '9' {
			return false
		}
	}
	return tok != ""
}
