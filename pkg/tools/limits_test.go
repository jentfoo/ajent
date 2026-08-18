package tools

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
		{"line_bound_elides_head_tail", longLines(100), Limit{Lines: 5}, true},
		{"byte_bound_caps_budget", strings.Repeat("x", 2000), Limit{Bytes: 120}, true},
		{"unbounded_dimension_not_dropped", longLines(50), Limit{Lines: 60}, false},
		{"overlong_line_still_elides", strings.Repeat("y", MaxLineChars+100), Limit{Lines: 20}, true},
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
		out, _ := Elide(longLines(100), Limit{Lines: 10})
		assert.True(t, strings.HasPrefix(out, "line"))
		assert.True(t, strings.HasSuffix(strings.ReplaceAll(out, "\n...", ""), "line"))
	})

	t.Run("byte bound shrinks output", func(t *testing.T) {
		out, _ := Elide(strings.Repeat("x", 10000), Limit{Bytes: 200})
		assert.Less(t, len(out), 400)
	})

	t.Run("overlong_line_caps_the_budget", func(t *testing.T) {
		// one minified line longer than MaxLineChars must still elide and stay
		// bounded: head + marker + tail can never exceed two capped lines.
		out, dropped := Elide(strings.Repeat("y", MaxLineChars+100), Limit{Lines: 2000})
		assert.True(t, dropped)
		assert.LessOrEqual(t, len(out), MaxLineChars*2+len(elideMarker))
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
		assert.Equal(t, "abcdefghij\n", over.String())
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
		assert.Equal(t, "three\nfour\n", over.String())
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

		// now over budget
		n, err = w.Write([]byte("ef"))
		require.NoError(t, err)
		assert.Equal(t, 2, n)
		assert.Contains(t, over.String(), "ef")
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
