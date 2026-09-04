package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRefilterMatches(t *testing.T) {
	t.Parallel()

	items := []PickItem{
		{Label: "aperture/moonshotai/kimi-k3", Detail: "Kimi K3"},
		{Label: "openrouter/google/gemini-3-pro", Detail: "Gemini 3 Pro"},
		{Label: "anthropic/claude-opus-4-5", Terms: []string{"Claude Opus 4.5"}},
	}

	tests := []struct {
		name   string
		filter string
		exp    []int
	}{
		{"empty_lists_all", "", []int{0, 1, 2}},
		{"verbatim_only", "pro", []int{1}},
		{"verbatim_in_terms", "claude opus", []int{2}},
		{"fuzzy_fallback", "amk", []int{0}},
		{"no_match", "zzzz", []int{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.exp, refilterMatches(items, tc.filter))
		})
	}

	t.Run("verbatim_outranks_fuzzy", func(t *testing.T) {
		// "kimi" is verbatim in item 0 and a subsequence of item 1
		// (moonshotai/**k**imi... vs gem**i**ni-3-pro), so only item 0 lists
		assert.Equal(t, []int{0}, refilterMatches(items, "kimi"))
	})
	t.Run("boundary_hit_ranks_first", func(t *testing.T) {
		ranked := []PickItem{
			{Label: "a-very-long-prefix-opus"},
			{Label: "opus-model"},
		}
		assert.Equal(t, []int{1, 0}, refilterMatches(ranked, "opus"))
	})
}
