package tui

import "strings"

// lineBuffer releases only whole lines so a write is never split mid line or
// mid escape sequence, which keeps the terminal's own wrapping intact.
type lineBuffer struct {
	pending strings.Builder
}

// Add appends s and returns any complete lines, empty when none are ready.
func (b *lineBuffer) Add(s string) string {
	b.pending.WriteString(s)
	buf := b.pending.String()
	cut := strings.LastIndexByte(buf, '\n')
	if cut < 0 {
		return ""
	}
	b.pending.Reset()
	b.pending.WriteString(buf[cut+1:])
	return buf[:cut+1]
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
