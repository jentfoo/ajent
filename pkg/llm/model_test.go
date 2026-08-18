package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestModelAccepts(t *testing.T) {
	t.Parallel()

	m := Model{Input: []Modality{ModalityText, ModalityImage}}
	assert.True(t, m.Accepts(ModalityImage))
	assert.False(t, Model{Input: []Modality{ModalityText}}.Accepts(ModalityImage))
	assert.False(t, Model{}.Accepts(ModalityText))
}

func TestReserve(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		m    Model
		want int
	}{
		{"default_fraction", Model{ContextWindow: 200000}, 40000},
		{"absolute_count_unclamped", Model{ContextWindow: 50000, ContextReserve: 30000}, 30000},
		{"fraction_override", Model{ContextWindow: 60000, ContextReserve: 0.25}, 15000},
		{"out_of_range_clamps_to_ninety", Model{ContextWindow: 40000, ContextReserve: 99999}, 36000},
		{"tiny_window_default_min_one", Model{ContextWindow: 3}, 1}, // fraction rounds to zero -> floor of one
		{"tiny_window_fraction_min_one", Model{ContextWindow: 5, ContextReserve: 0.2}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.m.Reserve())
		})
	}

	t.Run("zero_window_no_reserve", func(t *testing.T) {
		assert.Zero(t, Model{}.Reserve())
	})
}
