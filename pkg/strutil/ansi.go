package strutil

// ESC starts an escape sequence; BEL terminates an OSC string.
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
// zero value is ready to use; not safe for concurrent use.
type ANSIFilter struct {
	state ansiState
}

// Strip returns chunk with escape sequences removed. A sequence open at the end
// stays suppressed until a later call closes it.
func (f *ANSIFilter) Strip(chunk string) string {
	var out []byte
	for i := 0; i < len(chunk); i++ {
		c := chunk[i]
		switch f.state {
		case ansiGround:
			if c == escByte { // ESC is never text itself
				f.state = ansiEsc
				continue
			}
			out = append(out, c)
		case ansiEsc: // one byte names the sequence form
			switch c {
			case '[':
				f.state = ansiCSI
			case ']':
				f.state = ansiOSC
			case escByte: // a further ESC only restarts the wait for that byte
			default:
				f.state = ansiGround // two-byte escape complete
			}
		case ansiCSI: // CSI ... ends at a final byte in @~
			if c >= '@' && c <= '~' {
				f.state = ansiGround
			}
		case ansiOSC: // OSC ... ends at BEL or ST (ESC \)
			switch c {
			case belByte:
				f.state = ansiGround
			case escByte:
				f.state = ansiOSCEsc
			}
		case ansiOSCEsc: // an OSC ESC closes the string only when followed by \
			switch c {
			case '\\', belByte:
				f.state = ansiGround
			case escByte: // a further ESC keeps waiting for ST
			default:
				f.state = ansiOSC
			}
		}
	}
	return string(out)
}

// ansiState is a filter's position within an escape sequence.
type ansiState int

const (
	ansiGround ansiState = iota // ordinary text
	ansiEsc                     // after ESC, awaiting the byte naming the form
	ansiCSI                     // inside CSI ..., awaiting a final byte @~
	ansiOSC                     // inside OSC ..., awaiting BEL or ST
	ansiOSCEsc                  // inside OSC, after an ESC that may start ST
)
