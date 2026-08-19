package tui

import (
	"slices"
	"strconv"
	"strings"
)

// shadeRow renders one activity line padded inside its style's background,
// so the shade spans the row rather than sitting under the status text alone.
// The row is sanitized first so the fill measures exactly what is drawn, elided
// (never wrapped) and padded one column short of w: uniseg and the terminal
// can disagree by a column on a wide glyph, and a disagreement then reflows
// instead of wrapping the band into a second row.
func shadeRow(st Style, text string, w int) string {
	open := st.Open()
	if open == "" || w <= 0 { // no color or unknown width: plain elided row
		return truncateDisplay(sanitizeRow(text), max(w-1, 1))
	}
	sw := max(w-1, 1)
	body := truncateDisplay(sanitizeRow(text), sw)
	fill := sw - displayWidth(body)
	var b strings.Builder
	b.WriteString(open)
	b.WriteString(body)
	if fill > 0 {
		for i := 0; i < fill; i++ { // trailing blanks carry the background shade
			b.WriteByte(' ')
		}
	}
	b.WriteString(sgrReset)
	return b.String()
}

// maxActivityRows caps how many activity text rows are shown; overflow becomes
// a single dim "+N more" line, so the block never grows past maxActivityBudget.
const (
	maxActivityRows   = 3
	maxActivityBudget = maxActivityRows + 1 // includes the indicator row when capped
)

// Queued pending-prompt rows mirror activity: up to maxQueuedRows text rows,
// then a dim "+N more" line, so they never grow past maxQueuedBudget.
const (
	maxQueuedRows   = 3
	maxQueuedBudget = maxQueuedRows + 1 // includes the indicator row when capped
)

// activityRow is one transient, keyed status line above the input.
type activityRow struct {
	key  string
	text string
}

// SetActivity adds, replaces or (with an empty text) removes a keyed activity
// row above the input. Rows live in the live block only and never reach history.
func (u *UI) SetActivity(key, text string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.setActivityLocked(key, text)
}

// setActivityLocked adds/replaces/removes a keyed activity row. Caller holds the lock.
func (u *UI) setActivityLocked(key, text string) {
	text = sanitizeRow(text)

	for i := range u.activity {
		if u.activity[i].key != key {
			continue
		}
		if text == "" {
			u.activity = slices.Delete(u.activity, i, i+1)
		} else {
			u.activity[i].text = text
		}
		u.repaint()
		return
	}
	if text != "" {
		u.activity = append(u.activity, activityRow{key: key, text: text})
	}
	u.repaint()
}

// activityRows renders the ordered activity rows into at most budget rows, each
// full-width shaded (never wrapped) and followed by a dim "+N more" line when the
// cap is exceeded. Caller holds the lock.
func (u *UI) activityRows(w, budget int) []string {
	if len(u.activity) == 0 || budget <= 0 {
		return nil
	}
	n := min(len(u.activity), maxActivityRows)
	var rows []string
	for i := range n {
		if len(rows) >= budget {
			break
		}
		// never wrap: a wrapped row breaks exact row accounting
		rows = append(rows, shadeRow(u.theme.Activity, u.activity[i].text, w))
	}
	extra := len(u.activity) - len(rows)
	if extra > 0 && len(rows) < budget {
		rows = append(rows, u.theme.Dim.Wrap("+"+strconv.Itoa(extra)+" more"))
	}
	return rows
}

// queuedRows renders the ordered pending-prompt labels into at most budget dim
// shaded rows (oldest first), each prefixed with the user marker and showing only
// its first line, then a dim "+N more" overflow row when more remain. Caller holds
// the lock.
func (u *UI) queuedRows(w, budget int) []string {
	if len(u.queued) == 0 || budget <= 0 {
		return nil
	}
	n := min(len(u.queued), maxQueuedRows)
	var rows []string
	for i := range n {
		if len(rows) >= budget {
			break
		}
		label := u.queued[i]
		// a queued message can be multi-line; only its first line fits one shaded row
		if first, _, ok := strings.Cut(label, "\n"); ok {
			label = first
		}
		rows = append(rows, shadeRow(u.theme.Activity, userMarker+label, w))
	}
	extra := len(u.queued) - len(rows)
	if extra > 0 && len(rows) < budget {
		rows = append(rows, u.theme.Dim.Wrap("+"+strconv.Itoa(extra)+" more"))
	}
	return rows
}
