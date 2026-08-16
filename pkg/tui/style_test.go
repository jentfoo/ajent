package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDetectColorProfile(t *testing.T) {
	t.Parallel()

	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	tests := []struct {
		name     string
		vars     map[string]string
		isTTY    bool
		expected ColorProfile
	}{
		{"not_a_tty", map[string]string{"TERM": "xterm-256color"}, false, ColorNone},
		{"no_color_set", map[string]string{"TERM": "xterm-256color", "NO_COLOR": "1"}, true, ColorNone},
		{"dumb_term", map[string]string{"TERM": "dumb"}, true, ColorNone},
		{"empty_term", map[string]string{}, true, ColorNone},
		{"truecolor", map[string]string{"TERM": "xterm", "COLORTERM": "truecolor"}, true, ColorTrue},
		{"24bit", map[string]string{"TERM": "xterm", "COLORTERM": "24bit"}, true, ColorTrue},
		{"term_256", map[string]string{"TERM": "screen-256color"}, true, Color256},
		{"basic_only", map[string]string{"TERM": "xterm"}, true, ColorBasic},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, DetectColorProfile(env(tc.vars), tc.isTTY))
		})
	}
}

func TestStyleWrap(t *testing.T) {
	t.Parallel()

	t.Run("zero_value_passthrough", func(t *testing.T) {
		var s Style
		assert.Equal(t, "hello", s.Wrap("hello"))
		assert.Empty(t, s.Open())
		assert.Empty(t, s.Close())
	})
	t.Run("wraps_with_reset", func(t *testing.T) {
		s := Style{open: sgr(attrBold)}
		assert.Equal(t, "\x1b[1mhello\x1b[0m", s.Wrap("hello"))
		assert.Equal(t, sgrReset, s.Close())
	})
	t.Run("empty_text_unchanged", func(t *testing.T) {
		s := Style{open: sgr(attrBold)}
		assert.Empty(t, s.Wrap(""))
	})
}

func TestNewTheme(t *testing.T) {
	t.Parallel()

	t.Run("color_none_is_noop", func(t *testing.T) {
		th := NewTheme(ColorNone)
		assert.Equal(t, "text", th.Thinking.Wrap("text"))
		assert.Equal(t, "text", th.DiffAdd.Wrap("text"))
	})
	t.Run("basic_uses_16_color", func(t *testing.T) {
		th := NewTheme(ColorBasic)
		assert.Equal(t, "\x1b[32m+ok\x1b[0m", th.DiffAdd.Wrap("+ok"))
	})
	t.Run("256_uses_extended", func(t *testing.T) {
		th := NewTheme(Color256)
		assert.Equal(t, "\x1b[38;5;78m+ok\x1b[0m", th.DiffAdd.Wrap("+ok"))
		assert.Equal(t, "\x1b[2;3;38;5;245mhmm\x1b[0m", th.Thinking.Wrap("hmm"))
	})
	t.Run("activity_shades_background_at_256", func(t *testing.T) {
		th := NewTheme(Color256)
		assert.Equal(t, "\x1b[2;48;5;236mrow\x1b[0m", th.Activity.Wrap("row"),
			"dim fg on a dark grey background sets sub-agent rows apart")
	})
	t.Run("activity_falls_back_to_dim_at_basic", func(t *testing.T) {
		assert.Equal(t, "\x1b[2mrow\x1b[0m", NewTheme(ColorBasic).Activity.Wrap("row"))
	})
}
