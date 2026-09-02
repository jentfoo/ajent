package tools

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/jentfoo/ajent/pkg/strutil"
)

// Limit bounds one tool's output. A zero field means that dimension is
// unbounded; whichever bound is reached first truncates.
type Limit struct {
	Lines int
	Bytes int
}

// Package-level output limits are set once by ApplyLimits at startup, but tests
// run tools concurrently with a reconfiguration, so reads and writes share a lock.
var (
	limitsMu   sync.RWMutex                           // guards every bound below together
	bashOutput = Limit{Lines: 4000, Bytes: 32 << 10}  // ~30 kB for the model; the rest spills to disk
	readFile   = Limit{Lines: 2000, Bytes: 512 << 20} // bytes effectively unbounded
	findResult = Limit{Lines: 500}
	grepResult = Limit{Lines: 1000, Bytes: 128 << 10}
	lsResult   = Limit{Lines: 500}
	// refInject bounds a single @file injected in full; above either axis the
	// reference is annotated with its shape instead so the model reads it explicitly.
	refInject = Limit{Lines: 500, Bytes: 128 << 10}
	// refTotal caps total bytes for one message's references; once reached the rest
	// annotate rather than silently dropping a file.
	refTotal = Limit{Bytes: 128 << 10}
)

// BashLimit returns the bash output bound.
func BashLimit() Limit { return limitRead(&bashOutput) }

// ReadFileLimit returns the read tool's per-file bound.
func ReadFileLimit() Limit { return limitRead(&readFile) }

// FindResultLimit returns the find result bound.
func FindResultLimit() Limit { return limitRead(&findResult) }

// GrepResultLimit returns the grep output bound.
func GrepResultLimit() Limit { return limitRead(&grepResult) }

// LsResultLimit returns the ls result bound.
func LsResultLimit() Limit { return limitRead(&lsResult) }

// RefInjectLimit bounds a single @file injected in full; above either axis the
// reference is annotated with its shape instead.
func RefInjectLimit() Limit { return limitRead(&refInject) }

// RefTotalLimit caps total bytes injected for one message; once reached the rest
// annotate so no reference is silently lost.
func RefTotalLimit() Limit { return limitRead(&refTotal) }

// limitRead copies a bound under the read lock, returning an immutable snapshot.
func limitRead(dst *Limit) Limit {
	limitsMu.RLock()
	defer limitsMu.RUnlock()
	return *dst
}

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
// and MaxLineRunes stay compiled-in.
func ApplyLimits(l Limits) {
	limitsMu.Lock()
	defer limitsMu.Unlock()
	applyLimit(&bashOutput, l.Bash)
	applyLimit(&readFile, l.Read)
	applyLimit(&findResult, l.Find)
	applyLimit(&grepResult, l.Grep)
	applyLimit(&lsResult, l.Ls)
	applyLimit(&refInject, l.RefInject)
	applyLimit(&refTotal, l.RefTotal)
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

// MaxLineRunes caps a single line before it is emitted. Counted in runes so an
// all-CJK minified file cannot eat the whole budget either.
const MaxLineRunes = 1024

// Bounded describes one truncation: the kept head plus totals for the footer.
type Bounded struct {
	Text      string // kept head, whole lines where possible
	Truncated bool
	Shown     int // lines emitted
	Lines     int // total lines in the source
	Bytes     int // total bytes in the source
}

// Bound returns s bounded by l: the leading whole lines that fit both bounds,
// each capped at MaxLineRunes, or a single first line cut at MaxLineRunes when no
// complete line fits. One overlong line alone also counts as truncated, so the
// caller spills the full text and the footer names what was dropped.
func Bound(s string, l Limit) Bounded {
	b := Bounded{Bytes: len(s), Lines: countLines(s)}
	if !overBudget(s, l) && !overlongLine(s) {
		b.Text = s
		b.Shown = b.Lines
		return b
	}

	var kept []string
	var byteCount int // bytes of kept text including each joining newline
	for _, ln := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if l.Lines > 0 && len(kept) >= l.Lines {
			break
		}
		capped := capLine(ln)
		nb := byteCount + len(capped)
		if l.Bytes > 0 && nb > l.Bytes {
			break // stop at the last whole line that fits
		}
		byteCount = nb + 1
		kept = append(kept, capped)
	}

	b.Truncated = true
	if len(kept) == 0 { // no complete line fit; cut the first alone on the rune
		// budget alone — a byte-bound cut of one overlong line (a minified file)
		// would leave the model a useless sliver, so MaxLineRunes is a floor here
		kept = []string{capLine(strutil.FirstLine(s))}
	}
	b.Shown = len(kept)
	b.Text = strings.Join(kept, "\n")
	return b
}

// truncationNote names what Bound or a bounded writer dropped: shown/total lines,
// total bytes, and where the rest lives when spilled to disk.
func truncationNote(b Bounded, spillPath string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "... truncated: %d/%d lines shown (%d bytes total)", b.Shown, b.Lines, b.Bytes)
	if spillPath != "" {
		sb.WriteString("; full output in @" + spillPath)
	}
	return sb.String()
}

// overlongLine reports whether any line of s exceeds MaxLineRunes, so even an
// in-budget result cannot carry one minified line whole.
func overlongLine(s string) bool {
	for ln := range strings.Lines(s) {
		if utf8.RuneCountInString(strings.TrimSuffix(ln, "\n")) > MaxLineRunes {
			return true
		}
	}
	return false
}

// capText caps every line of s at MaxLineRunes, without markers: used on a
// truncated head whose full stream is spilled, so the model can recover the rest.
func capText(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = capLine(lines[i])
	}
	return strings.Join(lines, "\n")
}

// Elide returns s bounded by l and whether anything was dropped. When truncated,
// content is kept from both ends with an ellipsis marker so the model still sees
// the head (which usually carries errors) and the tail result. Text within the
// bound is returned whole and uncapped; only retained lines of a truncated
// result are capped at MaxLineRunes, so one minified line cannot eat the budget.
// Head+tail survival is for compaction's structural reduction only — tool
// output uses Bound.
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

	replay bytes.Buffer // copy of everything forwarded, for the complete-spill claim
	totB   int          // total input bytes seen across all writes
	totNL  int          // total newlines seen across all writes
	tail   bool         // input seen since the last newline (a partial final line)
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	total := len(p)
	b.totB += total
	for len(p) > 0 {
		if i := bytes.IndexByte(p, '\n'); i >= 0 {
			b.totNL++
			b.tail = false
			b.buf.Write(p[:i+1]) // the newline stays with its line
			p = p[i+1:]
			b.flushLine()
			continue
		}
		// no newline yet: hold a partial line, or spill it once at the limit.
		b.tail = true
		if b.atLimit() || b.lim.Bytes > 0 && b.count+b.buf.Len()+len(p) > b.lim.Bytes {
			// already at a bound, or this partial would blow the byte bound: spill
			// the held prefix first so the overflow file keeps stream order
			b.beginSpill()
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
		b.replay.Write(b.buf.Bytes()) // keep the complete-spill copy
		b.count += n
		if bytes.HasSuffix(b.buf.Bytes(), []byte{'\n'}) {
			b.nl++
		}
	} else {
		b.beginSpill()
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

// Total returns the lines and bytes seen across all input, forwarded or spilled,
// so a footer can name what was dropped without under-counting.
func (b *boundedWriter) Total() (lines, bytes int) {
	lines = b.totNL
	if b.tail {
		lines++ // a stream not ending in a newline has one partial line more
	}
	return lines, b.totB
}

// Truncated reports whether any input was diverted past the bounds.
func (b *boundedWriter) Truncated() bool { return b.full }

// beginSpill marks the writer full once and writes the kept head into over so
// the spill file holds the complete stream, not just the overflow. Truncation
// is reported even without an overflow writer, whose content is then dropped.
func (b *boundedWriter) beginSpill() {
	if b.full {
		return
	}
	b.full = true
	if b.over != nil && b.replay.Len() > 0 {
		_, _ = b.over.Write(b.replay.Bytes())
	}
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

// overBudget reports whether s exceeds any bound of l.
func overBudget(s string, l Limit) bool {
	if l.Bytes > 0 && len(s) > l.Bytes {
		return true
	}
	return l.Lines > 0 && countLines(s) > l.Lines
}

const elideMarker = "\n... [truncated]\n"

// elidedText keeps the head and tail of s within l with a marker between; each
// retained line is capped so one overlong line cannot consume a whole allowance.
func elidedText(s string, l Limit) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	maxKeep := l.Lines
	if maxKeep <= 0 || maxKeep > len(lines) {
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

// capLine truncates s to MaxLineRunes on a rune boundary.
func capLine(s string) string {
	rs := []rune(s)
	if len(rs) <= MaxLineRunes {
		return s
	}
	return string(rs[:MaxLineRunes])
}

// countLines returns the number of lines in s, not counting the empty string a
// trailing newline leaves behind.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++ // a trailing partial line
	}
	return n
}
