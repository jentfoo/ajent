package tui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// vt is a minimal terminal emulator covering the sequences this package emits,
// plus a few it no longer does. It exists so layout and scrolling can be asserted
// on a rendered screen rather than on raw escape bytes.
type vt struct {
	w, h       int
	cells      [][]rune
	row, col   int // zero based
	top, bot   int // scroll region, zero based inclusive
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
	v := &vt{w: w, h: h, bot: h - 1}
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
		case '\r':
			v.col, v.wrapDefer = 0, false
		case '\n':
			v.lineFeed()
			v.wrapDefer = false
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
	v.wrapDefer = false
	switch final {
	case 'H', 'f':
		v.row = min(max(arg(0, 1)-1, 0), v.h-1)
		v.col = min(max(arg(1, 1)-1, 0), v.w-1)
	case 'A':
		v.row = max(v.row-arg(0, 1), 0)
	case 'B':
		v.row = min(v.row+arg(0, 1), v.h-1)
	case 'C':
		v.col = min(v.col+arg(0, 1), v.w-1)
	case 'D':
		v.col = max(v.col-arg(0, 1), 0)
	case 'K':
		v.eraseLine(arg(0, 0))
	case 'J':
		v.eraseDisplay(arg(0, 0))
	case 'r':
		v.top = min(max(arg(0, 1)-1, 0), v.h-1)
		v.bot = min(max(arg(1, v.h)-1, 0), v.h-1)
		v.row, v.col = 0, 0
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
	if r < 0x20 {
		return
	} else if v.wrapDefer {
		v.col = 0
		v.lineFeed()
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
	v.cells[v.bot] = blankRow(v.w)
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
}
