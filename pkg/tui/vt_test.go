package tui

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rivo/uniseg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vt is a minimal terminal emulator covering the sequences this package emits,
// plus a few it no longer does. It exists so layout and scrolling can be asserted
// on a rendered screen rather than on raw escape bytes.
type vt struct {
	w, h       int
	cells      [][]rune
	cont       []bool // row i soft-wraps from row i-1
	row, col   int    // zero based
	top, bot   int    // scroll region, zero based inclusive
	savedRow   int
	savedCol   int
	wrapDefer  bool
	scrollback []string
	dsrCount   int
}

// Sequences the package no longer emits, kept so the emulator stays a faithful
// reference for the ones it does.
const (
	saveCursor    = esc + "7" // DECSC
	restoreCursor = esc + "8" // DECRC
)

// setRegionSeq builds a DECSTBM sequence.
func setRegionSeq(top, bottom int) string {
	return csi + strconv.Itoa(top) + ";" + strconv.Itoa(bottom) + "r"
}

func newVT(w, h int) *vt {
	v := &vt{w: w, h: h, bot: h - 1, cont: make([]bool, h)}
	v.cells = make([][]rune, h)
	for i := range v.cells {
		v.cells[i] = blankRow(w)
	}
	return v
}

func blankRow(w int) []rune {
	row := make([]rune, w)
	for i := range row {
		row[i] = ' '
	}
	return row
}

func (v *vt) Write(p []byte) (int, error) {
	v.WriteString(string(p))
	return len(p), nil
}

func (v *vt) WriteString(s string) {
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case 0x1b:
			i += v.escape(rs[i+1:])
		case 0x9b: // C1 CSI: xterm decodes UTF-8 then acts on U+009B
			i += v.csi(rs[i+1:])
		case '\r':
			v.col, v.wrapDefer = 0, false
		case '\n', '\v', '\f':
			v.lineFeed()
			v.cont[v.row], v.wrapDefer = false, false // an explicit line feed ends the line
		case '\b':
			v.col, v.wrapDefer = max(v.col-1, 0), false
		default:
			v.put(rs[i])
		}
	}
}

// escape handles the sequence following ESC and returns how many runes it used.
func (v *vt) escape(rs []rune) int {
	if len(rs) == 0 {
		return 0
	}
	switch rs[0] {
	case '7':
		v.savedRow, v.savedCol = v.row, v.col
		return 1
	case '8':
		v.row, v.col, v.wrapDefer = v.savedRow, v.savedCol, false
		return 1
	case 'D': // IND
		v.lineFeed()
		v.cont[v.row], v.wrapDefer = false, false
		return 1
	case 'E': // NEL
		v.col = 0
		v.lineFeed()
		v.cont[v.row], v.wrapDefer = false, false
		return 1
	case 'M': // RI
		v.reverseIndex()
		return 1
	case '[':
		return 1 + v.csi(rs[1:])
	default:
		return 1
	}
}

func (v *vt) csi(rs []rune) int {
	var i int
	for i < len(rs) && (rs[i] < 0x40 || rs[i] > 0x7e) {
		i++
	}
	if i >= len(rs) {
		return len(rs)
	}
	params, final := string(rs[:i]), rs[i]
	if strings.HasPrefix(params, "?") {
		if final == 'n' {
			v.dsrCount++
		}
		return i + 1 // private modes are not modelled
	}
	v.apply(params, final)
	return i + 1
}

func (v *vt) apply(params string, final rune) {
	p := parseParams(params)
	arg := func(i, def int) int {
		if i < len(p) && p[i] > 0 {
			return p[i]
		}
		return def
	}
	// only cursor motion clears a pending wrap; SGR and the erasers must not,
	// or an exact-width row followed by styling stops deferring
	switch final {
	case 'H', 'f':
		v.row = min(max(arg(0, 1)-1, 0), v.h-1)
		v.col = min(max(arg(1, 1)-1, 0), v.w-1)
		v.wrapDefer = false
	case 'A':
		v.row = max(v.row-arg(0, 1), 0)
		v.wrapDefer = false
	case 'B':
		v.row = min(v.row+arg(0, 1), v.h-1)
		v.wrapDefer = false
	case 'C':
		v.col = min(v.col+arg(0, 1), v.w-1)
		v.wrapDefer = false
	case 'D':
		v.col = max(v.col-arg(0, 1), 0)
		v.wrapDefer = false
	case 'd': // VPA
		v.row = min(max(arg(0, 1)-1, 0), v.h-1)
		v.wrapDefer = false
	case 'G': // CHA
		v.col = min(max(arg(0, 1)-1, 0), v.w-1)
		v.wrapDefer = false
	case 'K':
		v.eraseLine(arg(0, 0))
	case 'J':
		v.eraseDisplay(arg(0, 0))
	case 'L': // IL: blank lines appear at the cursor, region bottom lost
		if v.row >= v.top && v.row <= v.bot {
			n := min(arg(0, 1), v.bot-v.row+1)
			copy(v.cells[v.row+n:v.bot+1], v.cells[v.row:v.bot+1-n])
			copy(v.cont[v.row+n:v.bot+1], v.cont[v.row:v.bot+1-n])
			for i := v.row; i < v.row+n; i++ {
				v.cells[i], v.cont[i] = blankRow(v.w), false
			}
		}
	case 'M': // DL: cursor row and below shift up, blanks at region bottom
		if v.row >= v.top && v.row <= v.bot {
			n := min(arg(0, 1), v.bot-v.row+1)
			copy(v.cells[v.row:v.bot+1-n], v.cells[v.row+n:v.bot+1])
			copy(v.cont[v.row:v.bot+1-n], v.cont[v.row+n:v.bot+1])
			for i := v.bot + 1 - n; i <= v.bot; i++ {
				v.cells[i], v.cont[i] = blankRow(v.w), false
			}
		}
	case 'S': // SU
		for n := arg(0, 1); n > 0; n-- {
			v.scrollUp()
		}
	case 'T': // SD
		for n := arg(0, 1); n > 0; n-- {
			v.scrollDown()
		}
	case 'r':
		v.top = min(max(arg(0, 1)-1, 0), v.h-1)
		v.bot = min(max(arg(1, v.h)-1, 0), v.h-1)
		v.row, v.col, v.wrapDefer = 0, 0, false
	case 'n':
		v.dsrCount++
	}
}

func (v *vt) eraseLine(mode int) {
	switch mode {
	case 1:
		for c := 0; c <= v.col && c < v.w; c++ {
			v.cells[v.row][c] = ' '
		}
	case 2:
		v.cells[v.row] = blankRow(v.w)
	default:
		for c := v.col; c < v.w; c++ {
			v.cells[v.row][c] = ' '
		}
	}
}

func (v *vt) eraseDisplay(mode int) {
	if mode == 2 {
		for r := range v.cells {
			v.cells[r] = blankRow(v.w)
		}
		return
	}
	v.eraseLine(0)
	for r := v.row + 1; r < v.h; r++ {
		v.cells[r] = blankRow(v.w)
	}
}

func (v *vt) put(r rune) {
	if r == '\t' {
		v.col, v.wrapDefer = min((v.col/8+1)*8, v.w-1), false
		return
	}
	if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
		return // C0, DEL and C1 print nothing; 0x9b is CSI before this
	}
	if uniseg.StringWidth(string(r)) == 0 {
		return // zero-width runes attach to the prior cell: no advance, no wrap
	}
	if v.wrapDefer {
		v.col = 0
		v.lineFeed()
		v.cont[v.row] = true // the row we wrapped onto continues the one above
		v.wrapDefer = false
	}
	v.cells[v.row][v.col] = r
	if v.col == v.w-1 {
		v.wrapDefer = true
	} else {
		v.col++
	}
}

func (v *vt) lineFeed() {
	if v.row == v.bot {
		v.scrollUp()
	} else if v.row < v.h-1 {
		v.row++
	}
}

// scrollUp shifts the scroll region up one row, retiring the top line.
func (v *vt) scrollUp() {
	v.scrollback = append(v.scrollback, strings.TrimRight(string(v.cells[v.top]), " "))
	copy(v.cells[v.top:v.bot], v.cells[v.top+1:v.bot+1])
	copy(v.cont[v.top:v.bot], v.cont[v.top+1:v.bot+1])
	v.cells[v.bot] = blankRow(v.w)
	v.cont[v.bot] = false
}

// scrollDown shifts the scroll region down one row, retiring the bottom line.
func (v *vt) scrollDown() {
	copy(v.cells[v.top+1:v.bot+1], v.cells[v.top:v.bot])
	copy(v.cont[v.top+1:v.bot+1], v.cont[v.top:v.bot])
	v.cells[v.top] = blankRow(v.w)
	v.cont[v.top] = false
}

// reverseIndex moves the cursor up one row, scrolling the region down at the top.
func (v *vt) reverseIndex() {
	if v.row == v.top {
		v.scrollDown()
	} else if v.row > 0 {
		v.row--
	}
	v.wrapDefer = false
}

// setSize changes the grid size, reflowing soft-wrapped rows the way an
// emulator does on resize: continuation rows join their logical line and
// re-wrap at the new width, overflow retires to scrollback, and the cursor
// rides its cell. Growing adds blank rows at the bottom; it does not pull rows
// back from scrollback (some emulators do — the renderer must cope with both).
func (v *vt) setSize(w, h int) {
	if w <= 0 || h <= 0 || (w == v.w && h == v.h) {
		return
	}

	// join continuation rows into their logical lines
	var logical [][]rune
	starts := make([]int, v.h) // logical-line index each screen row belongs to
	for i := 0; i < v.h; i++ {
		if v.cont[i] && len(logical) > 0 {
			logical[len(logical)-1] = append(logical[len(logical)-1], v.cells[i]...)
		} else {
			logical = append(logical, slices.Clone(v.cells[i]))
		}
		starts[i] = len(logical) - 1
	}
	// the cursor's column as an offset into its logical line (chunks are full)
	cursorOff := v.col
	for i := v.row; i > 0 && v.cont[i]; i-- {
		cursorOff += v.w
	}
	// trailing blank rows are empty space, not content: reflowing them would
	// push real rows into scrollback
	for len(logical) > 0 && strings.TrimRight(string(logical[len(logical)-1]), " ") == "" {
		logical = logical[:len(logical)-1]
	}

	// re-wrap every logical line at the new width
	var cells [][]rune
	var cont []bool
	lineStart := make([]int, len(logical))
	for li, line := range logical {
		lineStart[li] = len(cells)
		text := []rune(strings.TrimRight(string(line), " "))
		for first := true; ; first = false {
			n := min(len(text), w)
			row := blankRow(w)
			copy(row, text[:n])
			cells = append(cells, row)
			cont = append(cont, !first)
			if n == len(text) {
				break
			}
			text = text[n:]
		}
	}
	newRow, newCol := 0, 0
	if len(logical) > 0 {
		newRow = lineStart[min(starts[v.row], len(logical)-1)] + cursorOff/w
		newCol = min(cursorOff%w, w-1)
	}

	for len(cells) > h {
		v.scrollback = append(v.scrollback, strings.TrimRight(string(cells[0]), " "))
		cells, cont = cells[1:], cont[1:]
		newRow--
	}
	for len(cells) < h {
		cells = append(cells, blankRow(w))
		cont = append(cont, false)
	}
	v.w, v.h, v.cells, v.cont = w, h, cells, cont
	v.row, v.col = max(newRow, 0), newCol
	v.top, v.bot, v.wrapDefer = 0, h-1, false
}

// Line returns the visible row i with trailing blanks removed.
func (v *vt) Line(i int) string {
	if i < 0 || i >= v.h {
		return ""
	}
	return strings.TrimRight(string(v.cells[i]), " ")
}

// Screen returns every visible row joined by newlines.
func (v *vt) Screen() string {
	lines := make([]string, v.h)
	for i := range lines {
		lines[i] = v.Line(i)
	}
	return strings.Join(lines, "\n")
}

func parseParams(s string) []int {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ";")
	out := make([]int, len(parts))
	for i, p := range parts {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

func TestVT(t *testing.T) {
	t.Parallel()

	t.Run("wraps_at_width", func(t *testing.T) {
		v := newVT(4, 3)
		v.WriteString("abcdef")
		assert.Equal(t, "abcd", v.Line(0))
		assert.Equal(t, "ef", v.Line(1))
	})
	t.Run("exact_width_defers_wrap", func(t *testing.T) {
		v := newVT(4, 3)
		v.WriteString("abcd\r\nx")
		assert.Equal(t, "abcd", v.Line(0))
		assert.Equal(t, "x", v.Line(1))
	})
	t.Run("scroll_region_confines_scrolling", func(t *testing.T) {
		v := newVT(4, 4)
		v.WriteString(setRegionSeq(1, 2))
		v.WriteString(cursorTo(4, 1) + "keep")
		v.WriteString(cursorTo(1, 1) + "a\r\nb\r\nc")
		assert.Equal(t, "keep", v.Line(3))
		assert.Equal(t, "b", v.Line(0))
		assert.Equal(t, "c", v.Line(1))
		assert.Equal(t, []string{"a"}, v.scrollback)
	})
	t.Run("erase_line", func(t *testing.T) {
		v := newVT(4, 2)
		v.WriteString("abcd" + cursorTo(1, 1) + eraseLine)
		assert.Empty(t, v.Line(0))
	})
	t.Run("save_restore_cursor", func(t *testing.T) {
		v := newVT(6, 2)
		v.WriteString("ab" + saveCursor + cursorTo(2, 1) + "z" + restoreCursor + "c")
		assert.Equal(t, "abc", v.Line(0))
		assert.Equal(t, "z", v.Line(1))
	})
	t.Run("sgr_ignored", func(t *testing.T) {
		v := newVT(6, 2)
		v.WriteString(sgr(attrBold) + "hi" + sgrReset)
		assert.Equal(t, "hi", v.Line(0))
	})
	t.Run("sgr_keeps_deferred_wrap", func(t *testing.T) {
		v := newVT(4, 3)
		v.WriteString("abcd") // fills the row, wrap deferred
		v.WriteString(sgr(attrBold) + "x")
		assert.Equal(t, "abcd", v.Line(0), "styling does not cancel the pending wrap")
		assert.Equal(t, "x", v.Line(1))
	})
	t.Run("tab_advances_to_next_stop", func(t *testing.T) {
		v := newVT(24, 2)
		v.WriteString("a\tb")
		assert.Equal(t, "a       b", v.Line(0)) // b lands at column 8
	})
	t.Run("tab_clamps_at_margin", func(t *testing.T) {
		v := newVT(9, 2)
		v.WriteString("abcdefgh\t") // already at the last column
		assert.Equal(t, "abcdefgh", v.Line(0))
		assert.Empty(t, v.Line(1), "a tab at the margin neither wraps nor scrolls")
	})
	t.Run("vertical_tab_and_form_feed_feed", func(t *testing.T) {
		v := newVT(8, 4)
		v.WriteString("a\vb\fc") // column rides, like a bare line feed
		assert.Equal(t, "a", v.Line(0))
		assert.Equal(t, " b", v.Line(1))
		assert.Equal(t, "  c", v.Line(2))
	})
	t.Run("ind_feeds_a_line", func(t *testing.T) {
		v := newVT(8, 4)
		v.WriteString("a\x1bDb")
		assert.Equal(t, "a", v.Line(0))
		assert.Equal(t, " b", v.Line(1))
	})
	t.Run("nel_returns_and_feeds", func(t *testing.T) {
		v := newVT(8, 3)
		v.WriteString("ab\x1bEc")
		assert.Equal(t, "ab", v.Line(0))
		assert.Equal(t, "c", v.Line(1))
	})
	t.Run("reverse_index_scrolls_at_top", func(t *testing.T) {
		v := newVT(8, 3)
		v.WriteString("a\r\nb\r\nc")
		v.WriteString(cursorTo(1, 1) + "\x1bMz")
		assert.Equal(t, "z", v.Line(0))
		assert.Equal(t, "a", v.Line(1))
		assert.Equal(t, "b", v.Line(2), "the bottom line retires")
	})
	t.Run("reverse_index_moves_up_below_top", func(t *testing.T) {
		v := newVT(8, 3)
		v.WriteString("a\r\nb\x1bMc")
		assert.Equal(t, "ac", v.Line(0))
	})
	t.Run("vpa_sets_row", func(t *testing.T) {
		v := newVT(8, 4)
		v.WriteString("a\x1b[3db") // row moves, column stays
		assert.Equal(t, "a", v.Line(0))
		assert.Equal(t, " b", v.Line(2))
	})
	t.Run("cha_sets_column", func(t *testing.T) {
		v := newVT(8, 2)
		v.WriteString("a\x1b[4Gb")
		assert.Equal(t, "a  b", v.Line(0))
	})
	t.Run("insert_lines", func(t *testing.T) {
		v := newVT(8, 4)
		v.WriteString("a\r\nb\r\nc\r\nd")
		v.WriteString(cursorTo(1, 1) + csi + "1L")
		assert.Empty(t, v.Line(0))
		assert.Equal(t, "a", v.Line(1))
		assert.Equal(t, "c", v.Line(3), "the bottom line is lost")
	})
	t.Run("delete_lines", func(t *testing.T) {
		v := newVT(8, 4)
		v.WriteString("a\r\nb\r\nc\r\nd")
		v.WriteString(cursorTo(1, 1) + csi + "1M")
		assert.Equal(t, "b", v.Line(0))
		assert.Equal(t, "c", v.Line(1))
		assert.Empty(t, v.Line(3), "a blank appears at the bottom")
	})
	t.Run("scroll_up", func(t *testing.T) {
		v := newVT(8, 3)
		v.WriteString("a\r\nb\r\nc")
		v.WriteString(csi + "1S")
		assert.Equal(t, "b", v.Line(0))
		assert.Equal(t, []string{"a"}, v.scrollback)
	})
	t.Run("scroll_down", func(t *testing.T) {
		v := newVT(8, 3)
		v.WriteString("a\r\nb\r\nc")
		v.WriteString(cursorTo(1, 1) + csi + "1T")
		assert.Empty(t, v.Line(0))
		assert.Equal(t, "a", v.Line(1))
		assert.Equal(t, "b", v.Line(2))
	})
	t.Run("c1_csi_parsed", func(t *testing.T) {
		v := newVT(8, 4)
		v.WriteString("a\u009b2Bb")
		assert.Equal(t, "a", v.Line(0))
		assert.Equal(t, " b", v.Line(2))
	})
	t.Run("del_and_c1_print_nothing", func(t *testing.T) {
		v := newVT(8, 2)
		v.WriteString("a\x7f\u0090b")
		assert.Equal(t, "ab", v.Line(0))
	})
}

func TestVTReflow(t *testing.T) {
	t.Parallel()

	t.Run("narrowing_multiplies_wrapped_rows", func(t *testing.T) {
		v := newVT(10, 4)
		v.WriteString("abcdefghijklmno") // wraps to two rows
		require.Equal(t, "klmno", v.Line(1))

		v.setSize(5, 4)
		assert.Equal(t, "abcde", v.Line(0))
		assert.Equal(t, "fghij", v.Line(1))
		assert.Equal(t, "klmno", v.Line(2))
	})
	t.Run("hard_lines_stay_separate", func(t *testing.T) {
		v := newVT(10, 4)
		v.WriteString("abc\r\ndef")

		v.setSize(2, 4)
		assert.Equal(t, "ab", v.Line(0))
		assert.Equal(t, "c", v.Line(1))
		assert.Equal(t, "de", v.Line(2))
		assert.Equal(t, "f", v.Line(3))
	})
	t.Run("widening_rejoins", func(t *testing.T) {
		v := newVT(5, 4)
		v.WriteString("abcdefghij")
		v.setSize(20, 4)
		assert.Equal(t, "abcdefghij", v.Line(0))
	})
	t.Run("overflow_retires_to_scrollback", func(t *testing.T) {
		v := newVT(10, 2)
		v.WriteString("abcdefghijklmno")
		v.setSize(5, 2) // three rows needed, two fit
		assert.Equal(t, []string{"abcde"}, v.scrollback)
		assert.Equal(t, "fghij", v.Line(0))
		assert.Equal(t, "klmno", v.Line(1))
	})
	t.Run("cursor_rides_its_cell", func(t *testing.T) {
		v := newVT(10, 4)
		v.WriteString("abcdefghijklmno") // cursor on row 1 col 5
		v.setSize(5, 4)
		assert.Equal(t, 3, v.row)
		assert.Equal(t, 0, v.col)
	})
}
