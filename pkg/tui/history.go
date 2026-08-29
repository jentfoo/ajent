package tui

import "strings"

// lineBuffer releases only whole lines so a write is never split mid line or
// mid escape sequence, which keeps the terminal's own wrapping intact.
type lineBuffer struct {
	pending strings.Builder
}

// Add appends s and returns any complete lines, empty when none are ready.
func (b *lineBuffer) Add(s string) string {
	// pending never holds a newline, so only the delta needs scanning
	cut := strings.LastIndexByte(s, '\n')
	if cut < 0 {
		b.pending.WriteString(s)
		return ""
	}
	whole := b.pending.String() + s[:cut+1]
	b.pending.Reset()
	b.pending.WriteString(s[cut+1:])
	return whole
}

// Flush returns the buffered remainder as a terminated line and empties the buffer.
func (b *lineBuffer) Flush() string {
	rest := b.pending.String()
	b.pending.Reset()
	if rest == "" {
		return ""
	}
	return rest + "\n"
}

// Pending returns the buffered partial line without consuming it.
func (b *lineBuffer) Pending() string {
	return b.pending.String()
}
