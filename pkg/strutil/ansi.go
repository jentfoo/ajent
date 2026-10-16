package strutil

import (
	"strings"
)

// ESC starts an escape sequence, BEL terminates a string form early.
const (
	escByte = 0x1b
	belByte = 0x07
)

// StripANSI removes ANSI escape sequences from s. Stateless: feed chunked
// streams to ANSIFilter instead.
func StripANSI(s string) string {
	var f ANSIFilter
	return f.Strip(s)
}

// ANSIFilter strips ANSI escape sequences from concatenated chunks, carrying its
// position between calls so a sequence split at a chunk boundary cannot leak. The
// zero value is ready to use, not safe for concurrent use.
type ANSIFilter struct {
	state ansiState
}

// Strip returns chunk with escape sequences removed. A sequence open at the end
// stays suppressed until a later call closes it.
func (f *ANSIFilter) Strip(chunk string) string {
	var b strings.Builder
	b.Grow(len(chunk)) // escapes are rare, so the output is nearly chunk-sized
	for i := 0; i < len(chunk); i++ {
		c := chunk[i]
		switch f.state {
		case ansiGround:
			if c == escByte { // ESC is never text itself
				f.state = ansiEsc
				continue
			}
			b.WriteByte(c)
		case ansiEsc: // one byte names the sequence form
			switch c {
			case '[':
				f.state = ansiCSI
			case ']', 'P', 'X', '^', '_': // string forms: OSC DCS SOS PM APC
				f.state = ansiStr
			case escByte: // a further ESC only restarts the wait for that byte
			default:
				f.state = ansiGround // two-byte escape complete
			}
		case ansiCSI: // CSI ... ends at a final byte in @~
			if c >= '@' && c <= '~' {
				f.state = ansiGround
			}
		case ansiStr: // string forms end at BEL or ST (ESC \)
			switch c {
			case belByte:
				f.state = ansiGround
			case escByte:
				f.state = ansiStrEsc
			}
		case ansiStrEsc: // a string's ESC closes it only when followed by \
			switch c {
			case '\\', belByte:
				f.state = ansiGround
			case escByte: // a further ESC keeps waiting for ST
			default:
				f.state = ansiStr
			}
		}
	}
	return b.String()
}

// ansiState is a filter's position within an escape sequence.
type ansiState int

const (
	ansiGround ansiState = iota // ordinary text
	ansiEsc                     // after ESC, awaiting the byte naming the form
	ansiCSI                     // inside CSI ..., awaiting a final byte @~
	ansiStr                     // inside OSC/DCS/SOS/PM/APC ..., awaiting BEL or ST
	ansiStrEsc                  // inside a string form, after an ESC that may start ST
)
