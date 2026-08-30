package tui

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"time"
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
	keyStatusReport // DSR reply (CSI 0 n): the terminal has caught up
	keyColorReport  // OSC 11 reply: the terminal's default background color
	keyDeviceAttrs  // DA1 reply (CSI ... c), used only to fence an earlier query
	keyEscape
	keyTab
	keyBackTab
	keyReverseSearch
	keyAltUp // Alt+↑ recalls the newest queued prompt into the editor (ControlRecallQueued)
)

// key is one decoded input event.
type key struct {
	typ     keyType
	text    string // literal text for keyRune and keyPaste, payload for keyColorReport
	row     int    // reported cursor row for keyCursorReport
	partial bool   // keyPaste delivered at maxPasteLen; its tail is still arriving
}

const (
	readChunk     = 1024 // bytes read from the source per call
	escByte       = 0x1b
	belByte       = 0x07
	maxControlLen = 1024 // a CSI longer than this without a final byte is dropped as truncated
)

// maxPasteLen bounds an unterminated paste; on reaching it the body so far is
// delivered rather than held forever. Far above tcell's cap because applyKey
// turns anything over pasteThreshold into a placeholder, so large pastes are supported.
const maxPasteLen = 4 << 20

var (
	pasteStartBytes = []byte(pasteStart)
	pasteEndBytes   = []byte(pasteEnd)
)

// escTimeout is how long a lone escape byte is held before it is reported as
// keyEscape rather than the start of a longer sequence.
const escTimeout = 50 * time.Millisecond

// escTimer holds a lone escape byte until it is certain no sequence follows. The
// interface lets tests drive the timeout paths deterministically instead of by
// wall clock.
type escTimer interface {
	Reset(d time.Duration) bool
	Stop() bool
	C() <-chan time.Time
}

// realEsc wraps *time.Timer as an escTimer, arming lazily so a never-fired
// timer needs no initial drain.
type realEsc struct{ t *time.Timer }

func newRealEsc() escTimer {
	return &realEsc{t: time.NewTimer(time.Hour)}
}

func (e *realEsc) Reset(d time.Duration) bool {
	if !e.t.Stop() {
		select {
		case <-e.t.C:
		default:
		}
	}
	return e.t.Reset(d)
}

func (e *realEsc) Stop() bool { return e.t.Stop() }

func (e *realEsc) C() <-chan time.Time { return e.t.C }

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
	0x12: keyReverseSearch, // Ctrl+R
	0x15: keyKillLine,
	0x17: keyKillWord,
	0x1a: keySuspend,
	0x7f: keyBackspace,
	0x09: keyTab,
}

// decodeKey decodes the first key in b. ok is false when b holds only part of a
// sequence and more input is needed.
func decodeKey(b []byte) (key, int, bool) { return decodeKeyFrom(b, 0) }

// decodeKeyFrom is decodeKey plus a hint: pasteFrom bytes at the start of an
// in-progress paste body are already known to hold no terminator. Still pure:
// the offset is just another input.
func decodeKeyFrom(b []byte, pasteFrom int) (k key, n int, ok bool) {
	if len(b) == 0 {
		return key{}, 0, false
	} else if b[0] == escByte {
		return decodeEscape(b, pasteFrom)
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

func decodeEscape(b []byte, pasteFrom int) (key, int, bool) {
	if len(b) < 2 {
		return key{}, 0, false
	}
	switch b[1] {
	case '[':
		return decodeCSI(b, pasteFrom)
	case ']':
		return decodeOSC(b)
	case 'O':
		if len(b) < 3 {
			return key{}, 0, false // SS3 is a fixed three bytes
		}
		if b[2] == escByte {
			return key{typ: keyIgnore}, 2, true // ESC aborts; leave it for the next decode
		}
		return key{typ: ss3Key(b[2])}, 3, true
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

func decodeCSI(b []byte, pasteFrom int) (key, int, bool) {
	i := 2
	for i < len(b) && b[i] != escByte && (b[i] < 0x40 || b[i] > 0x7e) {
		if i >= maxControlLen {
			return key{typ: keyIgnore}, i, true // cap reached; resync at the next byte
		}
		i++
	}
	if i >= len(b) {
		return key{}, 0, false // incomplete: wait for the final byte
	}
	if b[i] == escByte {
		return key{typ: keyIgnore}, i, true // ESC aborts; resync at the next byte
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
	case 'c':
		return key{typ: keyDeviceAttrs}, n, true
	case 'n':
		if params == "0" { // DSR "no malfunction": answers statusQuery
			return key{typ: keyStatusReport}, n, true
		}
		return key{typ: keyIgnore}, n, true
	case '~':
		return decodeTilde(b, params, n, pasteFrom)
	case 'Z':
		if field, _, _ := strings.Cut(params, ";"); field == "" || field == "1" {
			return key{typ: keyBackTab}, n, true
		}
		return key{typ: keyIgnore}, n, true
	default:
		return key{typ: keyIgnore}, n, true
	}
}

// decodeOSC decodes an OSC reply, terminated by BEL or ST. Only the OSC 11
// background answer is reported; every other OSC is swallowed so a reply to a
// query we never made cannot leak into the editor as runes.
func decodeOSC(b []byte) (key, int, bool) {
	for i := 2; i < len(b); i++ {
		switch {
		case b[i] == belByte:
			return oscKey(string(b[2:i])), i + 1, true
		case b[i] == escByte:
			if i+1 >= len(b) {
				return key{}, 0, false // ST may still be forming
			} else if b[i+1] != '\\' {
				return key{typ: keyIgnore}, i, true // ESC aborts; resync at that byte
			}
			return oscKey(string(b[2:i])), i + 2, true
		case i >= maxControlLen:
			return key{typ: keyIgnore}, i, true // cap reached; resync at the next byte
		}
	}
	return key{}, 0, false // incomplete: wait for the terminator
}

// oscKey returns the color report an OSC 11 payload carries, else keyIgnore.
func oscKey(payload string) key {
	if spec, ok := strings.CutPrefix(payload, "11;"); ok {
		return key{typ: keyColorReport, text: spec}
	}
	return key{typ: keyIgnore}
}

func decodeTilde(b []byte, params string, n int, pasteFrom int) (key, int, bool) {
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
		pasteFrom = min(max(pasteFrom, 0), max(len(b)-n, 0)) // clamp an out-of-range hint
		end := bytes.Index(b[n+pasteFrom:], pasteEndBytes)
		if end < 0 {
			if len(b)-n >= maxPasteLen {
				// deliver the capped body; partial tells the reader its tail is still in flight
				return key{typ: keyPaste, text: string(b[n:]), partial: true}, len(b), true
			}
			return key{}, 0, false // wait for more of the body
		}
		termAt := n + pasteFrom + end
		return key{typ: keyPaste, text: string(b[n:termAt])}, termAt + len(pasteEnd), true
	default:
		return key{typ: keyIgnore}, n, true
	}
}

// modifier bits in tcell's CSI parameter encoding (the value is 1 + bitmask).
const (
	modShift = 1 << iota // shift: never promotes word movement
	modAlt               // alt: Alt+↑ recalls, and with ctrl promotes to word movement
	modCtrl              // ctrl: promotes arrows to word movement
	modMeta              // meta: modeled but unused here
)

// csiModifier parses the second field of a CSI params string into tcell's raw
// modifier bitmask (the value minus its leading 1), masked to the modeled bits so
// a stray high value cannot alias into Ctrl or Alt.
func csiModifier(params string) int {
	_, rest, _ := strings.Cut(params, ";")
	if rest == "" {
		return 0 // no modifier field: plain key
	}
	field, _, _ := strings.Cut(rest, ";")   // drop any fields after the modifier
	modStr, _, _ := strings.Cut(field, ":") // drop sub-parameters (CSI u)
	v, err := strconv.Atoi(modStr)
	if err != nil || v < 2 {
		return 0 // malformed or unmodified
	}
	const modeled = modShift | modAlt | modCtrl | modMeta
	return (v - 1) & modeled
}

// ss3Key maps an SS3 final byte to a key. Only the safe subset is mapped: tcell
// would give PC-keypad navigation for p-y, but we never emit DECKPAM so they are
// unreachable here, and mapping them could turn an inert key into a wrong action.
func ss3Key(final byte) keyType {
	switch final {
	case 'A', 'B', 'C', 'D':
		return arrowKey(final, "")
	case 'H':
		return keyHome
	case 'F':
		return keyEnd
	case 'M':
		return keyEnter // keypad enter
	default:
		return keyIgnore
	}
}

// arrowKey maps a final byte to a movement, promoting to word movement when the
// parameters carry a ctrl or alt modifier.
func arrowKey(final byte, params string) keyType {
	switch final {
	case 'A':
		if csiModifier(params)&modAlt != 0 { // Alt+↑ recalls the newest queued prompt
			return keyAltUp
		}
		return keyUp
	case 'B':
		return keyDown
	case 'C', 'D':
		if csiModifier(params)&(modCtrl|modAlt) != 0 {
			if final == 'C' {
				return keyWordRight
			}
			return keyWordLeft
		}
		if final == 'C' {
			return keyRight
		}
		return keyLeft
	default:
		return keyIgnore
	}
}

// inputReader decodes keys from a terminal in raw mode. Cursor position and
// status reports are delivered separately so a query cannot swallow a
// keystroke.
type inputReader struct {
	src     io.Reader
	keys    chan key
	reports chan int      // never closed: cursorRow polls it, so it needs no end signal
	status  chan struct{} // DSR replies answering the resize barrier probe
	colors  chan string   // OSC 11 background answers
	attrs   chan struct{} // DA1 replies, the fence behind an OSC 11 query

	newTimer func() escTimer // defaults to newRealEsc, tests inject a manual one
	escDelay time.Duration   // how long a lone escape byte is held, seeded from escTimeout
}

func newInputReader(src io.Reader) *inputReader {
	return &inputReader{
		src:      src,
		keys:     make(chan key, 64),
		reports:  make(chan int, 4),
		status:   make(chan struct{}, 4),
		colors:   make(chan string, 1),
		attrs:    make(chan struct{}, 1),
		newTimer: newRealEsc,
		escDelay: escTimeout,
	}
}

// dropPasteTail discards the remainder of a capped paste body, reporting whether
// its terminator was reached. The bytes that could still begin one are kept.
func dropPasteTail(buf []byte) ([]byte, bool) {
	if i := bytes.Index(buf, pasteEndBytes); i >= 0 {
		return buf[i+len(pasteEndBytes):], true
	}
	return buf[max(len(buf)-len(pasteEnd)+1, 0):], false
}

// readResult is one chunk from the source, or the failure that ended it.
type readResult struct {
	data []byte
	err  error
}

// run decodes until the source ends, then closes keys. A genuine Ctrl+D byte
// (0x04) is decoded as keyEOF; a closed input stream emits no keystroke: readers
// stop on the channel close alone, so an external EOF never races with typed
// input as an editing key.
//
// A lone escape byte is indistinguishable from the start of a longer sequence
// until either more bytes arrive or enough time passes, so it is held for
// escTimeout before being reported as keyEscape.
func (r *inputReader) run() {
	defer close(r.keys)
	defer close(r.status)
	defer close(r.colors)
	defer close(r.attrs)

	reads := make(chan readResult, 1)
	go func() {
		for {
			chunk := make([]byte, readChunk)
			n, err := r.src.Read(chunk)
			reads <- readResult{data: chunk[:n], err: err}
			if err != nil {
				return
			}
		}
	}()

	var buf []byte
	var pasteScanned int   // bytes of an in-progress paste body already known to hold no terminator
	var pasteOverflow bool // a capped paste was delivered; its tail is dropped up to the terminator
	timer := r.newTimer()

	for {
		for {
			if pasteOverflow {
				var done bool
				buf, done = dropPasteTail(buf)
				if !done {
					break // still inside the capped body; nothing here decodes as a key
				}
				pasteOverflow = false
			}
			k, n, ok := decodeKeyFrom(buf, pasteScanned)
			if !ok {
				break
			}
			buf = buf[n:]
			pasteScanned = 0 // any in-progress paste resolved (or was delivered at cap)
			pasteOverflow = k.partial
			r.emit(k)
		}
		if len(buf) == 0 {
			buf = nil // drop the consumed backing array
		} else if bytes.HasPrefix(buf, pasteStartBytes) {
			// no terminator yet: only the last len(pasteEnd)-1 body bytes can still begin one
			pasteScanned = max(len(buf)-len(pasteStart)-len(pasteEnd)+1, 0)
		}

		var pendingEsc <-chan time.Time
		// not armed inside a capped paste either: the held bytes may be a split terminator
		if len(buf) > 0 && buf[0] == escByte && !pasteOverflow && !bytes.HasPrefix(buf, pasteStartBytes) {
			timer.Reset(r.escDelay)
			pendingEsc = timer.C()
		}

		select {
		case res := <-reads:
			if pendingEsc != nil && !timer.Stop() {
				<-timer.C()
			}
			buf = append(buf, res.data...)
			if res.err != nil {
				r.flush(buf) // deliver undecoded runes read up to the close
				return       // defer closes keys; stream end emits no editing keystroke
			}
		case <-pendingEsc:
			if len(buf) == 1 {
				r.keys <- key{typ: keyEscape} // a genuine lone Esc
			}
			buf = nil // truncated sequence: drop it rather than leak its bytes as runes
			pasteScanned = 0
		}
	}
}

// emit routes a decoded key to the right consumer.
func (r *inputReader) emit(k key) {
	switch k.typ {
	case keyCursorReport:
		sendLatest(r.reports, k.row)
	case keyStatusReport:
		select {
		case r.status <- struct{}{}:
		default:
		}
	case keyColorReport:
		select {
		case r.colors <- k.text:
		default:
		}
	case keyDeviceAttrs:
		select {
		case r.attrs <- struct{}{}:
		default:
		}
	case keyIgnore:
	default:
		r.keys <- k
	}
}

// sendLatest leaves ch holding the newest value, dropping an older one to make
// room: a cursor report is a position, and only the last one is true. Bounded
// on purpose (one drain, one retry) so the reader never spins here.
//
// Single writer only. emit runs on run's goroutine alone, so nothing can fill
// the slot between the drain and the retry; two senders racing here could drop
// the newer value instead. A second writer would need a lock of its own.
func sendLatest(ch chan int, v int) {
	select {
	case ch <- v:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- v:
	default:
	}
}

// flush reports a trailing lone escape that the source ended on.
func (r *inputReader) flush(buf []byte) {
	if len(buf) == 1 && buf[0] == escByte {
		r.keys <- key{typ: keyEscape}
	}
}
