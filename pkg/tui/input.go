package tui

import (
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

type keyType int

const (
	keyIgnore keyType = iota
	keyRune
	keyPaste
	keyEnter
	keyNewline
	keyBackspace
	keyDelete
	keyLeft
	keyRight
	keyUp
	keyDown
	keyWordLeft
	keyWordRight
	keyHome
	keyEnd
	keyKillToEnd
	keyKillLine
	keyKillWord
	keyInterrupt
	keyEOF
	keyRedraw
	keySuspend
	keyPageUp
	keyPageDown
	keyCursorReport
)

// key is one decoded input event.
type key struct {
	typ  keyType
	text string // literal text for keyRune and keyPaste
	row  int    // reported cursor row for keyCursorReport
}

const readChunk = 1024

// controlKeys maps single control bytes to their editing action.
var controlKeys = map[byte]keyType{
	0x01: keyHome,
	0x02: keyLeft,
	0x03: keyInterrupt,
	0x04: keyEOF,
	0x05: keyEnd,
	0x06: keyRight,
	0x08: keyBackspace,
	0x0a: keyNewline,
	0x0b: keyKillToEnd,
	0x0c: keyRedraw,
	0x0d: keyEnter,
	0x0e: keyDown,
	0x10: keyUp,
	0x15: keyKillLine,
	0x17: keyKillWord,
	0x1a: keySuspend,
	0x7f: keyBackspace,
}

// decodeKey decodes the first key in b. ok is false when b holds only part of a
// sequence and more input is needed.
func decodeKey(b []byte) (k key, n int, ok bool) {
	if len(b) == 0 {
		return key{}, 0, false
	} else if b[0] == 0x1b {
		return decodeEscape(b)
	} else if t, found := controlKeys[b[0]]; found {
		return key{typ: t}, 1, true
	} else if b[0] < 0x20 {
		return key{typ: keyIgnore}, 1, true
	}
	r, size := utf8.DecodeRune(b)
	if r == utf8.RuneError && size <= 1 && !utf8.FullRune(b) {
		return key{}, 0, false // partial multi byte rune
	}
	return key{typ: keyRune, text: string(b[:size])}, size, true
}

func decodeEscape(b []byte) (key, int, bool) {
	if len(b) < 2 {
		return key{}, 0, false
	}
	switch b[1] {
	case '[':
		return decodeCSI(b)
	case 'O':
		if len(b) < 3 {
			return key{}, 0, false
		}
		return key{typ: arrowKey(b[2], "")}, 3, true
	case 0x0d, 0x0a:
		return key{typ: keyNewline}, 2, true // alt+enter
	case 'b':
		return key{typ: keyWordLeft}, 2, true
	case 'f':
		return key{typ: keyWordRight}, 2, true
	case 0x7f:
		return key{typ: keyKillWord}, 2, true
	default:
		return key{typ: keyIgnore}, 2, true
	}
}

func decodeCSI(b []byte) (key, int, bool) {
	i := 2
	for i < len(b) && (b[i] < 0x40 || b[i] > 0x7e) {
		i++
	}
	if i >= len(b) {
		return key{}, 0, false
	}
	params, final, n := string(b[2:i]), b[i], i+1

	switch final {
	case 'A', 'B', 'C', 'D':
		return key{typ: arrowKey(final, params)}, n, true
	case 'H':
		return key{typ: keyHome}, n, true
	case 'F':
		return key{typ: keyEnd}, n, true
	case 'R':
		row, _, _ := strings.Cut(params, ";")
		v, err := strconv.Atoi(row)
		if err != nil {
			return key{typ: keyIgnore}, n, true
		}
		return key{typ: keyCursorReport, row: v}, n, true
	case '~':
		return decodeTilde(b, params, n)
	default:
		return key{typ: keyIgnore}, n, true
	}
}

func decodeTilde(b []byte, params string, n int) (key, int, bool) {
	code, _, _ := strings.Cut(params, ";")
	switch code {
	case "1", "7":
		return key{typ: keyHome}, n, true
	case "3":
		return key{typ: keyDelete}, n, true
	case "4", "8":
		return key{typ: keyEnd}, n, true
	case "5":
		return key{typ: keyPageUp}, n, true
	case "6":
		return key{typ: keyPageDown}, n, true
	case "200":
		end := strings.Index(string(b[n:]), pasteEnd)
		if end < 0 {
			return key{}, 0, false // wait for the whole paste
		}
		return key{typ: keyPaste, text: string(b[n : n+end])}, n + end + len(pasteEnd), true
	default:
		return key{typ: keyIgnore}, n, true
	}
}

// arrowKey maps a final byte to a movement, promoting to word movement when the
// parameters carry a ctrl or alt modifier.
func arrowKey(final byte, params string) keyType {
	word := strings.HasSuffix(params, ";5") || strings.HasSuffix(params, ";3")
	switch final {
	case 'A':
		return keyUp
	case 'B':
		return keyDown
	case 'C':
		if word {
			return keyWordRight
		}
		return keyRight
	case 'D':
		if word {
			return keyWordLeft
		}
		return keyLeft
	default:
		return keyIgnore
	}
}

// inputReader decodes keys from a terminal in raw mode. Cursor position reports
// are delivered separately so a startup query cannot swallow a keystroke.
type inputReader struct {
	src     io.Reader
	keys    chan key
	reports chan int
}

func newInputReader(src io.Reader) *inputReader {
	return &inputReader{
		src:     src,
		keys:    make(chan key, 64),
		reports: make(chan int, 4),
	}
}

// run decodes until the source ends, then emits keyEOF and closes keys.
func (r *inputReader) run() {
	defer close(r.keys)

	var buf []byte
	chunk := make([]byte, readChunk)
	for {
		for {
			k, n, ok := decodeKey(buf)
			if !ok {
				break
			}
			buf = buf[n:]
			if k.typ == keyCursorReport {
				select {
				case r.reports <- k.row:
				default:
				}
			} else if k.typ != keyIgnore {
				r.keys <- k
			}
		}
		if len(buf) == 0 {
			buf = nil // drop the consumed backing array
		}

		n, err := r.src.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if err != nil {
			r.keys <- key{typ: keyEOF}
			return
		}
	}
}
