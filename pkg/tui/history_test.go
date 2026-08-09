package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLineBufferAdd(t *testing.T) {
	t.Parallel()

	t.Run("holds_partial_line", func(t *testing.T) {
		var b lineBuffer
		assert.Empty(t, b.Add("hel"))
		assert.Equal(t, "hel", b.pending.String())
	})
	t.Run("releases_complete_lines", func(t *testing.T) {
		var b lineBuffer
		assert.Empty(t, b.Add("hel"))
		assert.Equal(t, "hello\n", b.Add("lo\nwor"))
		assert.Equal(t, "wor", b.pending.String())
		assert.Equal(t, "world\n", b.Add("ld\n"))
		assert.Empty(t, b.pending.String())
	})
	t.Run("releases_multiple_lines", func(t *testing.T) {
		var b lineBuffer
		assert.Equal(t, "a\nb\n", b.Add("a\nb\nc"))
	})
	t.Run("empty_add", func(t *testing.T) {
		var b lineBuffer
		assert.Empty(t, b.Add(""))
		assert.Empty(t, b.pending.String())
	})
}

func TestLineBufferFlush(t *testing.T) {
	t.Parallel()

	t.Run("terminates_remainder", func(t *testing.T) {
		var b lineBuffer
		b.Add("tail")
		assert.Equal(t, "tail\n", b.Flush())
		assert.Empty(t, b.pending.String())
	})
	t.Run("empty_stays_empty", func(t *testing.T) {
		var b lineBuffer
		assert.Empty(t, b.Flush())
	})
}
