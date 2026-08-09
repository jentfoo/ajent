package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStatusRender(t *testing.T) {
	t.Parallel()

	plain := NewTheme(ColorNone)

	t.Run("full_line", func(t *testing.T) {
		s := Status{Model: "opus-5", Tokens: 68200, MaxTokens: 200000}
		assert.Equal(t, "ctx 34% ▓▓▓▓░░░░░░ 68.2k/200k · opus-5", s.render(plain, 80))
	})
	t.Run("model_only", func(t *testing.T) {
		assert.Equal(t, "opus-5", Status{Model: "opus-5"}.render(plain, 80))
	})
	t.Run("empty_status", func(t *testing.T) {
		assert.Empty(t, Status{}.render(plain, 80))
	})
	t.Run("truncated_to_width", func(t *testing.T) {
		s := Status{Model: "opus-5", Tokens: 68200, MaxTokens: 200000}
		assert.Equal(t, "ctx 34%", s.render(plain, 7))
	})
	t.Run("over_capacity_clamped", func(t *testing.T) {
		s := Status{Tokens: 300000, MaxTokens: 200000}
		assert.Equal(t, "ctx 100% ▓▓▓▓▓▓▓▓▓▓ 300k/200k", s.render(plain, 80))
	})
	t.Run("styled_when_color_enabled", func(t *testing.T) {
		s := Status{Model: "opus-5", Tokens: 68200, MaxTokens: 200000}
		assert.Contains(t, s.render(NewTheme(Color256), 80), "\x1b[2m")
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
