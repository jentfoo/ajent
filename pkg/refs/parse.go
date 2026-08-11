// Package refs parses @file references in a message and expands them into real
// tool-call + result pairs ahead of the user's text: a small file is injected
// via read, a directory via ls, and a large or non-text file is annotated in
// place with its shape so the model can choose whether to read it.
package refs

import "strings"

// Ref is one @path token in a message. Note carries any existing
// `(800 lines, 64kb)` measurement absorbed into the token, which is what makes
// re-expansion idempotent: expansion recognises it and replaces it rather than
// appending a second measurement.
type Ref struct {
	Path  string // the path as written, without @ or annotation
	Start int    // byte offset of the leading @ in the message
	End   int    // byte offset just past the token (path or annotation)
	Note  string // any absorbed trailing (…), empty when none
}

// Parse returns every @path reference in text. @ matches only at a word
// boundary (start of line, or after whitespace / `(`, `[`, a quote) so
// `email@example.com` is prose. The path token stops at whitespace or trailing
// punctuation, and a trailing `(...)` measurement is absorbed into Note so
// re-expansion replaces it.
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
		for j < len(text) && !isSpaceOrPunct(text[j]) {
			j++
		}
		path := text[pathStart:j]
		note := ""
		// absorb a trailing (…) measurement, allowing a single space before it
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
	case ' ', '\t', '\n', '(', '[', '`', '"', '\'', '{':
		return true
	}
	return false
}

// isSpaceOrPunct reports whether c ends a path token.
func isSpaceOrPunct(c byte) bool {
	switch c {
	case ' ', '\t', '\n', ',', ';', ')', ']', '}', '`', '"', '\'', '!', '?':
		return true
	}
	return false
}

// absorbNote returns the end index of a trailing `(…)` annotation immediately
// after pos, when one is present. It matches balanced parens so a measurement
// containing a parenthesised unit still terminates. ok is false when there is
// no annotation or it does not look like a measurement.
func absorbNote(text string, pos int) (end int, ok bool) {
	if pos >= len(text) || text[pos] != '(' {
		return pos, false
	}
	depth := 0
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
		case '\n':
			return pos, false // a newline inside the parens is not a measurement
		}
	}
	return pos, false
}

// looksLikeNote reports whether inner matches the measurement shape
// `(N lines, SIZE)` / `(binary, SIZE)` / `(image, SIZE)` / `(dir)` / `(SIZE)`
// closely enough to be treated as an existing annotation. It is strict about
// the allowed words so ordinary parenthetical prose after a path is never eaten.
func looksLikeNote(inner string) bool {
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return false
	}
	// a measurement contains a digit (a size) or one of the kind words
	if !strings.ContainsAny(inner, "0123456789") &&
		!strings.Contains(inner, "binary") &&
		!strings.Contains(inner, "image") &&
		!strings.Contains(inner, "dir") &&
		!strings.Contains(inner, "lines") {
		return false
	}
	// allowed tokens: digits, the units b/kb/mb, the words, commas, dots, spaces
	for _, tok := range strings.FieldsFunc(inner, func(r rune) bool {
		return r == ' ' || r == ',' || r == '.'
	}) {
		if isSizeToken(tok) || tok == "lines" || tok == "binary" || tok == "image" ||
			tok == "dir" || tok == "b" || tok == "kb" || tok == "mb" {
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
