package tui

import (
	"slices"
	"strconv"
	"strings"

	"github.com/jentfoo/ajent/pkg/strutil"
)

const (
	statusSep      = " · "
	statusBarCells = 10
	barFull        = "▓"
	barEmpty       = "░"
)

// Segment is a keyed status line item, rendered after the model in insertion
// order and dropped lowest priority first when even two rows overflow.
type Segment struct {
	Key      string
	Text     string
	Short    string // used when the full text does not fit; falls back to Text
	Priority int
}

// Status is the state rendered on the line below the input field.
type Status struct {
	Spinner   string // the working glyph, first element (bottom-left corner); static at rest
	Tool      string // a running tool's label, shown right after the spinner while active
	Model     string
	Tokens    int  // context usage count; drives the bar against Budget()
	MaxTokens int  // the model's window; 0 renders no bar
	Reserve   int  // tokens held back from MaxTokens for a response
	Compact   int  // where an auto-compaction fires; when set, the bar fills against it
	Estimated bool // Used includes an estimate; prefixes the count with ~
	Segments  []Segment
}

// rows renders the status block: one row when everything fits at full text,
// else a second short-form segment row. Never more than two; segments drop in
// priority order (lowest first) only when even two rows overflow.
func (s Status) rows(t Theme, width int) []string {
	fixed := s.fixedParts(t)
	order := activeSegments(s.Segments)

	if line := joinStatus(append(fixed, segTexts(t, s.Segments, order)...), t); displayWidth(line) <= width {
		return []string{truncateDisplay(line, width)}
	}
	// the fixed part stays on row one; segments move to a second short-form row
	row1 := truncateDisplay(joinStatus(fixed, t), width)
	if len(order) == 0 {
		return []string{row1} // nothing left for a second row
	}
	dropSeq := s.dropOrder()
	for {
		line := joinStatus(segTextsShort(t, s.Segments, order), t)
		if displayWidth(line) <= width || len(order) == 1 {
			return []string{row1, truncateDisplay(line, width)}
		}
		dropped := dropSeq[0]
		dropSeq = dropSeq[1:]
		for i, idx := range order {
			if idx == dropped {
				order = slices.Delete(order, i, i+1)
				break
			}
		}
	}
}

// fixedParts returns the always-present pieces: spinner, tool, context bar and
// token totals, then model. The percentage is omitted as it duplicates what the
// bar already conveys.
func (s Status) fixedParts(t Theme) []string {
	var parts []string
	if s.Spinner != "" {
		parts = append(parts, s.Spinner)
	}
	if s.Tool != "" {
		parts = append(parts, t.Dim.Wrap(s.Tool))
	}
	if s.MaxTokens > 0 {
		// bar fills to where an auto-compact would fire when that is known, else to
		// the response-safe budget (window−reserve); count shows used vs the real window.
		budget := s.Compact
		if budget <= 0 {
			budget = s.MaxTokens - s.Reserve
		}
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
		toks := t.Dim.Wrap(tilde + strutil.FormatTokens(s.Tokens) + "/" + strutil.FormatTokens(s.MaxTokens))
		parts = append(parts, bar+" "+toks)
	}
	if s.Model != "" {
		parts = append(parts, t.Dim.Wrap(s.Model))
	}
	return parts
}

// activeSegments lists segment indices whose text is non-empty, in insertion order.
func activeSegments(segs []Segment) []int {
	var idxs []int
	for i := range segs {
		if segs[i].Text != "" {
			idxs = append(idxs, i)
		}
	}
	return idxs
}

// segTexts renders the given segments' full text.
func segTexts(t Theme, segs []Segment, idxs []int) []string {
	texts := make([]string, 0, len(idxs))
	for _, i := range idxs {
		texts = append(texts, t.Dim.Wrap(segs[i].Text))
	}
	return texts
}

// segTextsShort renders segment short forms (falling back to Text).
func segTextsShort(t Theme, segs []Segment, idxs []int) []string {
	texts := make([]string, 0, len(idxs))
	for _, i := range idxs {
		short := segs[i].Short
		if short == "" {
			short = segs[i].Text
		}
		texts = append(texts, t.Dim.Wrap(short))
	}
	return texts
}

// dropOrder returns segment indices ordered by when to drop them: lowest priority
// first, ties dropping the later insertion earlier (matching drop-last behaviour).
func (s Status) dropOrder() []int {
	idxs := activeSegments(s.Segments)
	slices.SortStableFunc(idxs, func(a, b int) int {
		if s.Segments[a].Priority != s.Segments[b].Priority {
			return s.Segments[a].Priority - s.Segments[b].Priority
		}
		return b - a // later insertion drops first
	})
	return idxs
}

// joinStatus joins status pieces with the dim separator.
func joinStatus(parts []string, t Theme) string {
	return strings.Join(parts, t.Dim.Wrap(statusSep))
}

// SetStatusSegment adds, replaces or (with an empty Text) removes a keyed status
// segment.
func (u *UI) SetStatusSegment(seg Segment) {
	u.mu.Lock()
	defer u.mu.Unlock()
	seg.Text = sanitizeRow(seg.Text) // arbitrary caller text; keep SGR only
	seg.Short = sanitizeRow(seg.Short)

	for i := range u.status.Segments {
		if u.status.Segments[i].Key != seg.Key {
			continue
		}
		if seg.Text == "" {
			u.status.Segments = slices.Delete(u.status.Segments, i, i+1)
		} else {
			u.status.Segments[i] = seg
		}
		u.repaint()
		return
	}
	if seg.Text != "" {
		u.status.Segments = append(u.status.Segments, seg)
	}
	u.repaint()
}

// SetModel updates the model name and context window shown in the status line,
// leaving any segments alone.
func (u *UI) SetModel(name string, maxTokens int) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.status.Model = sanitizeRow(name)
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

// FormatBytes abbreviates a byte count, such as 259b, 3.5kb or 1.2mb. Binary
// units with an explicit suffix, so a size is never mistaken for a token count;
// kept in step with tools.HumanSize, which annotations use.
func FormatBytes(n int) string {
	const (
		kb = 1024.0
		mb = 1024.0 * 1024.0
	)
	switch {
	case float64(n) >= mb:
		return strutil.TrimZero(strconv.FormatFloat(float64(n)/mb, 'f', 1, 64)) + "mb"
	case float64(n) >= kb:
		return strutil.TrimZero(strconv.FormatFloat(float64(n)/kb, 'f', 1, 64)) + "kb"
	default:
		return strconv.Itoa(n) + "b"
	}
}
