package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOutputHead(t *testing.T) {
	t.Parallel()

	t.Run("head_under_limit", func(t *testing.T) {
		var h outputHead
		var out strings.Builder
		for i := 1; i <= 3; i++ {
			out.WriteString(h.add("line " + string(rune('a'+i-1)) + "\n"))
		}
		assert.Equal(t, "line a\nline b\nline c\n", out.String())
		assert.Zero(t, h.hidden())
		assert.Empty(t, h.summary())
	})
	t.Run("head_then_summary", func(t *testing.T) {
		var h outputHead
		var out strings.Builder
		for i := 1; i <= 6; i++ {
			out.WriteString(h.add("0123456789\n")) // 10 ascii runes each past the head
		}
		assert.Equal(t, "0123456789\n0123456789\n0123456789\n0123456789\n", out.String())
		assert.Equal(t, 2, h.hidden())
		assert.Contains(t, h.summary(), "+2 lines")
	})
	t.Run("partial_line_flush", func(t *testing.T) {
		var h outputHead
		out := ""
		out += h.add("ok  gith") // no newline: held back whole
		assert.Empty(t, out)
		assert.Zero(t, h.hidden())
		out = h.flush()
		assert.Equal(t, "ok  gith\n", out)
	})
	t.Run("reset_between_calls", func(t *testing.T) {
		var h outputHead
		for i := 1; i <= 8; i++ {
			h.add("x\n")
		}
		assert.Equal(t, 4, h.hidden())
		h.reset()
		out := h.add("fresh\n")
		assert.Equal(t, "fresh\n", out)
		assert.Zero(t, h.hidden())
	})
	t.Run("single_write_whole_body", func(t *testing.T) {
		var b []byte
		for i := 1; i <= 6; i++ {
			b = append(b, "line "+string(rune('0'+i))+"\n"...)
		}
		var h outputHead // the Display path: one call carries the whole body
		out := h.add(string(b))
		assert.Equal(t, "line 1\nline 2\nline 3\nline 4\n", out)
		assert.Equal(t, 2, h.hidden())
	})
}
