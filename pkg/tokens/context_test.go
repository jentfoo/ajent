package tokens

import (
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
)

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
