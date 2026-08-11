package tokens

import (
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
)

// wantReserve mirrors the Reserve policy for a given window and configured value,
// so tests assert branch selection (absolute vs fraction vs clamp) against a
// clear reference rather than magic numbers.
func wantReserve(window int, cfg float64) int {
	if window <= 0 {
		return 0
	}
	maxR := max(1, window*9/10)
	switch {
	case cfg >= 1: // absolute token count, clamped to 90%
		return min(int(cfg), maxR)
	case cfg > 0 && cfg < 1: // fraction of the window, clamped to 15%
		r := int(float64(window) * cfg)
		if r <= 0 {
			return 500
		}
		return min(r, maxR)
	default:
		r := int(float64(window) * defaultReserveFraction)
		if r <= 1 {
			return 900
		}
		return min(r, maxR)
	}
}

func TestReserve(t *testing.T) {
	t.Parallel()

	const win = 100000

	cases := []struct {
		name string
		m    llm.Model
	}{
		{"default_fraction", llm.Model{ContextWindow: win}},
		{"absolute_count_unclamped", llm.Model{ContextWindow: win, ContextReserve: 32000}},
		{"fraction_override", llm.Model{ContextWindow: win, ContextReserve: 0.4}},
		{"out_of_range_clamps_to_ninety", llm.Model{ContextWindow: win, ContextReserve: 1e6}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t,
				wantReserve(tc.m.ContextWindow, tc.m.ContextReserve),
				Reserve(tc.m))
		})
	}

	t.Run("zero_window_no_reserve", func(t *testing.T) {
		assert.Zero(t, Reserve(llm.Model{}))
	})
}
