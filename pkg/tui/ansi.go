package tui

import (
	"strconv"
	"strings"
)

// Terminal escape sequences emitted by this package
const (
	esc = "\x1b"
	csi = esc + "["

	eraseLine  = csi + "2K"
	eraseBelow = csi + "0J"
	eraseTail  = csi + "0K" // row diffs clear the tail of a rewritten row

	hideCursor = csi + "?25l"
	showCursor = csi + "?25h"

	bracketedPasteOn  = csi + "?2004h"
	bracketedPasteOff = csi + "?2004l"

	pasteStart = csi + "200~"
	pasteEnd   = csi + "201~"

	// synchronized output, avoids tearing while a frame is written
	beginSync = csi + "?2026h"
	endSync   = csi + "?2026l"

	// statusQuery asks the terminal for a DSR "no malfunction" reply (CSI 0 n).
	// The reply is emitted only after the terminal has processed everything
	// that preceded the query — including a pending resize reflow — so it
	// serves as a barrier before drawing after a settled resize.
	statusQuery = csi + "5n"

	sgrReset = csi + "0m"
	// the caret is drawn as a reversed cell rather than parked on with the
	// terminal's own cursor, which stays hidden and out of the row maths
	caretReverse = csi + "7m"
)

// cursorTo moves the cursor to an absolute 1-indexed position.
func cursorTo(row, col int) string {
	return csi + strconv.Itoa(row) + ";" + strconv.Itoa(col) + "H"
}

func cursorUp(n int) string {
	if n <= 0 {
		return ""
	}
	return csi + strconv.Itoa(n) + "A"
}

func cursorRight(n int) string {
	if n <= 0 {
		return ""
	}
	return csi + strconv.Itoa(n) + "C"
}

// sgr builds a Select Graphic Rendition sequence from the given parameters.
func sgr(params ...int) string {
	if len(params) == 0 {
		return sgrReset
	}
	var b strings.Builder
	b.WriteString(csi)
	for i, p := range params {
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(strconv.Itoa(p))
	}
	b.WriteByte('m')
	return b.String()
}
