package tui

import (
	"strings"

	"github.com/rivo/uniseg"
)

// displayWidth returns the terminal column count of s, ignoring ANSI escape sequences.
func displayWidth(s string) int {
	var w int
	for _, seg := range splitANSI(s) {
		if !seg.escape {
			w += uniseg.StringWidth(seg.text)
		}
	}
	return w
}

// TruncateDisplay returns s limited to max columns, keeping escape sequences
// intact. Use it to size text by what the terminal shows rather than by bytes.
func TruncateDisplay(s string, max int) string { return truncateDisplay(s, max) }

// truncateDisplay returns s limited to max columns, keeping escape sequences intact.
func truncateDisplay(s string, max int) string {
	if max <= 0 {
		return ""
	} else if displayWidth(s) <= max {
		return s
	}
	var b strings.Builder
	var w int
	var styled bool
	for _, seg := range splitANSI(s) {
		if seg.escape {
			styled = true
			b.WriteString(seg.text)
			continue
		}
		g := uniseg.NewGraphemes(seg.text)
		for g.Next() {
			cw := uniseg.StringWidth(g.Str())
			if w+cw > max {
				if styled {
					b.WriteString(sgrReset)
				}
				return b.String()
			}
			w += cw
			b.WriteString(g.Str())
		}
	}
	if styled {
		b.WriteString(sgrReset)
	}
	return b.String()
}

// stripANSI returns s with every escape sequence removed.
func stripANSI(s string) string {
	var b strings.Builder
	for _, seg := range splitANSI(s) {
		if !seg.escape {
			b.WriteString(seg.text)
		}
	}
	return b.String()
}

// GraphemeCells splits s into grapheme clusters, one cell per element.
func GraphemeCells(s string) []string {
	return graphemesOf(s)
}

// graphemesOf splits s into grapheme clusters for cursor movement.
func graphemesOf(s string) []string {
	if s == "" {
		return nil
	}
	out := make([]string, 0, len(s))
	g := uniseg.NewGraphemes(s)
	for g.Next() {
		out = append(out, g.Str())
	}
	return out
}

type ansiSegment struct {
	text   string
	escape bool
}

// splitANSI separates escape sequences from printable runs
func splitANSI(s string) []ansiSegment {
	var segs []ansiSegment
	var start int
	for i := 0; i < len(s); {
		if s[i] != 0x1b {
			i++
			continue
		}
		if start < i {
			segs = append(segs, ansiSegment{text: s[start:i]})
		}
		end := escapeEnd(s, i)
		segs = append(segs, ansiSegment{text: s[i:end], escape: true})
		i, start = end, end
	}
	if start < len(s) {
		segs = append(segs, ansiSegment{text: s[start:]})
	}
	return segs
}

// escapeEnd returns the index just past the escape sequence beginning at i.
func escapeEnd(s string, i int) int {
	j := i + 1
	if j >= len(s) {
		return len(s)
	}
	switch s[j] {
	case '[': // CSI, parameters then a final byte in 0x40-0x7e
		for j++; j < len(s); j++ {
			if s[j] >= 0x40 && s[j] <= 0x7e {
				return j + 1
			}
		}
		return len(s)
	case ']': // OSC, terminated by BEL or ST
		for j++; j < len(s); j++ {
			if s[j] == 0x07 {
				return j + 1
			} else if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
		}
		return len(s)
	default:
		return j + 1
	}
}
