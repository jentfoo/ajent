package tools

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestElideLineBound(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("line\n")
	}
	s := b.String()
	out, dropped := Elide(s, Limit{Lines: 10})
	assert.True(t, dropped)
	assert.Contains(t, out, "... [truncated]")
	assert.True(t, strings.HasPrefix(out, "line"))
	assert.True(t, strings.HasSuffix(strings.ReplaceAll(out, "\n...", ""), "line"))
}

func TestElideByteBound(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 10000)
	out, dropped := Elide(long, Limit{Bytes: 200})
	assert.True(t, dropped)
	assert.Less(t, len(out), 400)
	assert.Contains(t, out, "... [truncated]")
}

func TestElideUnboundedDimension(t *testing.T) {
	t.Parallel()

	// bytes unbounded but lines bounded
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("line\n")
	}
	out, dropped := Elide(b.String(), Limit{Lines: 100})
	assert.False(t, dropped) // under the line bound and no byte bound
	assert.Equal(t, b.String(), out)
}

func TestElideUnderBothBoundsUnchanged(t *testing.T) {
	t.Parallel()

	s := "a\nb\nc"
	out, dropped := Elide(s, Limit{Lines: 10, Bytes: 1000})
	assert.False(t, dropped)
	assert.Equal(t, s, out)
}

func TestElideOverlongLineCapsBudget(t *testing.T) {
	t.Parallel()

	// one minified line longer than MaxLineChars must still elide
	long := strings.Repeat("y", MaxLineChars+100)
	out, dropped := Elide(long, Limit{Lines: 2000})
	assert.True(t, dropped)
	assert.LessOrEqual(t, len(out), MaxLineChars*2+len(elideMarker))
}

func TestWriterDivertsAtCrossover(t *testing.T) {
	t.Parallel()

	var kept bytes.Buffer
	var over bytes.Buffer
	w := Writer(&kept, Limit{Bytes: 16}, &over)

	// writes a full line that crosses the byte bound; it spills whole
	n, err := w.Write([]byte("1234567890\n"))
	require.NoError(t, err)
	assert.Equal(t, 11, n)
	assert.Equal(t, "1234567890\n", kept.String())
	assert.Empty(t, over.String())

	// second write is past the bound and spills entirely
	_, _ = w.Write([]byte("abcdefghij\n"))
	assert.Equal(t, "abcdefghij\n", over.String())
}

func TestWriterLineBoundSpillsWholeLines(t *testing.T) {
	t.Parallel()

	var kept bytes.Buffer
	var over bytes.Buffer
	w := Writer(&kept, Limit{Lines: 2}, &over)

	_, _ = w.Write([]byte("one\ntwo\n"))
	assert.Equal(t, "one\ntwo", strings.TrimRight(kept.String(), "\n"))

	// third line crosses the bound and spills whole
	_, _ = w.Write([]byte("three\nfour\n"))
	assert.Equal(t, "three\nfour\n", over.String())
}

func TestWriterBuffersPartialLineAcrossWrites(t *testing.T) {
	t.Parallel()

	var kept bytes.Buffer
	var over bytes.Buffer
	w := Writer(&kept, Limit{Lines: 1}, &over)

	_, _ = w.Write([]byte("ab")) // partial line buffered
	assert.Empty(t, kept.String())
	_, _ = w.Write([]byte("cd\n")) // completes the first line
	assert.Equal(t, "abcd\n", kept.String())

	// now over budget
	_, _ = w.Write([]byte("ef"))
	assert.Contains(t, over.String(), "ef")
}
