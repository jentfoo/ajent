package tools

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBound(t *testing.T) {
	t.Parallel()

	longLines := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString("line\n")
		}
		return b.String()
	}

	cases := []struct {
		name      string
		in        string
		limit     Limit
		wantDrop  bool
		wantShown int
	}{
		{"under_both_bounds_unchanged", "a\nb\nc", Limit{Lines: 10, Bytes: 1000}, false, 3},
		{"line_bound_truncates_head_only", longLines(50), Limit{Lines: 5}, true, 5},
		{"byte_bound_caps_budget", strings.Repeat("x", 2000), Limit{Bytes: 120}, true, 1},
		{"unbounded_dimension_not_dropped", longLines(2), Limit{}, false, 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := Bound(tc.in, tc.limit)
			assert.Equal(t, tc.wantDrop, b.Truncated)
			assert.Equal(t, tc.wantShown, b.Shown)
			if !tc.wantDrop {
				assert.Equal(t, tc.in, b.Text)
				return
			}
			assert.NotContains(t, b.Text, "...") // head only, no mid marker
		})
	}

	t.Run("no_duplication_when_truncated", func(t *testing.T) {
		// one overlong line plus truncation: each retained line appears exactly once
		in := "alpha\n" + strings.Repeat("y", MaxLineRunes+100) + "\nbeta\n"
		b := Bound(in, Limit{Lines: 2})
		assert.True(t, b.Truncated)
		assert.Equal(t, 1, strings.Count(b.Text, "alpha"))
		assert.NotContains(t, b.Text, "beta") // third line dropped by the head-only policy
		assert.Equal(t, 2, b.Shown)
		// the overlong kept line is capped even though it fits the bounds
		for _, ln := range strings.Split(b.Text, "\n") {
			assert.LessOrEqual(t, len([]rune(ln)), MaxLineRunes)
		}
	})

	t.Run("overlong line alone truncates", func(t *testing.T) {
		// within both bounds, but one minified line must not reach the model whole
		in := "short\n" + strings.Repeat("y", MaxLineRunes+100)
		b := Bound(in, Limit{Lines: 10, Bytes: 1 << 20})
		assert.True(t, b.Truncated)
		assert.Equal(t, 2, b.Shown) // every line kept, each capped
		for _, ln := range strings.Split(b.Text, "\n") {
			assert.LessOrEqual(t, len([]rune(ln)), MaxLineRunes)
		}
	})

	t.Run("single_long_line_cut_at_rune_budget", func(t *testing.T) {
		cjk := strings.Repeat("\u4e2d", 3000) // runes, not bytes; a byte-bound cut would stop at ~667
		b := Bound(cjk+"\n", Limit{Bytes: 100})
		assert.True(t, b.Truncated)
		runes := []rune(b.Text)
		assert.Len(t, runes, MaxLineRunes) // cut to the rune budget, not bytes
	})

	t.Run("first_line_alone_cut_when_nothing_fits", func(t *testing.T) {
		in := strings.Repeat("\u4e2d", 3000) + "\nrest\n"
		b := Bound(in, Limit{Bytes: 100})
		assert.True(t, b.Truncated)
		runes := []rune(b.Text)
		assert.Len(t, runes, MaxLineRunes) // single cut first line
	})

	t.Run("truncation_note_names_totals", func(t *testing.T) {
		b := Bound(longLines(50), Limit{Lines: 5})
		note := truncationNote(b, "/tmp/spill.txt")
		assert.Contains(t, note, "5/50 lines shown")
		assert.Contains(t, note, "/tmp/spill.txt") // names the spill path
	})
}

func TestElide(t *testing.T) {
	t.Parallel()

	longLines := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString("line\n")
		}
		return b.String()
	}

	cases := []struct {
		name     string
		in       string
		limit    Limit
		wantDrop bool
	}{
		{"under_both_bounds_unchanged", "a\nb\nc", Limit{Lines: 10, Bytes: 1000}, false},
		{"line_bound_elides_head_tail", longLines(50), Limit{Lines: 5}, true},
		{"byte_bound_caps_budget", strings.Repeat("x", 2000), Limit{Bytes: 120}, true},
		{"unbounded_dimension_not_dropped", longLines(2), Limit{}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, dropped := Elide(tc.in, tc.limit)
			assert.Equal(t, tc.wantDrop, dropped)
			if !tc.wantDrop {
				assert.Equal(t, tc.in, out)
				return
			}
			assert.Contains(t, out, "... [truncated]")
		})
	}

	t.Run("head and tail preserved", func(t *testing.T) {
		out, _ := Elide(longLines(50), Limit{Lines: 10})
		assert.True(t, strings.HasPrefix(out, "line"))
		assert.True(t, strings.HasSuffix(strings.ReplaceAll(out, "\n...", ""), "line"))
	})

	t.Run("byte bound shrinks output", func(t *testing.T) {
		out, _ := Elide(strings.Repeat("x", 10000), Limit{Bytes: 200})
		assert.Less(t, len(out), 400)
	})

	t.Run("max_keep_clamped_to_line_count", func(t *testing.T) {
		// a budget larger than the input must not duplicate lines (the old bug)
		out, dropped := Elide(longLines(2), Limit{Lines: 2000})
		assert.False(t, dropped) // under every bound: unchanged
		assert.Equal(t, longLines(2), out)
	})
}

func TestWriterSpillsAtBound(t *testing.T) {
	t.Parallel()

	t.Run("crosses byte bound whole lines", func(t *testing.T) {
		var kept bytes.Buffer
		var over bytes.Buffer
		w := Writer(&kept, Limit{Bytes: 16}, &over)

		n, err := w.Write([]byte("1234567890\n"))
		require.NoError(t, err)
		assert.Equal(t, 11, n)
		assert.Equal(t, "1234567890\n", kept.String())
		assert.Empty(t, over.String())

		// second write is past the bound and spills entirely
		n, err = w.Write([]byte("abcdefghij\n"))
		require.NoError(t, err)
		assert.Equal(t, 11, n)
		assert.Contains(t, over.String(), "1234567890\n") // head replay precedes overflow
		assert.Contains(t, over.String(), "abcdefghij\n")
	})

	t.Run("line bound spills whole lines", func(t *testing.T) {
		var kept bytes.Buffer
		var over bytes.Buffer
		w := Writer(&kept, Limit{Lines: 2}, &over)

		n, err := w.Write([]byte("one\ntwo\n"))
		require.NoError(t, err)
		assert.Equal(t, 8, n)
		assert.Equal(t, "one\ntwo", strings.TrimRight(kept.String(), "\n"))

		// third line crosses the bound and spills whole
		n, err = w.Write([]byte("three\nfour\n"))
		require.NoError(t, err)
		assert.Equal(t, 11, n)
		assert.Contains(t, over.String(), "one\ntwo\n") // head replay first
		assert.Contains(t, over.String(), "three\nfour\n")
	})

	t.Run("buffers partial line across writes", func(t *testing.T) {
		var kept bytes.Buffer
		var over bytes.Buffer
		w := Writer(&kept, Limit{Lines: 1}, &over)

		n, err := w.Write([]byte("ab")) // partial line buffered
		require.NoError(t, err)
		assert.Equal(t, 2, n)
		assert.Empty(t, kept.String())

		n, err = w.Write([]byte("cd\n")) // completes the first line
		require.NoError(t, err)
		assert.Equal(t, 3, n)
		assert.Equal(t, "abcd\n", kept.String())

		// now over budget; the spill file holds head + overflow
		n, err = w.Write([]byte("ef"))
		require.NoError(t, err)
		assert.Equal(t, 2, n)
		assert.Contains(t, over.String(), "abcd\nef")
	})

	t.Run("spill_file_equals_full_stream", func(t *testing.T) {
		var kept bytes.Buffer
		var over bytes.Buffer
		w := Writer(&kept, Limit{Lines: 3}, &over)
		full := strings.Repeat("line\n", 10)

		n, err := w.Write([]byte(full))
		require.NoError(t, err)
		assert.Equal(t, len(full), n)
		bw := w.(*boundedWriter)
		bw.Flush()

		// the spill destination holds head + overflow = the complete stream
		assert.Equal(t, full, over.String())
		assert.True(t, bw.Truncated())
	})

	t.Run("spill keeps order across a mid-line bound", func(t *testing.T) {
		// a partial line held in the buffer when the byte bound hits must reach
		// the spill before later chunks, or the file is not the real stream
		var kept bytes.Buffer
		var over bytes.Buffer
		w := Writer(&kept, Limit{Bytes: 10}, &over)
		bw := w.(*boundedWriter)

		for _, chunk := range []string{"abc", "defgh", "ij", "k"} {
			_, _ = bw.Write([]byte(chunk))
		}
		bw.Flush()

		assert.Empty(t, kept.String()) // the bound hit inside the first line
		assert.Equal(t, "abcdefghijk", over.String())
		assert.True(t, bw.Truncated())
		lines, byts := bw.Total()
		assert.Equal(t, 1, lines) // one partial line, never terminated
		assert.Equal(t, len("abcdefghijk"), byts)
	})

	t.Run("nil overflow still reports truncation", func(t *testing.T) {
		var kept bytes.Buffer
		w := Writer(&kept, Limit{Lines: 1}, nil).(*boundedWriter)

		_, _ = w.Write([]byte("one\ntwo\n"))

		assert.Equal(t, "one\n", kept.String())
		assert.True(t, w.Truncated()) // dropped content must not pass silently
	})

	t.Run("totals_count_every_input_line", func(t *testing.T) {
		var kept bytes.Buffer
		var over bytes.Buffer
		w := Writer(&kept, Limit{Lines: 3}, &over)
		bw := w.(*boundedWriter)

		_, _ = bw.Write([]byte("one\ntwo\n"))
		_, _ = bw.Write([]byte("three\nfour\nfive\n"))
		bw.Flush()

		lines, bytes_ := bw.Total()
		assert.Equal(t, 5, lines) // every input line; a trailing newline adds no line
		assert.Equal(t, len("one\ntwo\n")+len("three\nfour\nfive\n"), bytes_)
	})
}

func TestApplyLimitsNonZeroFields(t *testing.T) {
	t.Parallel()

	orig := Limits{
		Bash: BashLimit(), Read: ReadFileLimit(), Find: FindResultLimit(),
		Grep: GrepResultLimit(), Ls: LsResultLimit(), RefInject: RefInjectLimit(), RefTotal: RefTotalLimit(),
	}
	t.Cleanup(func() { ApplyLimits(orig) })

	// only the set dimension changes; a zero field keeps its default
	const bashLines = 12 // distinct from the compiled-in defaults below
	const readBytes = 9001
	ApplyLimits(Limits{Bash: Limit{Lines: bashLines}, Read: Limit{Bytes: readBytes}})
	b := BashLimit()
	assert.Equal(t, bashLines, b.Lines) // bash line bound overridden
	assert.Equal(t, 32<<10, b.Bytes)    // bash byte bound untouched (default)
	r := ReadFileLimit()
	assert.Equal(t, 2000, r.Lines)      // read line bound untouched (default)
	assert.Equal(t, readBytes, r.Bytes) // read bytes overridden
}
