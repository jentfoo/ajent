package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMatchScore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		text    string
		query   string
		matches bool
	}{
		{"empty_query_matches", "anything", "", true},
		{"exact_prefix", "opus", "opus", true},
		{"case_insensitive", "Claude Opus", "opus", true},
		{"subsequence", "anthropic/claude-opus-4-5", "aco", true},
		{"provider_prefix", "openrouter/z-ai/glm", "openr", true},
		{"no_match", "opus", "zzz", false},
		{"out_of_order_fails", "opus", "supo", false},
		{"longer_than_text", "ab", "abc", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := matchScore(tc.text, tc.query)
			assert.Equal(t, tc.matches, ok)
		})
	}

	t.Run("contiguous_beats_scattered", func(t *testing.T) {
		tight, ok := matchScore("qwen3.6", "qwen")
		require.True(t, ok)
		loose, ok := matchScore("q-w-e-n-x", "qwen")
		require.True(t, ok)
		assert.Greater(t, tight, loose)
	})
	t.Run("word_boundary_beats_mid_word", func(t *testing.T) {
		boundary, ok := matchScore("lmstudio/qwen", "qwen")
		require.True(t, ok)
		mid, ok := matchScore("xxqwen", "qwen")
		require.True(t, ok)
		assert.Greater(t, boundary, mid)
	})
	t.Run("earlier_match_beats_later", func(t *testing.T) {
		early, ok := matchScore("opus-model", "opus")
		require.True(t, ok)
		late, ok := matchScore("a-very-long-prefix-opus", "opus")
		require.True(t, ok)
		assert.Greater(t, early, late)
	})
}

func TestVerbatimScore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		text    string
		query   string
		matches bool
	}{
		{"empty_query_matches", "anything", "", true},
		{"exact", "opus", "opus", true},
		{"case_insensitive", "Claude Opus", "opus", true},
		{"mid_word", "reasoning", "son", true},
		{"after_separator", "aperture/moonshotai/kimi-k3", "kimi", true},
		{"scattered_fails", "aperture/moonshotai/kimi-k3", "pro", false},
		{"separator_omitted_fails", "openai/gpt-5", "gpt5", false},
		{"out_of_order_fails", "opus", "supo", false},
		{"longer_than_text", "ab", "abc", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := verbatimScore(tc.text, tc.query)
			assert.Equal(t, tc.matches, ok)
		})
	}

	t.Run("word_boundary_beats_mid_word", func(t *testing.T) {
		boundary, ok := verbatimScore("lmstudio/qwen", "qwen")
		require.True(t, ok)
		mid, ok := verbatimScore("xxqwen", "qwen")
		require.True(t, ok)
		assert.Greater(t, boundary, mid)
	})
	t.Run("earlier_match_beats_later", func(t *testing.T) {
		early, ok := verbatimScore("opus-model", "opus")
		require.True(t, ok)
		late, ok := verbatimScore("a-very-long-prefix-opus", "opus")
		require.True(t, ok)
		assert.Greater(t, early, late)
	})
	t.Run("best_of_repeated_hits", func(t *testing.T) {
		// the mid-word hit comes first; the boundary hit later still wins
		best, ok := verbatimScore("xopus/opus", "opus")
		require.True(t, ok)
		assert.Equal(t, boundaryBonus-6, best)
	})
}

func TestIsBoundary(t *testing.T) {
	t.Parallel()

	for _, c := range []byte{' ', '/', '-', '_', '.', ':', '\t'} {
		assert.True(t, isBoundary(c), string(c))
	}
	for _, c := range []byte{'a', 'Z', '0'} {
		assert.False(t, isBoundary(c), string(c))
	}
}

func TestWindowFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		cursor, total, rows int
		expStart, expEnd    int
	}{
		{"fits_entirely", 0, 3, 10, 0, 3},
		{"no_rows", 0, 3, 0, 0, 0},
		{"empty_list", 0, 0, 5, 0, 0},
		{"cursor_centred", 10, 100, 5, 8, 13},
		{"clamped_at_the_start", 0, 100, 5, 0, 5},
		{"clamped_at_the_end", 99, 100, 5, 95, 100},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start, end := windowFor(tc.cursor, tc.total, tc.rows)
			assert.Equal(t, tc.expStart, start)
			assert.Equal(t, tc.expEnd, end)
		})
	}
}

func TestWrapIndex(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 0, wrapIndex(0, 3))
	assert.Equal(t, 2, wrapIndex(-1, 3))
	assert.Equal(t, 0, wrapIndex(3, 3))
	assert.Equal(t, 0, wrapIndex(5, 0))
}
