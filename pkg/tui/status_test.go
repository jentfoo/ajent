package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStatusRows(t *testing.T) {
	t.Parallel()

	plain := NewTheme(ColorNone)

	single := func(s Status, width int) []string { return s.rows(plain, width) }

	t.Run("full_line", func(t *testing.T) {
		s := Status{Model: "opus-5", Tokens: 68200, MaxTokens: 200000}
		assert.Equal(t, []string{"▓▓▓▓░░░░░░ 68.2k/200k · opus-5"}, single(s, 80))
	})
	t.Run("model_only", func(t *testing.T) {
		assert.Equal(t, []string{"opus-5"}, Status{Model: "opus-5"}.rows(plain, 40))
	})
	t.Run("empty_status_is_one_blank_row", func(t *testing.T) {
		// the live block always reserves one status row
		assert.Equal(t, []string{""}, Status{}.rows(plain, 80))
	})
	t.Run("context_survives_narrow_width", func(t *testing.T) {
		s := Status{Model: "opus-5", Tokens: 68200, MaxTokens: 200000}
		// the fixed part alone overflows; row one is clipped to width and stays single
		assert.Equal(t, []string{"▓▓▓▓░░░░░░ 68.2k/200k"}, single(s, 21))
	})
	t.Run("over_capacity_clamped", func(t *testing.T) {
		s := Status{Tokens: 300000, MaxTokens: 100}
		// tokens past the window clamp to a full bar rather than overflowing
		assert.Equal(t, []string{"▓▓▓▓▓▓▓▓▓▓ 300k/100"}, single(s, 80))
	})
	t.Run("styled_when_color_enabled", func(t *testing.T) {
		s := Status{Model: "opus-5", Tokens: 68200, MaxTokens: 200000}
		assert.Contains(t, s.rows(NewTheme(Color256), 80)[0], "\x1b[2m")
	})
	t.Run("fills_to_budget_not_window", func(t *testing.T) {
		// a full budget (window minus reserve) fills the bar even though raw
		// usage is well under the window.
		s := Status{Tokens: 160000, MaxTokens: 200000, Reserve: 40000}
		assert.Equal(t, []string{"▓▓▓▓▓▓▓▓▓▓ 160k/200k"}, single(s, 80))
	})
	t.Run("estimated_prefixes_tilde", func(t *testing.T) {
		s := Status{Tokens: 4000, MaxTokens: 10000, Estimated: true}
		assert.Equal(t, []string{"▓▓▓▓░░░░░░ ~4k/10k"}, single(s, 120))
	})
	t.Run("zero_window_renders_no_bar", func(t *testing.T) {
		s := Status{Model: "opus-5"}
		assert.Equal(t, []string{"opus-5"}, s.rows(plain, 40)) // no MaxTokens -> no bar
	})
}

func TestUsageBar(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		pct      int
		expected string
	}{
		{"zero", 0, "░░░░░░░░░░"},
		{"partial_rounds_up", 1, "▓░░░░░░░░░"},
		{"half", 50, "▓▓▓▓▓░░░░░"},
		{"full", 100, "▓▓▓▓▓▓▓▓▓▓"},
		{"over_full_clamped", 140, "▓▓▓▓▓▓▓▓▓▓"},
		{"negative_clamped", -5, "░░░░░░░░░░"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, usageBar(tc.pct))
		})
	}
}

func TestFormatTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    int
		expected string
	}{
		{"small", 950, "950"},
		{"exact_thousand", 1000, "1k"},
		{"fractional_k", 68200, "68.2k"},
		{"exact_million", 1000000, "1M"},
		{"fractional_m", 1260000, "1.3M"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, formatTokens(tc.input))
		})
	}
}
