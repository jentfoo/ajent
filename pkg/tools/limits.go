package tools

import (
	"bytes"
	"io"
	"strings"
	"unicode/utf8"
)

// Limit bounds one tool's output. A zero field means that dimension is
// unbounded; whichever bound is reached first truncates.
type Limit struct {
	Lines int
	Bytes int
}

// Defaults for the built-in tools, expressed as a line count and/or a byte
// count so a single minified file cannot consume a whole budget on either axis.
var (
	BashOutput = Limit{Lines: 4000, Bytes: 32 << 10}  // ~30 kB for the model; the rest spills to disk
	ReadFile   = Limit{Lines: 2000, Bytes: 512 << 20} // bytes effectively unbounded
	FindResult = Limit{Lines: 500}
	GrepResult = Limit{Lines: 1000, Bytes: 128 << 10}
	LsResult   = Limit{Lines: 500}

	// RefInject bounds a single @file injected in full. Above either axis the
	// reference is annotated with its shape instead, so the model can choose to
	// read it with offset/limit rather than paying for the whole file.
	RefInject = Limit{Lines: 500, Bytes: 32 << 10}
	// RefTotal caps the total bytes injected for one message; once reached the
	// remaining references annotate instead, so a message never silently loses one.
	RefTotal = Limit{Bytes: 128 << 10}
)

// Limits is the configurable subset of the built-in tool output bounds. A zero
// field leaves that dimension at its compiled-in default.
type Limits struct {
	Bash      Limit `json:"bash,omitzero"`
	Read      Limit `json:"read,omitzero"`
	Find      Limit `json:"find,omitzero"`
	Grep      Limit `json:"grep,omitzero"`
	Ls        Limit `json:"ls,omitzero"`
	RefInject Limit `json:"refInject,omitzero"`
	RefTotal  Limit `json:"refTotal,omitzero"`
}

// ApplyLimits overwrites the package limits with l's non-zero fields. MeasureCeiling
// and MaxLineChars stay compiled-in.
func ApplyLimits(l Limits) {
	applyLimit(&BashOutput, l.Bash)
	applyLimit(&ReadFile, l.Read)
	applyLimit(&FindResult, l.Find)
	applyLimit(&GrepResult, l.Grep)
	applyLimit(&LsResult, l.Ls)
	applyLimit(&RefInject, l.RefInject)
	applyLimit(&RefTotal, l.RefTotal)
}

// applyLimit folds a configured bound over its package default dimension by dimension.
func applyLimit(dst *Limit, src Limit) {
	if src.Lines > 0 {
		dst.Lines = src.Lines
	}
	if src.Bytes > 0 {
		dst.Bytes = src.Bytes
	}
}

// MeasureCeiling is the byte size above which Measure reports only the byte
// count and never reads the file to count lines, so annotating a giant file is
// itself bounded.
const MeasureCeiling int64 = 512 << 10

// MaxLineChars caps a single line before it is emitted.
const MaxLineChars = 2000

// Elide returns s bounded by l and whether anything was dropped. When truncated,
// content is kept from both ends with an ellipsis marker in the middle so the
// model still sees the head (which usually carries errors) and the tail result.
func Elide(s string, l Limit) (string, bool) {
	if !overBudget(s, l) {
		return s, false
	}
	return elidedText(s, l), true
}

// Writer returns an io.Writer that forwards whole lines to w until either bound
// of l is reached, then diverts the remainder to overflow. A line split across
// writes is buffered so it never splits mid-capture.
func Writer(w io.Writer, l Limit, overflow io.Writer) io.Writer {
	return &boundedWriter{w: w, lim: l, over: overflow}
}

type boundedWriter struct {
	w     io.Writer // kept prefix destination
	lim   Limit
	over  io.Writer // spill destination once a bound is hit
	buf   bytes.Buffer
	count int // bytes forwarded to w
	nl    int // newlines forwarded to w
	full  bool
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		if i := bytes.IndexByte(p, '\n'); i >= 0 {
			b.buf.Write(p[:i+1]) // the newline stays with its line
			p = p[i+1:]
			b.flushLine()
			continue
		}
		// no newline yet: hold a partial line, or spill it once at the limit.
		if b.atLimit() {
			b.spillBytes(p)
		} else if b.lim.Bytes > 0 && b.count+b.buf.Len()+len(p) > b.lim.Bytes {
			// a single overlong partial would blow the byte bound; stop keeping it
			b.full = true
			b.spillBytes(b.buf.Bytes())
			b.buf.Reset()
			b.spillBytes(p)
		} else {
			b.buf.Write(p)
		}
		p = p[len(p):]
	}
	return total, nil
}

// flushLine forwards one completed buffered line to w while a bound allows,
// otherwise marks full and spills it. The buffer is always cleared.
func (b *boundedWriter) flushLine() {
	n := b.buf.Len()
	if !b.full && fitsBytes(b, n) && fitsLines(b) {
		_, _ = b.w.Write(b.buf.Bytes())
		b.count += n
		if bytes.HasSuffix(b.buf.Bytes(), []byte{'\n'}) {
			b.nl++
		}
	} else {
		b.full = true
	}
	b.clearBuf()
}

// Flush writes any trailing partial line still held in the buffer, so an output
// that never ends in a newline is not lost. Call it once reads are drained.
func (b *boundedWriter) Flush() {
	if b.buf.Len() == 0 {
		return
	}
	b.flushLine()
}

// atLimit reports whether the writer is past any bound and should spill.
func (b *boundedWriter) atLimit() bool {
	return b.full || !fitsLines(b) || b.lim.Bytes > 0 && b.count+b.buf.Len() >= b.lim.Bytes
}

// fitsBytes reports whether a line of n bytes still fits the byte bound.
func fitsBytes(b *boundedWriter, n int) bool {
	return b.lim.Bytes == 0 || b.count+n <= b.lim.Bytes
}

// fitsLines reports whether another whole line still fits the line bound.
func fitsLines(b *boundedWriter) bool {
	return b.lim.Lines == 0 || b.nl < b.lim.Lines
}

// clearBuf spills any buffered content once full, then always resets it.
func (b *boundedWriter) clearBuf() {
	if b.over != nil && b.full {
		_, _ = b.over.Write(b.buf.Bytes())
	}
	b.buf.Reset()
}

// spillBytes writes p to overflow when one is attached.
func (b *boundedWriter) spillBytes(p []byte) {
	if b.over != nil {
		_, _ = b.over.Write(p)
	}
}

// overBudget reports whether s exceeds any bound of l or holds an overlong line.
func overBudget(s string, l Limit) bool {
	if l.Bytes > 0 && len(s) > l.Bytes {
		return true
	}
	if l.Lines > 0 && strings.Count(s, "\n")+1 > l.Lines {
		return true
	}
	for _, ln := range strings.Split(s, "\n") {
		if utf8.RuneCountInString(ln) > MaxLineChars {
			return true
		}
	}
	return false
}

const elideMarker = "\n... [truncated]\n"

// elidedText keeps the head and tail of s within l with a marker between. Each
// retained line is capped at MaxLineChars, so one minified file cannot eat the
// whole budget.
func elidedText(s string, l Limit) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	maxKeep := l.Lines
	if maxKeep <= 0 {
		maxKeep = len(lines)
	}
	headAllow, tailAllow := splitBudget(l)

	var head []string
	var hb int
	for i := 0; i < len(lines) && len(head) < halfLines(maxKeep); i++ {
		capped := capLine(lines[i])
		if l.Bytes > 0 && hb+len(capped)+1 > headAllow {
			break
		}
		head = append(head, capped)
		hb += len(capped) + 1
	}

	var tail []string
	var tb int
	for i := len(lines) - 1; i >= 0 && len(tail) < maxKeep-halfLines(maxKeep); i-- {
		capped := capLine(lines[i])
		if l.Bytes > 0 && tb+len(capped)+1 > tailAllow {
			break
		}
		tail = append([]string{capped}, tail...)
		tb += len(capped) + 1
	}

	return strings.Join(head, "\n") + elideMarker + strings.Join(tail, "\n")
}

// maxInt is the largest int, portable across 32- and 64-bit platforms.
const maxInt = int(^uint(0) >> 1)

// splitBudget divides the byte budget between head and tail.
func splitBudget(l Limit) (head, tail int) {
	if l.Bytes <= 0 {
		return maxInt / 2, maxInt / 2 // effectively unbounded; line cap governs
	}
	return l.Bytes / 2, l.Bytes - l.Bytes/2
}

// halfLines returns roughly half of n, at least one.
func halfLines(n int) int {
	if n <= 0 {
		return 1
	}
	h := (n + 1) / 2
	if h < 1 {
		return 1
	}
	return h
}

// capLine truncates s to MaxLineChars on a rune boundary.
func capLine(s string) string {
	n := utf8.RuneCountInString(s)
	if n <= MaxLineChars {
		return s
	}
	var out strings.Builder
	for i, r := range s {
		if i >= MaxLineChars {
			break
		}
		out.WriteRune(r)
	}
	return out.String()
}

// countLines returns the number of lines in s, counting a trailing partial line.
func countLines(s string) int {
	return strings.Count(s, "\n") + 1
}
