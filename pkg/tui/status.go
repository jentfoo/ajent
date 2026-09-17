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

// Positions of the pieces the block owns. Segments carry their own Order set by the
// front end, so they always follow these; the numbers leave room in between.
const (
	orderSpinner = -30 // leftmost: the working glyph
	orderContext = -20 // bar plus used/total
	orderModel   = -10 // the model label, last piece of row one

	// neverCollapse marks a piece that keeps its full text at any width. A missing
	// short form has the same effect.
	neverCollapse = -1

	// modelCollapse places the model near the front of the collapse ladder: labels are
	// long, so it shortens early rather than pushing segments off. It never vanishes.
	modelCollapse = 1
)

// Segment is a keyed status line item, placed by Order and collapsed to its short form
// in Priority order when the row runs out of room.
type Segment struct {
	Key   string
	Text  string
	Short string // used when the full text does not fit; falls back to Text

	Order    int // display position, ascending; ties keep insertion order
	Priority int // collapse and drop step: 0 goes first, then 1, 2…; negative goes last
}

// Status is the state rendered on the line below the input field.
type Status struct {
	Spinner    string // the working glyph, first element (bottom-left corner); static at rest
	Model      string // full model label
	ModelShort string // short model label, taken once every lower-priority piece has collapsed
	Tokens     int    // context usage count; drives the bar against Budget()
	MaxTokens  int    // the model's window; 0 renders no bar
	Reserve    int    // tokens held back from MaxTokens for a response
	Compact    int    // where an auto-compaction fires; when set, the bar fills against it
	Estimated  bool   // Used includes an estimate; prefixes the count with ~
	Segments   []Segment
}

// part is one rendered status piece: where it sits and what shortening it costs.
type part struct {
	order int    // display position, ascending
	coll  int    // collapse step; neverCollapse (or no short form) means it keeps full text
	full  string // already styled
	short string // already styled, empty when there is nothing to collapse to
}

// rows renders the status block. The ladder is one rule at every width: keep pieces at
// full text and collapse them in Priority order (unset first, then ascending, negative
// never) until the row fits. Only a fully collapsed single row that still overflows
// splits in two: row one keeps the fixed part plus the model, row two holds the
// segments, and pieces drop there by ascending Priority (negatives last) before anything
// is clipped. Never more than two rows.
func (s Status) rows(t Theme, width int) []string {
	parts := s.parts(t)
	if line, fits := packRow(t, parts, width); fits || len(parts) == 1 {
		return []string{truncateDisplay(line, width)}
	}

	head, tail := splitRow(parts)
	row1, _ := packRow(t, head, width)
	row1 = truncateDisplay(row1, width)
	if len(tail) == 0 {
		return []string{row1} // nothing left for a second row
	}
	for {
		line, fits := packRow(t, tail, width)
		if fits || len(tail) == 1 {
			return []string{row1, truncateDisplay(line, width)}
		}
		// still overflowing with everything short: the ladder's first loser leaves,
		// and the survivors get another pass at full text
		tail = withoutPart(tail, dropOrder(tail)[0])
	}
}

// parts lists every piece in display order: the fixed part, the model, then segments by
// their Order. Sorting is stable so equal orders keep insertion order; a segment removed
// between paints therefore cannot drag its neighbours around.
func (s Status) parts(t Theme) []part {
	parts := s.fixedParts(t)
	if name := s.modelText(t); name != "" {
		parts = append(parts, part{orderModel, modelCollapse, name, s.modelTextShort(t)})
	}
	for _, seg := range s.Segments {
		if seg.Text == "" {
			continue
		}
		var short string
		if seg.Short != "" {
			short = t.Dim.Wrap(seg.Short)
		}
		parts = append(parts, part{seg.Order, seg.Priority, t.Dim.Wrap(seg.Text), short})
	}
	slices.SortStableFunc(parts, func(a, b part) int { return a.order - b.order })
	return parts
}

// fixedParts returns the always-present pieces: the spinner and the context bar with its
// token totals. Both keep their full text at any width.
func (s Status) fixedParts(t Theme) []part {
	var parts []part
	if s.Spinner != "" {
		parts = append(parts, part{orderSpinner, neverCollapse, s.Spinner, ""})
	}
	if s.MaxTokens <= 0 {
		return parts
	}
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
	var tilde string
	if s.Estimated {
		tilde = "~" // the count is approximate until the next provider report
	}
	toks := t.Dim.Wrap(tilde + strutil.FormatTokens(s.Tokens) + "/" + strutil.FormatTokens(s.MaxTokens))
	return append(parts, part{orderContext, neverCollapse, bar + " " + toks, ""})
}

// modelText returns the wrapped full model label, "" when unset.
func (s Status) modelText(t Theme) string {
	if s.Model == "" {
		return ""
	}
	return t.Dim.Wrap(s.Model)
}

// modelTextShort returns the wrapped short label; without one there is nothing to
// collapse to, so the full label stands at any width.
func (s Status) modelTextShort(t Theme) string {
	if s.ModelShort == "" || s.Model == "" {
		return ""
	}
	return t.Dim.Wrap(s.ModelShort)
}

// collapseOrder lists the indices that can shorten, in the order they give up their full
// text: lowest Priority first (0/unset before anything), ties the later position. A
// neverCollapse piece or one without a short form is absent.
func collapseOrder(parts []part) []int {
	var idxs []int
	for i := range parts {
		if parts[i].coll >= 0 && parts[i].short != "" {
			idxs = append(idxs, i)
		}
	}
	slices.SortStableFunc(idxs, func(a, b int) int {
		if parts[a].coll != parts[b].coll {
			return parts[a].coll - parts[b].coll
		}
		return b - a // later position yields first
	})
	return idxs
}

// dropOrder lists indices in the order they leave row two entirely, once shortening is not
// enough: lowest Priority first, neverCollapse pieces last, ties the rightmost position.
func dropOrder(parts []part) []int {
	idxs := make([]int, len(parts))
	for i := range parts {
		idxs[i] = i
	}
	slices.SortStableFunc(idxs, func(a, b int) int {
		if ga, gb := dropGroup(parts[a]), dropGroup(parts[b]); ga != gb {
			return ga - gb
		}
		if parts[a].coll != parts[b].coll {
			return parts[a].coll - parts[b].coll
		}
		return b - a
	})
	return idxs
}

// dropGroup ranks a piece for dropping: by Priority, with neverCollapse pieces last. A
// missing short form is not protection, it only means the piece cannot shorten.
func dropGroup(p part) int {
	if p.coll >= 0 {
		return 0
	}
	return 1
}

// packRow packs the given parts at as full a text as fits: everything full, then the
// collapse ladder one step at a time. The bool reports whether the row fits width.
func packRow(t Theme, parts []part, width int) (string, bool) {
	coll := collapseOrder(parts)
	for k := 0; k <= len(coll); k++ {
		line := joinStatus(partTexts(parts, coll[:k]), t)
		if displayWidth(line) <= width {
			return line, true
		}
	}
	// nothing left to collapse: the fully short row still needs clipping
	return joinStatus(partTexts(parts, coll), t), false
}

// partTexts renders the parts, taking the short form of those listed in collapsed.
func partTexts(parts []part, collapsed []int) []string {
	texts := make([]string, 0, len(parts))
	for i, p := range parts {
		text := p.full
		if p.short != "" && slices.Contains(collapsed, i) {
			text = p.short
		}
		texts = append(texts, text)
	}
	return texts
}

// splitRow divides the block into row one's pieces (everything up to and including the
// model) and the segments that may take a second row.
func splitRow(parts []part) (head, tail []part) {
	for _, p := range parts {
		if p.order <= orderModel {
			head = append(head, p)
		} else {
			tail = append(tail, p)
		}
	}
	return head, tail
}

// withoutPart returns the parts minus the given index, remapping nothing: callers drop by
// position in the slice they still hold.
func withoutPart(parts []part, idx int) []part {
	out := make([]part, 0, len(parts)-1)
	for i, p := range parts {
		if i != idx {
			out = append(out, p)
		}
	}
	return out
}

// joinStatus joins status pieces with the dim separator.
func joinStatus(parts []string, t Theme) string {
	return strings.Join(parts, t.Dim.Wrap(statusSep))
}

// SetStatusSegment adds, replaces or (with an empty Text) removes a keyed status
// segment. A replacement keeps the slot's position only through its Order, so a publisher
// that can be cleared and re-shown must pass the same one every time.
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
