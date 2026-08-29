package tui

import (
	"strings"

	"github.com/go-analyze/bulk"
)

// SearchItem is one candidate line for the Ctrl+R history search.
type SearchItem struct {
	Text   string // full text placed in the editor on accept
	Detail string // dim trailing context, e.g. when it was sent
}

// searchAction is what the UI does after the overlay saw a key.
type searchAction uint8

const (
	searchStay   searchAction = iota // consumed, overlay stays open
	searchClose                      // close, buffer untouched
	searchAccept                     // close and fill the editor with current()
	searchPass                       // close, editor handles the key
)

// searchOverlay is an incremental reverse history search over recorded prompts.
type searchOverlay struct {
	query   string
	items   []SearchItem
	matches []SearchItem // substring hits, newest first
	cursor  int          // index into matches
	pending bool         // provider still running
}

// refilter recomputes the match set on a case-insensitive substring. An empty
// query matches nothing: Ctrl+R stays blank until the user types content to match.
func (s *searchOverlay) refilter() {
	q := strings.ToLower(s.query)
	s.matches = nil
	if q != "" {
		s.matches = bulk.SliceFilter(func(it SearchItem) bool {
			return strings.Contains(strings.ToLower(it.Text), q)
		}, s.items)
	}
	s.cursor = 0
}

// key applies one overlay key and reports what the UI should do with it.
func (s *searchOverlay) key(k key) searchAction {
	switch k.typ {
	case keyRune, keyPaste:
		s.query += strings.ReplaceAll(k.text, "\n", "")
		s.refilter()
		return searchStay
	case keyBackspace:
		if s.query != "" {
			s.query = trimLastCluster(s.query)
			s.refilter()
		}
		return searchStay
	case keyKillLine:
		s.query = ""
		s.refilter()
		return searchStay
	case keyReverseSearch, keyUp:
		s.cursor = wrapIndex(s.cursor+1, len(s.matches)) // older match
		return searchStay
	case keyDown:
		s.cursor = wrapIndex(s.cursor-1, len(s.matches)) // newer match
		return searchStay
	// Enter and the first Escape both select the highlighted match; a second
	// Escape (now that the overlay is closed) clears via the editor's own handler.
	case keyEnter, keyEscape:
		if _, ok := s.current(); ok {
			return searchAccept
		}
		return searchClose
	case keyInterrupt:
		return searchClose
	default:
		return searchPass
	}
}

// current returns the highlighted candidate.
// matchSpans returns byte ranges of every case-insensitive occurrence of q in
// text. An empty query yields none.
func matchSpans(text, q string) [][2]int {
	lq := strings.ToLower(q)
	if lq == "" {
		return nil
	}
	lower := strings.ToLower(text)
	if len(lower) != len(text) {
		return nil // lowering moved the byte offsets; a shifted highlight is worse than none
	}
	var spans [][2]int
	for i := 0; ; {
		offset := strings.Index(lower[i:], lq)
		if offset < 0 {
			break
		}
		start, end := i+offset, i+offset+len(lq)
		spans = append(spans, [2]int{start, end})
		i = end // non-overlapping occurrences
	}
	return spans
}

func (s *searchOverlay) current() (SearchItem, bool) {
	if len(s.matches) == 0 || s.cursor >= len(s.matches) {
		return SearchItem{}, false
	}
	return s.matches[s.cursor], true
}

// rows renders the overlay above the editor: a query-echo header followed by
// every line of the current match with matched spans emphasized. Lines beyond
// maxRows collapse to an overflow marker.
func (s *searchOverlay) rows(t Theme, width, maxRows int) []string {
	header := t.Accent.Wrap("(reverse-i-search)`" + s.query + "':")
	out := make([]string, 0, max(2, min(maxRows+1, len(s.matches)+1)))

	switch {
	case s.pending:
		return append(out, header, t.Dim.Wrap(selectIndent+"searching…"))
	case s.query == "":
		// nothing typed yet: keep the field blank rather than implying a result
		out = append(out, header)
		return out
	case len(s.matches) == 0:
		return append(out, header, t.Dim.Wrap(selectIndent+"no match"))
	}

	it := s.matches[s.cursor]
	if it.Detail != "" {
		header += t.Dim.Wrap("  " + it.Detail)
	}
	out = append(out, header)

	lines := strings.Split(it.Text, "\n")
	for i, ln := range lines {
		if maxRows <= 0 {
			out = append(out, t.Dim.Wrap(selectIndent+moreLabel(len(lines)-i)))
			break
		}
		maxRows--
		out = append(out, truncateDisplay(" "+highlightLine(t, ln, s.query), width))
	}
	return out
}

// highlightLine emphasizes every query occurrence in one text line; a no-op on plain.
func highlightLine(t Theme, text, q string) string {
	if q == "" {
		return text
	}
	return applySpans("", text, matchSpans(text, q), Style{}, t.Accent)
}
