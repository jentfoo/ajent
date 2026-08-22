package tui

import (
	"fmt"
	"strings"
)

// Completion is one candidate for the live completion overlay. Text is what
// accepting the candidate inserts; Label is what the overlay shows; Detail is
// optional dim trailing text; Score ranks the list (higher first).
type Completion struct {
	Text   string
	Label  string
	Detail string
	Score  int
}

// Completer supplies candidates for the live completion overlay. Complete is
// called under the UI lock from the key goroutine, so it must not block or call
// back into the UI; a path source keeps a cached index for exactly this reason.
// start is the grapheme index the accepted Text replaces, up to the cursor.
type Completer interface {
	Complete(text string, pos int) (start int, items []Completion)
}

// MatchScore rates how well query matches text as a subsequence, returning
// false when it does not match at all. It is the exported form of the filter the
// pickers use, so a path completion source ranks identically rather than with a
// second implementation.
func MatchScore(text, query string) (int, bool) { return matchScore(text, query) }

// SetCompleter installs the live completion overlay's source. A nil source
// disables the overlay. The overlay is only active in inline and alt modes;
// plain mode has no live block and ignores it.
func (u *UI) SetCompleter(c Completer) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.completer = c
	u.completion = nil // clear any open list when the source changes
	u.repaint()
}

// queryCompleter asks the completer for candidates at the cursor and opens the
// overlay when there are any, or closes it when there are none. Caller holds
// the lock.
func (u *UI) queryCompleter() {
	if u.completer == nil {
		u.completion = nil
		return
	}
	text := u.editor.Value()
	pos := u.editor.pos
	start, items := u.completer.Complete(text, pos)
	if len(items) == 0 {
		u.completion = nil
		return
	}
	if u.completion == nil {
		u.completion = &completionOverlay{}
	}
	u.completion.open(items, start)
}

// pasteThreshold is the byte size above which a paste becomes a placeholder.
const pasteThreshold = 2048

// pastePlaceholder returns the marker text inserted for a large paste.
func pastePlaceholder(text string) string {
	lines := strings.Count(text, "\n") + 1
	return fmt.Sprintf("[pasted %d lines]", lines)
}

// expandPastes replaces any paste placeholders in v with their stored content so
// the model receives the whole text while the input block stayed small.
func (u *UI) expandPastes(v string) string {
	if len(u.pastes) == 0 {
		return v
	}
	for ph, content := range u.pastes {
		v = strings.ReplaceAll(v, ph, content)
	}
	return v
}
