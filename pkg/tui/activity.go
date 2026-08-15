package tui

import (
	"slices"
	"strconv"
)

// maxActivityRows caps how many activity text rows are shown; overflow becomes
// a single dim "+N more" line, so the block never grows past maxActivityBudget.
const (
	maxActivityRows   = 3
	maxActivityBudget = maxActivityRows + 1 // includes the indicator row when capped
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
// elided to width (never wrapped) and followed by a dim "+N more" line when the
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
		rows = append(rows, truncateDisplay(u.theme.Dim.Wrap(u.activity[i].text), w))
	}
	extra := len(u.activity) - len(rows)
	if extra > 0 && len(rows) < budget {
		rows = append(rows, u.theme.Dim.Wrap("+"+strconv.Itoa(extra)+" more"))
	}
	return rows
}
