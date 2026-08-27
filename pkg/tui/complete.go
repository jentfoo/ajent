package tui

import (
	"fmt"
	"strings"
	"time"
)

// Completion is one Tab completion candidate. Text is what accepting the
// candidate inserts; Label is what an ambiguous listing shows; Detail is
// optional dim trailing text; Score ranks the list (higher first).
type Completion struct {
	Text   string
	Label  string
	Detail string
	Score  int
}

// Completer supplies candidates for Tab completion. Complete is called with the
// buffer and cursor, returning where accepted text should start replacing from
// plus the candidates. It must not call back into the UI.
type Completer interface {
	Complete(text string, pos int) (start int, items []Completion)
}

// AsyncCompleter marks a completer whose queries may take time (a slow directory
// listing, a subprocess). The UI runs those off the key loop so a Tab never
// stalls typing; IsAsync reports whether the token under the cursor is such a
// query, so the rest keeps its synchronous contract.
type AsyncCompleter interface {
	Completer
	IsAsync(text string, pos int) bool
}

// MatchScore rates how well query matches text as a subsequence, returning
// false when it does not match at all. It is the exported form of the filter the
// pickers use, so a path completion source ranks identically rather than with a
// second implementation.
func MatchScore(text, query string) (int, bool) { return matchScore(text, query) }

// SetCompleter installs the Tab completion source. A nil source disables
// completion. Completion is only active in inline and alt modes; plain mode has
// no live block and ignores it.
func (u *UI) SetCompleter(c Completer) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.completer = c
	u.completion = nil // drop any listing when the source changes
	u.repaint()
}

// startCompletion answers a Tab: it asks the completer for candidates at the
// cursor and applies them. A source that may block runs off the key loop, so a
// slow listing never stalls the editor. Caller holds the lock.
func (u *UI) startCompletion() {
	if u.completer == nil {
		return
	}
	text, pos := u.editor.Value(), u.editor.pos
	u.completionSeq++
	seq := u.completionSeq
	if ac, ok := u.completer.(AsyncCompleter); ok && ac.IsAsync(text, pos) {
		// safeGo, not a bare goroutine: a source may shell out, and a panic there
		// must still restore the terminal
		u.safeGo(func() {
			start, items := ac.Complete(text, pos)
			u.deliverCompletion(seq, start, items, text, pos)
		})
		return
	}
	u.applyCompletion(u.completer.Complete(text, pos))
}

// deliverCompletion applies an off-lock query's result unless a newer Tab
// superseded it.
func (u *UI) deliverCompletion(seq, start int, items []Completion, text string, pos int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || seq != u.completionSeq || u.editor.Value() != text || u.editor.pos != pos {
		return // stale: a newer Tab or more typing moved on
	}
	u.applyCompletion(start, items)
	u.repaint()
}

// applyCompletion inserts as much as the candidates unambiguously agree on, or
// lists them when they agree on nothing more. Caller holds the lock.
func (u *UI) applyCompletion(start int, items []Completion) {
	if len(items) == 0 {
		u.completion = nil
		u.flashRule()
		return
	}
	typed := u.spanText(start)
	prefix := commonPrefix(items)
	switch {
	case !strings.HasPrefix(prefix, typed):
		// a source free to return anything (a command's own Complete) has no
		// meaningful common prefix; take its best candidate whole
		u.replaceSpan(start, items[0].Text)
	case prefix != typed:
		u.replaceSpan(start, prefix)
	case len(items) > 1 && u.completion == nil:
		u.completion = &completionList{items: items} // ambiguous: show what is left
		return
	default:
		u.flashRule() // nothing to add and nothing new to show
		return
	}
	u.completion = nil
}

// spanText returns the buffer from start up to the cursor.
func (u *UI) spanText(start int) string {
	start, end := max(start, 0), u.editor.pos
	if start >= end {
		return ""
	}
	return strings.Join(u.editor.cells[start:end], "")
}

// replaceSpan swaps the buffer from start to the cursor for text, leaving the
// cursor at its end.
func (u *UI) replaceSpan(start int, text string) {
	end := u.editor.pos
	start = min(max(start, 0), end)
	replace := graphemesOf(text)
	cells := u.editor.cells
	out := make([]string, 0, len(cells)-(end-start)+len(replace))
	out = append(out, cells[:start]...)
	out = append(out, replace...)
	out = append(out, cells[end:]...)
	u.editor.cells = out
	u.editor.pos = start + len(replace)
}

// ruleFlashDuration is how long the prompt rule stays accented after a Tab that
// could not complete anything.
const ruleFlashDuration = 120 * time.Millisecond

// flashRule accents the rule above the prompt briefly, acknowledging a keypress
// that changed nothing. Caller holds the lock and repaints; only the clearing
// timer redraws on its own.
func (u *UI) flashRule() {
	if u.ruleFlash || u.closed {
		return
	}
	fn := u.afterDelay
	if fn == nil {
		fn = time.AfterFunc // UI built without New (tests) still gets a real timer
	}
	u.ruleFlash = true
	fn(ruleFlashDuration, func() {
		u.mu.Lock()
		defer u.mu.Unlock()
		u.ruleFlash = false
		if !u.closed {
			u.repaint()
		}
	})
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
