package tokens

import (
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
)

func TestCompactAt(t *testing.T) {
	t.Parallel()

	const win = 100000

	cases := []struct {
		name string
		m    llm.Model
		want int
	}{
		// no threshold: default fraction (0.8) of the window.
		{"default_fraction", llm.Model{ContextWindow: win}, 80000},
		// an absolute count below the window passes through unchanged.
		{"absolute_count_below_window", llm.Model{ContextWindow: win, CompactThreshold: 64000}, 64000},
		// a fractional threshold scales the window directly.
		{"fraction_override", llm.Model{ContextWindow: win, CompactThreshold: 0.5}, 50000},
		// an absolute count above the window clamps to it.
		{"absolute_count_clamped_to_window", llm.Model{ContextWindow: win, CompactThreshold: 1e6}, 100000},
		// a negative threshold falls back to the default fraction.
		{"invalid_negative_uses_default", llm.Model{ContextWindow: win, CompactThreshold: -1}, 80000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, CompactAt(tc.m))
		})
	}

	t.Run("zero_window_never_triggers", func(t *testing.T) {
		assert.Zero(t, CompactAt(llm.Model{}))
		assert.Zero(t, CompactAt(llm.Model{CompactThreshold: 0.8}))
	})
}
