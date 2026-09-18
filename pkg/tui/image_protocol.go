package tui

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// cellSizeQuery asks the terminal for one cell's pixel size (CSI 16 t); the
// reply arrives as CSI 6;<height>;<width> t on the input stream.
const cellSizeQuery = csi + "16t"

// defaultCellW/H assume the common 10x20 cell when the query goes unanswered,
// so a failed query still gets sane row accounting.
const (
	defaultCellW = 10
	defaultCellH = 20
)

// setCellSize records a CSI 16 t reply. Caller holds the UI lock.
func (u *UI) setCellSize(w, h int) {
	if w > 0 && h > 0 {
		u.cellW, u.cellH = w, h
	}
}

// imageRowsFor converts an image's pixel height to terminal rows.
func (u *UI) imageRowsFor(pxH int) int {
	if pxH <= 0 {
		return 1
	}
	h := u.cellH
	if h <= 0 {
		h = defaultCellH
	}
	return max((pxH+h-1)/h, 1)
}

// imageColsFor converts an image's pixel width to terminal columns.
func (u *UI) imageColsFor(pxW int) int {
	if pxW <= 0 {
		return 1
	}
	w := u.cellW
	if w <= 0 {
		w = defaultCellW
	}
	return max((pxW+w-1)/w, 1)
}

// viewportRowDivisor bounds an image's height at the viewport over this, so
// one image can never fill the whole screen.
const viewportRowDivisor = 2

// fitCells caps a cell placement to the terminal, scaling both axes together so
// the aspect survives: width to the viewport, height to a fraction of it.
// Terminals clip oversized draws while the committed spacer rows remain, and
// blank gaps in scrollback are how an unbounded placement looks broken.
func fitCells(cols, rows, termCols, termRows int) (int, int) {
	scale := 1.0
	if termCols > 0 && cols > termCols {
		scale = min(scale, float64(termCols)/float64(cols))
	}
	if termRows > 0 {
		if maxRows := max(termRows/viewportRowDivisor, 1); rows > maxRows {
			scale = min(scale, float64(maxRows)/float64(rows))
		}
	}
	if scale >= 1 {
		return cols, rows
	}
	return max(int(float64(cols)*scale+0.5), 1), max(int(float64(rows)*scale+0.5), 1)
}

// imagePayloadChunk bounds the base64 payload of one kitty transmission, so
// every control sequence stays well under any terminal's input buffer.
const imagePayloadChunk = 4096

// kittyTransmit renders one image as a single kitty graphics transmission:
// full control keys on the first chunk (m=1/m=0 continuations carry only m,
// per the spec), with the c/r cell sizing on that same a=T so the transmit
// doubles as the one placement. q=2 quiets failure replies too, so a rejected
// transmit cannot echo its error into the input stream.
//
// f=100 names PNG, so the caller hands this PNG bytes: JPEG payloads are
// converted first, since only some terminals sniff the real format.
func kittyTransmit(id uint32, data []byte, cols, rows int) string {
	enc := base64.StdEncoding.EncodeToString(data)
	var b strings.Builder
	b.Grow(len(enc) + 128)
	head := "a=T,f=100,q=2,C=1,i=" + strconv.FormatUint(uint64(id), 10) +
		",c=" + strconv.Itoa(max(cols, 1)) + ",r=" + strconv.Itoa(max(rows, 1)) + ","
	first := true
	remaining := enc
	for {
		n := min(imagePayloadChunk, len(remaining))
		more := len(remaining) > n
		keys := "m=0"
		if more {
			keys = "m=1"
		}
		if first { // continuations carry only m, per the spec
			keys = head + keys
			first = false
		}
		b.WriteString(esc + "_G" + keys + ";")
		b.WriteString(remaining[:n])
		b.WriteString(esc + "\\")
		remaining = remaining[n:]
		if !more {
			break
		}
	}
	return b.String()
}

// kittyDelete renders the deletion sequence for one image id, freeing its
// placements and data. q=2 for the same reason as the transmit.
func kittyDelete(id uint32) string {
	return esc + "_Ga=d,q=2,d=I,i=" + strconv.FormatUint(uint64(id), 10) + esc + "\\"
}

// iterm2Sequence renders one image as an OSC 1337 inline file sized in cells.
// There is no chunking and no deletion: the whole payload rides one sequence.
// doNotMoveCursor keeps the cursor where it was, like kitty's placement, so
// the shared spacer-row model holds for both protocols.
func iterm2Sequence(data []byte, cols, rows int) string {
	var b strings.Builder
	b.Grow(len(data)*4/3 + 128)
	b.WriteString(esc + "]1337;File=inline=1;doNotMoveCursor=1;size=" + strconv.Itoa(len(data)) +
		";width=" + strconv.Itoa(max(cols, 1)) +
		";height=" + strconv.Itoa(max(rows, 1)) +
		";preserveAspectRatio=0:")
	b.WriteString(base64.StdEncoding.EncodeToString(data))
	b.WriteString(belString)
	return b.String()
}

// belString is the OSC terminator most terminals accept.
const belString = "\a"
