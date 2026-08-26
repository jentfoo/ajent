package tui

import (
	"slices"
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
// order and shortened, then dropped, lowest priority first once even two rows
// overflow.
type Segment struct {
	Key      string
	Text     string
	Short    string // used when the full text does not fit; falls back to Text
	Priority int
}

// Status is the state rendered on the line below the input field.
type Status struct {
	Spinner    string // the working glyph, first element (bottom-left corner); static at rest
	Tool       string // a running tool's label, shown right after the spinner while active
	Model      string // full model label; the first thing to collapse when its row overflows
	ModelShort string // short model label, collapsing Model before anything else changes
	Tokens     int    // context usage count; drives the bar against Budget()
	MaxTokens  int    // the model's window; 0 renders no bar
	Reserve    int    // tokens held back from MaxTokens for a response
	Compact    int    // where an auto-compaction fires; when set, the bar fills against it
	Estimated  bool   // Used includes an estimate; prefixes the count with ~
	Segments   []Segment
}

// rows renders the status block, preferring one row: everything at full text,
// then the model shortened, then segments shortened in drop order, and only
// then a second row. Row one keeps the fixed part plus the model (shortening
// before clipping it); row two holds the segments at as full a text as fits.
// Never more than two rows; segments drop in drop order (lowest priority first,
// ties later insertion first) once even two rows overflow.
func (s Status) rows(t Theme, width int) []string {
	fixed := s.fixedParts(t)
	order := activeSegments(s.Segments)
	full := segTexts(t, s.Segments, order)

	if line := joinStatus(slices.Concat(fixed, s.modelText(t, false), full), t); displayWidth(line) <= width {
		return []string{truncateDisplay(line, width)}
	}
	// the model shortens first: one row at its short form, segments still full
	prefix := slices.Concat(fixed, s.modelText(t, true))
	if line := joinStatus(slices.Concat(prefix, full), t); displayWidth(line) <= width {
		return []string{truncateDisplay(line, width)}
	}
	dropSeq := s.dropOrder()
	// segments shorten in drop order before the block may split to two rows
	if line, fits := s.packSegmentRow(t, prefix, order, dropSeq, width); fits {
		return []string{truncateDisplay(line, width)}
	}
	// two rows: row one keeps the full model while it fits, shortening then
	// clipping it; segments take a second row
	row1 := joinStatus(slices.Concat(fixed, s.modelText(t, false)), t)
	if displayWidth(row1) > width {
		row1 = joinStatus(slices.Concat(fixed, s.modelText(t, true)), t)
	}
	row1 = truncateDisplay(row1, width)
	if len(order) == 0 {
		return []string{row1} // nothing left for a second row
	}
	for {
		line, fits := s.packSegmentRow(t, nil, order, dropSeq, width)
		if fits || len(order) == 1 {
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
// token totals. The model is not among them; it collapses first when its row
// would overflow (see rows).
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
	return parts
}

// modelText wraps the model label (short form when short), nil when unset.
func (s Status) modelText(t Theme, short bool) []string {
	name := s.Model
	if short {
		name = s.ModelShort
		if name == "" {
			name = s.Model // no short form: the full label is also the short one
		}
	}
	if name == "" {
		return nil
	}
	return []string{t.Dim.Wrap(name)}
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

// packSegmentRow packs the segments after prefix at as full a text as fits:
// segments start full and shorten in drop order until the row fits or all are
// short. The bool reports whether the row fits the width.
func (s Status) packSegmentRow(t Theme, prefix []string, idxs, dropSeq []int, width int) (string, bool) {
	line := joinStatus(slices.Concat(prefix, segTexts(t, s.Segments, idxs)), t)
	for k := range dropSeq {
		if displayWidth(line) <= width {
			break
		}
		line = joinStatus(slices.Concat(prefix, s.segRowTexts(t, idxs, dropSeq[:k+1])), t)
	}
	return line, displayWidth(line) <= width
}

// segRowTexts renders the given segments at full text, except those in
// shortSet, which take their short form (falling back to full).
func (s Status) segRowTexts(t Theme, idxs, shortSet []int) []string {
	texts := make([]string, 0, len(idxs))
	for _, i := range idxs {
		text := s.Segments[i].Text
		if slices.Contains(shortSet, i) {
			if short := s.Segments[i].Short; short != "" {
				text = short
			}
		}
		texts = append(texts, t.Dim.Wrap(text))
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

// SetModel updates the model labels and context window shown in the status
// line, leaving any segments alone. name is the full label, short its collapse
// (empty falls back to name).
func (u *UI) SetModel(name, short string, maxTokens int) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.status.Model = sanitizeRow(name)
	u.status.ModelShort = sanitizeRow(short)
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

// Segment is a keyed status line item
