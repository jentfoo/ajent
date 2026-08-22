package tui

// completionOverlay is the live completion list rendered above the editor. It
// is distinct from u.act (an interaction): the editor keeps focus and keeps
// receiving characters, and only keyTab/keyUp/keyDown/keyEnter/keyEscape are
// consumed; everything else falls through to the editor, after which the
// completer is re-queried.
type completionOverlay struct {
	items  []Completion
	start  int  // grapheme index the accepted Text replaces, up to the cursor
	cursor int  // highlighted index into items
	moved  bool // selection navigated via ↑/↓; Enter accepts it and submits
}

// open sets the candidates, keeping the cursor in range and dropping a stale
// selection when the start index changes (a fresh query).
func (o *completionOverlay) open(items []Completion, start int) {
	if o.start != start {
		o.moved = false
		o.cursor = 0
	}
	o.items = items
	o.start = start
	if o.cursor >= len(items) {
		o.cursor = 0
	}
}

// accept reports whether the overlay should consume a key rather than the editor.
func (o *completionOverlay) accept(k key) bool {
	switch k.typ {
	case keyTab, keyUp, keyDown, keyEnter, keyEscape:
		return true
	}
	return false
}

// key applies one overlay key, returning whether the editor should also
// receive it and whether the line should submit now.
func (o *completionOverlay) key(k key, u *UI) (consume, submit bool) {
	switch k.typ {
	case keyUp:
		if len(o.items) > 0 {
			o.cursor = wrapIndex(o.cursor-1, len(o.items))
			o.moved = true
		}
		return true, false
	case keyDown:
		if len(o.items) > 0 {
			o.cursor = wrapIndex(o.cursor+1, len(o.items))
			o.moved = true
		}
		return true, false
	case keyTab:
		// accept now; a following Enter just submits
		if len(o.items) > 0 {
			o.applyCurrent(u)
			o.moved = false
		}
		return true, false
	case keyEnter:
		// accept and submit when ↑/↓ moved the selection; otherwise submit as typed
		if o.moved && len(o.items) > 0 {
			o.applyCurrent(u)
			return true, true
		}
		return false, true
	case keyEscape:
		return true, false // close, do not submit
	}
	return false, false
}

// applyCurrent replaces the text from start to the cursor with the selected
// candidate's Text.
func (o *completionOverlay) applyCurrent(u *UI) {
	if len(o.items) == 0 {
		return
	}
	c := o.items[o.cursor]
	end := u.editor.pos
	start := o.start
	if start > end {
		start = end
	}
	replace := graphemesOf(c.Text)
	cells := u.editor.cells
	out := make([]string, 0, len(cells)-(end-start)+len(replace))
	out = append(out, cells[:start]...)
	out = append(out, replace...)
	out = append(out, cells[end:]...)
	u.editor.cells = out
	u.editor.pos = start + len(replace)
}

// rows renders the overlay, capped at maxRows.
func (o *completionOverlay) rows(t Theme, width, maxRows int) []string {
	if len(o.items) == 0 {
		return nil
	}
	n := len(o.items)
	if n > maxRows {
		n = maxRows
	}
	rows := make([]string, 0, n)
	for i := 0; i < n; i++ {
		c := o.items[i]
		marker, style := selectIndent, t.Dim
		if i == o.cursor {
			marker, style = selectMarker, t.Accent
		}
		line := style.Wrap(marker + c.Label)
		if c.Detail != "" {
			line += t.Dim.Wrap("  " + c.Detail)
		}
		rows = append(rows, truncateDisplay(line, width))
	}
	if len(o.items) > n {
		rows = append(rows, t.Dim.Wrap(selectIndent+moreLabel(len(o.items)-n)))
	}
	return rows
}
