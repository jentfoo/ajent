package tokens

import (
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
)

// wantReserve mirrors the Reserve policy so tests assert branch selection against a
// clear reference rather than magic numbers.
func wantReserve(window int, cfg float64) int {
	if window <= 0 {
		return 0
	}
	maxR := max(1, window*9/10)
	switch {
	case cfg >= 1: // absolute token count, clamped to the cap
		return min(int(cfg), maxR)
	default:
		fraction := defaultReserveFraction
		if cfg > 0 && cfg < 1 { // fraction of the window
			fraction = cfg
		}
		r := int(float64(window) * fraction)
		return min(max(r, 1), maxR)
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
		{"tiny_window_default_min_one", llm.Model{ContextWindow: 3}}, // fraction rounds to zero -> floor of one
		{"tiny_window_fraction_min_one", llm.Model{ContextWindow: 5, ContextReserve: 0.2}},
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

// wantCompactAt mirrors the CompactAt policy so tests assert branch selection
// against a clear reference rather than magic numbers.
func wantCompactAt(window int, threshold float64) int {
	if window <= 0 {
		return 0
	}
	switch {
	case threshold >= 1: // absolute token count, clamped to the window
		return min(int(threshold), window)
	default:
		fraction := defaultCompactFraction
		if threshold > 0 && threshold < 1 { // fraction of the window
			fraction = threshold
		}
		return int(float64(window) * fraction)
	}
}

func TestCompactAt(t *testing.T) {
	t.Parallel()

	const win = 100000

	cases := []struct {
		name string
		m    llm.Model
	}{
		{"default_fraction", llm.Model{ContextWindow: win}},
		{"absolute_count", llm.Model{ContextWindow: win, CompactThreshold: 64000}},
		{"absolute_clamps_to_window", llm.Model{ContextWindow: win, CompactThreshold: 1e6}},
		{"fraction_override", llm.Model{ContextWindow: win, CompactThreshold: 0.5}},
		{"invalid_negative_uses_default", llm.Model{ContextWindow: win, CompactThreshold: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t,
				wantCompactAt(tc.m.ContextWindow, tc.m.CompactThreshold),
				CompactAt(tc.m))
		})
	}

	t.Run("zero_window_never_triggers", func(t *testing.T) {
		assert.Zero(t, CompactAt(llm.Model{}))
		assert.Zero(t, CompactAt(llm.Model{CompactThreshold: 0.8}))
	})

	t.Run("default_is_eighty_percent", func(t *testing.T) {
		assert.Equal(t, 80000, CompactAt(llm.Model{ContextWindow: win}))
	})
}
