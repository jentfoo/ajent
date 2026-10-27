package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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
		r := newTestInlineAt(w, 120, 40)
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
		r := newTestInlineAt(w, 120, 40)
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
		r := newTestInlineAt(w, 120, 40)
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

// BenchmarkStreamingRows measures the per-delta cost of composing the live
// markdown preview at growing open-block sizes. Every delta recomposes; idle
// spinner frames ride the memo and only compare keys. Past the tail cap the
// cost is flat instead of growing with the block.
func BenchmarkStreamingRows(b *testing.B) {
	chunk := "# Heading\n\nA paragraph with some *emphasis*, `code` and a [link](url).\n\n- one\n- two\n- three\n\n"
	for _, size := range []int{1 << 10, 1 << 12, 1 << 14, 1 << 18} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			src := strings.Repeat(chunk, size/len(chunk)+1)
			u := withTimer(&UI{theme: NewTheme(ColorNone, DefaultPalette()), streaming: true, textBuf: src})
			b.ResetTimer()
			for b.Loop() {
				u.streamOK = false // a delta landed; the memo no longer matches
				u.streamingRows(118)
			}
		})
	}
}

// BenchmarkCodeBlock measures the highlight path against the flat fallback at
// growing block sizes. Highlighting runs once per block, at commit.
func BenchmarkCodeBlock(b *testing.B) {
	chunk := "func handle(ctx context.Context, n int) error {\n\ts := fmt.Sprintf(\"n=%d\", n) // note\n\tif n > 0 {\n\t\treturn errors.New(s)\n\t}\n\treturn nil\n}\n\n"
	for _, lines := range []int{50, 200, 800} {
		src := "```go\n" + strings.Repeat(chunk, lines/8+1) + "```"
		b.Run(strconv.Itoa(lines)+"_highlighted", func(b *testing.B) {
			th := NewTheme(Color256, DefaultPalette())
			for b.Loop() {
				renderMarkdown(th, 118, src)
			}
		})
		b.Run(strconv.Itoa(lines)+"_flat", func(b *testing.B) {
			th := NewTheme(ColorBasic, DefaultPalette())
			for b.Loop() {
				renderMarkdown(th, 118, src)
			}
		})
	}
}

// benchStreamChunks cuts a reply into provider-sized deltas, split at spaces so
// the boundaries look like real token traffic.
func benchStreamChunks(src string, size int) []string {
	var out []string
	for len(src) > 0 {
		end := min(size, len(src))
		if end < len(src) {
			if sp := strings.LastIndexByte(src[:end], ' '); sp > 0 {
				end = sp + 1 // cut after a space, never mid-word
			}
		}
		out, src = append(out, src[:end]), src[end:]
	}
	return out
}

// BenchmarkStreamText measures one streamed reply end to end: the per-delta
// block split over the pending buffer, the preview compose and the repaint each
// delta forces. The cases differ in how long a block stays pending, which is
// what decides how much of the buffer every later delta re-reads.
func BenchmarkStreamText(b *testing.B) {
	prose := strings.Repeat("A paragraph with some *emphasis*, `code` and a [link](https://example.com) that runs on.\n\n", 10)
	paragraph := strings.ReplaceAll(strings.TrimSpace(prose), "\n\n", " ")
	fence := "```go\n" + strings.Repeat("func step(n int) int { return n % 97 } // trimmed\n", 30)

	for _, tc := range []struct{ name, src string }{
		{"prose_blocks", prose},       // commits every paragraph: the buffer stays short
		{"open_paragraph", paragraph}, // one block streams to the end: worst pending size
		{"open_fence", fence},
	} {
		b.Run(tc.name, func(b *testing.B) {
			chunks := benchStreamChunks(tc.src, 24)
			w := &countWriter{}
			u := withTimer(&UI{
				theme:  NewTheme(ColorNone, DefaultPalette()),
				render: newTestInlineAt(w, 120, 40),
				mode:   ModeInline,
			})
			b.ReportMetric(float64(len(chunks)), "deltas/op")

			for b.Loop() {
				for _, c := range chunks {
					u.Text(c)
				}
				u.EndText()
			}
			b.ReportMetric(float64(w.n)/float64(b.N), "wire-B/op")
		})
	}
}

// BenchmarkWrapLine measures re-cutting one styled row into display rows, the
// per-line cost every markdown, diff and tool-output commit pays. The fast path
// is a line that already fits; the rest run the cell split and re-join.
func BenchmarkWrapLine(b *testing.B) {
	plain := strings.Repeat("lorem ipsum dolor sit amet ", 12)
	styled := "\x1b[38:5:196mERR\x1b[0m " + strings.Repeat("panic recovered in \\u001b[31mreduce\x1b[0m step ", 12)
	wide := strings.Repeat("\u4e16\u754c \U0001F468\u200d\U0001F469 \u30c6\u30ad\u30b9\u30c8 ", 24)

	for _, tc := range []struct{ name, line string }{
		{"fits", strings.Repeat("short row ", 8)},
		{"plain_wrap", plain},
		{"styled_wrap", styled},
		{"wide_wrap", wide},
	} {
		b.Run(tc.name+"_soft", func(b *testing.B) {
			for b.Loop() {
				wrapLine(tc.line, 118)
			}
		})
		b.Run(tc.name+"_hard", func(b *testing.B) {
			for b.Loop() {
				hardWrap(tc.line, 118)
			}
		})
	}
}

// BenchmarkMarkdownTable measures table layout, which no other bench reaches:
// every repaint of a streaming table re-measures each cell and re-lays the grid,
// and a narrow terminal adds column shrinking plus per-cell wrapping.
func BenchmarkMarkdownTable(b *testing.B) {
	th := NewTheme(Color256, DefaultPalette())
	row := "| `reduce.go` | trimmed buffer keeps a window of recent lines | 214 | ok |\n"
	src := "| file | note | line | state |\n|---|---|---:|---|\n" + strings.Repeat(row, 12)

	for _, tc := range []struct {
		name  string
		width int
	}{
		{"fits", 160},
		{"shrinks", 96},
		{"wraps_cells", 60},
	} {
		b.Run(tc.name, func(b *testing.B) {
			require.NotEmpty(b, renderMarkdown(th, tc.width, src))

			for b.Loop() {
				renderMarkdown(th, tc.width, src)
			}
		})
	}
}

// benchDiffFile is a large Go-like file the diff benches run against.
func benchDiffFile() string {
	var sb strings.Builder
	for i := 0; i < 3000; i += 6 {
		fmt.Fprintf(&sb, "func handler_%04d(ctx context.Context, n int) error {\n", i)
		sb.WriteString("\tif n <= 0 {\n\t\treturn fmt.Errorf(\"bad input %d\", n)\n\t}\n\treturn nil\n}\n\n")
	}
	return sb.String()
}

// BenchmarkRenderDiff measures diff rendering on the approval-dialog and
// edit-result path: go-udiff over whole files plus hunk and intraline markup.
func BenchmarkRenderDiff(b *testing.B) {
	th := NewTheme(Color256, DefaultPalette())
	before := benchDiffFile()
	small := strings.Replace(before, "if n <= 0 {", "if n < 0 {", 1)
	scattered := strings.ReplaceAll(before, "if n <= 0 {", "if n < 0 {")
	for _, tc := range []struct {
		name, before, after string
	}{
		{"small_change", before, small},
		{"scattered_changes", before, scattered},
	} {
		b.Run(tc.name, func(b *testing.B) {
			out := RenderDiff(th, "big.go", tc.before, tc.after)
			require.NotEmpty(b, out)
			b.ReportMetric(float64(lineCount(tc.before)), "file-lines")

			for b.Loop() {
				RenderDiff(th, "big.go", tc.before, tc.after)
			}
		})
	}
}

// BenchmarkToolOutput measures the per-delta streamed-output path: head commits,
// the collapse counter past the cap, and the transient activity row.
func BenchmarkToolOutput(b *testing.B) {
	v := newVT(120, 40)
	u := newTestUI(b, v, strings.NewReader(""))
	delta := "goroutine 1 [running]: main.step(0x41, 0x9) at reduce.go:214 +0x9f\n"
	for _, tc := range []struct {
		name  string
		delta string
	}{
		{"plain_line", delta},
		{"styled_line", "\x1b[31mFAIL\x1b[0m: TestStep flaked on pass 12 of 200\n"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for b.Loop() {
				u.mu.Lock()
				u.runs = nil // one op is one tool's whole stream
				u.mu.Unlock()
				for range 200 {
					u.Output("call", tc.delta)
				}
			}
		})
	}
}
