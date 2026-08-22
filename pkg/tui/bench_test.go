package tui

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// countWriter counts bytes written, so a benchmark can report wire bytes.
type countWriter struct{ n int }

func (w *countWriter) Write(p []byte) (int, error) {
	w.n += len(p)
	return len(p), nil
}

// benchBlock is a typical live block: divider, three editor rows, an activity
// row and the status row, at a common terminal size.
func benchBlock() []string {
	return []string{
		strings.Repeat(ruleChar, 119),
		"❯ a fairly long draft line of input text that keeps going",
		"  continuation of the draft, indented under the prompt",
		"sub-1  investigating the render path for the live block",
		"⠋ write notes.go · ▓▓▓░░░░░░░░ ~68.2k/200k · opus-5 · permissions: allow-read",
	}
}

// BenchmarkUIRepaint measures the full compose: status, activity and a
// multi-line editor at a typical terminal size.
func BenchmarkUIRepaint(b *testing.B) {
	v := newVT(120, 40)
	u := newTestUI(b, v, strings.NewReader(""))
	u.SetStatusSegment(Segment{Key: "perm", Text: "permissions: allow-read", Short: "read"})
	u.SetActivity("call:1", "write notes.go 12.4kb")
	u.SetActivity("agent-1", "investigating pkg/tui render path")
	u.mu.Lock()
	u.editor.Insert(strings.Repeat("line of draft text\n", 8))
	u.mu.Unlock()

	b.ResetTimer()
	for b.Loop() {
		u.mu.Lock()
		u.repaint()
		u.mu.Unlock()
	}
}

// BenchmarkDrawLive measures the wire bytes a frame costs, since wire bytes
// are what the terminal pays for. Cases: an idle spinner tick that changes
// only the status row, a keystroke that moves the caret within its row, and a
// full block change.
func BenchmarkDrawLive(b *testing.B) {
	base := benchBlock()

	b.Run("spinner_tick", func(b *testing.B) {
		w := &countWriter{}
		r := &inlineRenderer{t: &termState{out: w, fd: -1, width: 120, height: 40}}
		r.setLive(slices.Clone(base), 1, 5)
		start := w.n
		b.ResetTimer()
		for i := 0; b.Loop(); i++ {
			rows := slices.Clone(base)
			rows[4] = spinnerFrames[i%len(spinnerFrames)] + rows[4][1:]
			r.setLive(rows, 1, 5)
		}
		b.ReportMetric(float64(w.n-start)/float64(b.N), "wire-B/op")
	})
	b.Run("one_keystroke", func(b *testing.B) {
		w := &countWriter{}
		r := &inlineRenderer{t: &termState{out: w, fd: -1, width: 120, height: 40}}
		r.setLive(slices.Clone(base), 1, 5)
		start := w.n
		b.ResetTimer()
		for i := 0; b.Loop(); i++ {
			r.setLive(slices.Clone(base), 1, 5+i%2)
		}
		b.ReportMetric(float64(w.n-start)/float64(b.N), "wire-B/op")
	})
	b.Run("full_block_change", func(b *testing.B) {
		w := &countWriter{}
		r := &inlineRenderer{t: &termState{out: w, fd: -1, width: 120, height: 40}}
		r.setLive(slices.Clone(base), 1, 5)
		start := w.n
		b.ResetTimer()
		for i := 0; b.Loop(); i++ {
			rows := slices.Clone(base)
			mark := []string{"", " "}[i%2] // every row differs every frame
			for j := range rows {
				rows[j] = mark + rows[j]
			}
			r.setLive(rows, 1, 5)
		}
		b.ReportMetric(float64(w.n-start)/float64(b.N), "wire-B/op")
	})
}

// BenchmarkSanitizeRow confirms the sanitize funnel is not a hot-path
// regression: it runs on every live row of every frame.
func BenchmarkSanitizeRow(b *testing.B) {
	for _, tc := range []struct{ name, in string }{
		{"plain_ascii", strings.Repeat("lorem ipsum dolor sit amet ", 4)},
		{"styled", "\x1b[2;48;5;236msub-1  investigating the render path\x1b[0m · \x1b[1mwrite\x1b[0m notes.go"},
		{"contaminated", "sub \x1b[2B\ttab\r\n\u009b2B\x1b]0;t\x07 world \x1b[38:5:196mred\x1b[0m"},
		{"wide", "\u4e16\u754c \U0001F468\u200d\U0001F469 \u9032\u3081\u308b \u30c6\u30ad\u30b9\u30c8"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for b.Loop() {
				sanitizeRow(tc.in)
			}
		})
	}
}

// BenchmarkStreamingRows measures the goldmark path at growing open-block
// sizes, the one genuinely quadratic path: every delta re-parses the block.
func BenchmarkStreamingRows(b *testing.B) {
	chunk := "# Heading\n\nA paragraph with some *emphasis*, `code` and a [link](url).\n\n- one\n- two\n- three\n\n"
	for _, size := range []int{1 << 10, 1 << 12, 1 << 14} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			src := strings.Repeat(chunk, size/len(chunk)+1)
			u := &UI{theme: NewTheme(ColorNone, DefaultPalette()), streaming: true, textBuf: src}
			b.ResetTimer()
			for b.Loop() {
				u.streamingRows(118)
			}
		})
	}
}
