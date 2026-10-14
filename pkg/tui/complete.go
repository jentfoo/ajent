package tui

import (
	"fmt"
	"strings"
	"time"
)

// Completion is one Tab completion candidate.
type Completion struct {
	Text   string // inserted when the candidate is accepted
	Label  string // shown in an ambiguous listing
	Detail string // optional dim trailing text
	Score  int    // ranks the listing, higher first
}

// Completer supplies candidates for Tab completion: given the buffer and
// cursor, the cell an accepted Text replaces from plus the candidates. Complete
// must not call back into the UI.
type Completer interface {
	Complete(text string, pos int) (start int, items []Completion)
}

// CompleteStyle is how a context's candidates are presented. Menu re-queries on
// every keystroke under the UI lock, so a menu context's Complete must return
// promptly and must not re-enter the UI. Async is the escape hatch for one that
// cannot.
type CompleteStyle struct {
	Menu  bool // live list while typing, with ↑/↓ selecting and Tab accepting
	Async bool // query off the key loop, for a source that may block
}

// StyledCompleter reports how the cursor's context completes. A plain Completer
// gets the zero style: Tab-driven and synchronous.
type StyledCompleter interface {
	Completer
	Style(text string, pos int) CompleteStyle
}

// MatchScore rates how well query matches text as a subsequence, returning
// false when it does not match at all. It is the exported form of the filter the
// pickers use, so a path completion source ranks identically rather than with a
// second implementation.
func MatchScore(text, query string) (int, bool) { return matchScore(text, query) }

// SetCompleter installs the Tab completion source. Nil disables completion.
// Only inline and alt modes complete, since plain mode has no live block.
func (u *UI) SetCompleter(c Completer) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.completer = c
	u.completionSeq++  // a query in flight for the old source must not land
	u.completion = nil // drop any listing when the source changes
	u.repaint()
}

// style reports how the cursor's context presents. Caller holds the lock.
func (u *UI) style(text string, pos int) CompleteStyle {
	if sc, ok := u.completer.(StyledCompleter); ok {
		return sc.Style(text, pos)
	}
	return CompleteStyle{}
}

// refreshMenu re-queries after an edit, keeping a live menu open for the
// contexts that use one. A Tab-driven context drops any listing instead of
// opening on its own. Caller holds the lock, so a menu query runs on the key
// loop (see CompleteStyle).
func (u *UI) refreshMenu() {
	if u.completer == nil {
		u.completion = nil
		return
	}
	text, pos := u.editor.Value(), u.editor.pos
	if !u.style(text, pos).Menu {
		u.completion = nil
		return
	}
	u.completionSeq++ // supersede any in-flight Tab query
	start, items := u.completer.Complete(text, pos)
	if len(items) == 0 {
		u.completion = nil
		return
	}
	if u.completion == nil || !u.completion.menu {
		u.completion = &completionOverlay{menu: true}
	}
	u.completion.open(items, start)
}

// startCompletion answers a Tab, applying the candidates at the cursor. Caller
// holds the lock.
func (u *UI) startCompletion() {
	if u.completer == nil {
		return
	}
	text, pos := u.editor.Value(), u.editor.pos
	st := u.style(text, pos)
	if st.Menu {
		u.refreshMenu() // an Esc-dismissed menu comes back
		return
	}
	u.completionSeq++
	seq := u.completionSeq
	if st.Async {
		// safeGo: a source may shell out, and a panic must restore the terminal
		c := u.completer
		u.safeGo(func() {
			start, items := c.Complete(text, pos)
			u.deliverCompletion(seq, start, items, text, pos)
		})
		return
	}
	u.applyCompletion(u.completer.Complete(text, pos))
}

// deliverCompletion applies an off-lock query's result unless a newer Tab,
// more typing or an owning overlay (search, interaction) superseded it.
func (u *UI) deliverCompletion(seq, start int, items []Completion, text string, pos int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || seq != u.completionSeq ||
		u.editor.Value() != text || u.editor.pos != pos ||
		u.search != nil || u.act != nil {
		return // stale: the buffer moved on or an overlay owns the keys now
	}
	u.applyCompletion(start, items)
	u.repaint()
}

// applyCompletion inserts as much as the candidates agree on at the prompt
// editor's cursor, or lists them when they agree on nothing more. Caller holds
// the lock.
func (u *UI) applyCompletion(start int, items []Completion) {
	if len(items) == 0 {
		u.completion = nil
		u.flashRule()
		return
	}
	o, changed := completeAt(&u.editor, start, items, u.completion != nil)
	switch {
	case o != nil:
		u.completion = o // ambiguous: show what is left
	case changed:
		u.completion = nil
	default:
		u.flashRule() // nothing to add and nothing new to show
	}
}

// completeAt is the Tab core every completion surface shares: it inserts as much
// of items as they agree on at ed's cursor, returning a listing to open when
// they agree on nothing more (unless one already is) and reporting whether the
// buffer changed.
func completeAt(ed *editor, start int, items []Completion, listOpen bool) (*completionOverlay, bool) {
	typed := ed.SpanText(start)
	prefix := commonPrefix(items)
	switch {
	case !strings.HasPrefix(prefix, typed):
		// candidates that do not extend the typed text have no useful prefix
		ed.ReplaceSpan(start, items[0].Text)
		return nil, true
	case prefix != typed:
		ed.ReplaceSpan(start, prefix)
		return nil, true
	case len(items) > 1 && !listOpen:
		return &completionOverlay{items: items, start: start}, false
	}
	return nil, false
}

// ruleFlashDuration is how long the prompt rule stays accented.
const ruleFlashDuration = 120 * time.Millisecond

// flashRule accents the rule above the prompt briefly, acknowledging a keypress
// that changed nothing. Caller holds the lock and repaints.
func (u *UI) flashRule() {
	if u.ruleFlash || u.closed {
		return
	}
	u.ruleFlash = true
	u.afterSafe(ruleFlashDuration, func() {
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

// pasteEntry is one stored large paste, kept for the whole session so a
// recalled prompt still expands.
type pasteEntry struct {
	placeholder string
	content     string
}

// pastePlaceholder returns the marker text inserted for a large paste. seq makes
// it unique, so two pastes of the same line count never share an entry.
func pastePlaceholder(text string, seq int) string {
	lines := strings.Count(text, "\n") + 1
	return fmt.Sprintf("[pasted %d lines #%d]", lines, seq)
}

// expandPastes replaces any paste placeholders in v with their stored content so
// the model receives the whole text while the input block stayed small.
func (u *UI) expandPastes(v string) string {
	for _, p := range u.pastes { // oldest first, so nesting is deterministic
		v = strings.ReplaceAll(v, p.placeholder, p.content)
	}
	return v
}
