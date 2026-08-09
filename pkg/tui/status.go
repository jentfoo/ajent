package tui

import (
	"strconv"
	"strings"
)

const (
	statusSep      = " · "
	statusBarCells = 10
	barFull        = "▓"
	barEmpty       = "░"
)

// Status is the state rendered on the line below the input field.
type Status struct {
	Model     string
	Tokens    int
	MaxTokens int
}

// render returns the single status line, truncated to width.
func (s Status) render(t Theme, width int) string {
	var b strings.Builder
	if s.MaxTokens > 0 {
		pct := s.Tokens * 100 / s.MaxTokens
		if pct > 100 {
			pct = 100
		}
		b.WriteString(t.Dim.Wrap("ctx "))
		b.WriteString(usageStyle(t, pct).Wrap(strconv.Itoa(pct) + "%"))
		b.WriteString(" ")
		b.WriteString(usageStyle(t, pct).Wrap(usageBar(pct)))
		b.WriteString(t.Dim.Wrap(" " + formatTokens(s.Tokens) + "/" + formatTokens(s.MaxTokens)))
	}
	if s.Model != "" {
		if b.Len() > 0 {
			b.WriteString(t.Dim.Wrap(statusSep))
		}
		b.WriteString(t.Dim.Wrap(s.Model))
	}
	return truncateDisplay(b.String(), width)
}

// usageStyle escalates the color as the context fills.
func usageStyle(t Theme, pct int) Style {
	switch {
	case pct >= 90:
		return t.DiffDel
	case pct >= 70:
		return t.Code
	default:
		return t.Dim
	}
}

// usageBar renders pct as a fixed width block bar.
func usageBar(pct int) string {
	filled := (pct*statusBarCells + 99) / 100
	if filled > statusBarCells {
		filled = statusBarCells
	} else if filled < 0 {
		filled = 0
	}
	return strings.Repeat(barFull, filled) + strings.Repeat(barEmpty, statusBarCells-filled)
}

// formatTokens abbreviates a token count, such as 68.2k or 1.2M.
func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return trimZero(strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64)) + "M"
	case n >= 1_000:
		return trimZero(strconv.FormatFloat(float64(n)/1_000, 'f', 1, 64)) + "k"
	default:
		return strconv.Itoa(n)
	}
}

func trimZero(s string) string {
	return strings.TrimSuffix(s, ".0")
}
