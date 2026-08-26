package tui

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jentfoo/ajent/pkg/strutil"
)

// outputHeadLines is how many lines of a tool's output reach history before the
// remainder collapses into one summary line.
const outputHeadLines = 4

// outputKey namespaces a running tool's activity row against sub-agent rows, so
// long streamed output shows movement past its committed head.
const outputKey = "tool-output"

// outputHead splits a tool's output into a short committed head and a count of
// everything past it, so long output leaves one summary line instead of pages.
// When full is set the cap is bypassed: every line reaches history. The stager
// enables it for user-initiated `!`/`!!` shells, whose output must be shown whole.
type outputHead struct {
	buf   lineBuffer // whole lines only; never splits an escape sequence
	shown int        // head lines already committed
	lines int        // lines seen in total
	chars int        // runes seen past the head, for the summary count
	bytes int        // bytes seen past the head, for a live activity row
	full  bool       // show every line; no collapse or summary
}

// add appends s and returns any whole lines to commit, capped at the head. The
// rest are counted into hidden() instead of shown.
func (h *outputHead) add(s string) string {
	return h.commit(h.buf.Add(s))
}

// flush commits the trailing partial line under the same cap and empties the buffer.
func (h *outputHead) flush() string { return h.commit(h.buf.Flush()) }

// commit counts each newline-terminated whole line, returning those still within
// the head (or every line when full). Empty when nothing is ready to show.
func (h *outputHead) commit(whole string) string {
	if whole == "" {
		return ""
	}
	var b strings.Builder
	for _, ln := range strings.Split(strings.TrimSuffix(whole, "\n"), "\n") {
		h.lines++
		if h.full || h.shown < outputHeadLines {
			b.WriteString(ln)
			b.WriteByte('\n')
			h.shown++
		} else {
			h.chars += utf8.RuneCountInString(ln)
			h.bytes += len(ln)
		}
	}
	return b.String()
}

// hidden reports how many lines were counted but not shown. shown tracks every
// line actually committed, so it equals lines in full mode and the count is zero.
func (h *outputHead) hidden() int { return max(0, h.lines-h.shown) }

// summary returns the collapse line for whatever was hidden, "" when nothing.
func (h *outputHead) summary() string {
	if n := h.hidden(); n > 0 {
		return "… +" + strconv.Itoa(n) + " lines, " + strutil.FormatTokens(h.chars) + " chars"
	}
	return ""
}

// reset empties the head so a new tool call starts clean.
func (h *outputHead) reset() {
	h.buf.pending.Reset()
	h.shown = 0
	h.lines = 0
	h.chars = 0
	h.bytes = 0
	h.full = false
}
