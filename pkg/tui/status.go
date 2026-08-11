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

// Segment is a keyed status line item, rendered after the model in insertion
// order and dropped first when the line does not fit.
type Segment struct {
	Key  string
	Text string
}

// Status is the state rendered on the line below the input field.
type Status struct {
	Spinner   string // the working glyph, first element (bottom-left corner); static at rest
	Tool      string // a running tool's label, shown right after the spinner while active
	Model     string
	Tokens    int  // context usage count; drives the bar against Budget()
	MaxTokens int  // the model's window; 0 renders no bar
	Reserve   int  // tokens held back from MaxTokens for a response
	Estimated bool // Used includes an estimate; prefixes the count with ~
	Segments  []Segment
}

// render returns the single status line, truncated to width. Segments are
// dropped one at a time until it fits, so the model survives longest.
func (s Status) render(t Theme, width int) string {
	parts := s.parts(t)
	for len(parts) > 0 {
		line := strings.Join(parts, t.Dim.Wrap(statusSep))
		if displayWidth(line) <= width || len(parts) == 1 {
			return truncateDisplay(line, width)
		}
		parts = parts[:len(parts)-1] // the last segment is the least important
	}
	return ""
}

// parts returns the status pieces in priority order, most important first. The
// context bar and token totals survive longest; the percentage is omitted as it
// duplicates what the bar already conveys.
func (s Status) parts(t Theme) []string {
	var parts []string
	if s.Spinner != "" {
		parts = append(parts, s.Spinner)
	}
	if s.Tool != "" {
		parts = append(parts, t.Dim.Wrap(s.Tool))
	}
	if s.MaxTokens > 0 {
		// the bar fills to the compaction point (window minus reserve); the count
		// shows used against the real window. A full bar means compaction fires now.
		budget := s.MaxTokens - s.Reserve
		if budget <= 0 {
			budget = s.MaxTokens
		}
		pct := s.Tokens * 100 / budget
		if pct < 0 {
			pct = 0
		} else if pct > 100 {
			pct = 100
		}
		bar := usageStyle(t, pct).Wrap(usageBar(pct))
		tilde := ""
		if s.Estimated {
			tilde = "~" // the count is approximate until the next provider report
		}
		toks := t.Dim.Wrap(tilde + formatTokens(s.Tokens) + "/" + formatTokens(s.MaxTokens))
		parts = append(parts, bar+" "+toks)
	}
	if s.Model != "" {
		parts = append(parts, t.Dim.Wrap(s.Model))
	}
	for _, seg := range s.Segments {
		if seg.Text != "" {
			parts = append(parts, t.Dim.Wrap(seg.Text))
		}
	}
	return parts
}

// SetStatusSegment adds, replaces or (with an empty text) removes a keyed
// status segment.
func (u *UI) SetStatusSegment(key, text string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	for i, seg := range u.status.Segments {
		if seg.Key != key {
			continue
		}
		if text == "" {
			u.status.Segments = append(u.status.Segments[:i], u.status.Segments[i+1:]...)
		} else {
			u.status.Segments[i].Text = text
		}
		u.repaint()
		return
	}
	if text != "" {
		u.status.Segments = append(u.status.Segments, Segment{Key: key, Text: text})
	}
	u.repaint()
}

// SetModel updates the model name and context window shown in the status line,
// leaving any segments alone.
func (u *UI) SetModel(name string, maxTokens int) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.status.Model = name
	if maxTokens > 0 {
		u.status.MaxTokens = maxTokens
	}
	u.repaint()
}

// usageStyle escalates the color as the context fills against its budget.
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

// FormatTokens abbreviates a token count, such as 68.2k or 1.2M.
func FormatTokens(n int) string {
	return formatTokens(n)
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
