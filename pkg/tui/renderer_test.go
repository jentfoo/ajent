package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func envFunc(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		want     Mode
		vars     map[string]string
		isTTY    bool
		expected Mode
	}{
		{"not_a_tty", ModeAuto, map[string]string{"TERM": "xterm"}, false, ModePlain},
		{"dumb_terminal", ModeAuto, map[string]string{"TERM": "dumb"}, true, ModePlain},
		{"no_term", ModeAuto, map[string]string{}, true, ModePlain},
		{"plain_tty_defaults_inline", ModeAuto, map[string]string{"TERM": "xterm-256color"}, true, ModeInline},
		{"tmux_env", ModeAuto, map[string]string{"TERM": "xterm", "TMUX": "/tmp/s"}, true, ModeAlt},
		{"screen_sty", ModeAuto, map[string]string{"TERM": "xterm", "STY": "1.pts"}, true, ModeAlt},
		{"screen_term", ModeAuto, map[string]string{"TERM": "screen-256color"}, true, ModeAlt},
		{"tmux_term", ModeAuto, map[string]string{"TERM": "tmux-256color"}, true, ModeAlt},
		{"explicit_inline_wins", ModeInline, map[string]string{"TERM": "xterm", "TMUX": "/tmp/s"}, true, ModeInline},
		{"explicit_alt_wins", ModeAlt, map[string]string{"TERM": "xterm"}, true, ModeAlt},
		{"non_tty_overrides_request", ModeAlt, map[string]string{"TERM": "xterm"}, false, ModePlain},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, ResolveMode(tc.want, envFunc(tc.vars), tc.isTTY))
		})
	}
}

func TestMultiplexed(t *testing.T) {
	t.Parallel()

	assert.True(t, multiplexed(envFunc(map[string]string{"TMUX": "x"})))
	assert.True(t, multiplexed(envFunc(map[string]string{"STY": "x"})))
	assert.True(t, multiplexed(envFunc(map[string]string{"TERM": "screen"})))
	assert.False(t, multiplexed(envFunc(map[string]string{"TERM": "xterm-256color"})))
	assert.False(t, multiplexed(envFunc(map[string]string{})))
}

func TestParseMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		expected Mode
		ok       bool
	}{
		{"", ModeAuto, true},
		{"auto", ModeAuto, true},
		{"inline", ModeInline, true},
		{"alt", ModeAlt, true},
		{"altscreen", ModeAlt, true},
		{"ALT", ModeAlt, true},
		{"plain", ModePlain, true},
		{"nonsense", ModeAuto, false},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			mode, ok := ParseMode(tc.input)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.expected, mode)
		})
	}
}

func TestPlainRenderer(t *testing.T) {
	t.Parallel()

	var buf strings.Builder
	p := &plainRenderer{out: &buf}
	p.commit([]histLine{{text: "one"}, {text: "two", flow: flowClip}})
	p.setLive([]string{"❯ ignored"}, 0, 0)

	assert.Equal(t, "one\ntwo\n", buf.String(), "flow is ignored and no live block")
	assert.False(t, p.scroll(1))
}

func TestHistLineRows(t *testing.T) {
	t.Parallel()

	t.Run("reflow_and_wrap_are_wrapped", func(t *testing.T) {
		want := []string{"aaaa", "bbbb"}
		assert.Equal(t, want, histLine{text: "aaaa bbbb", flow: flowReflow}.rows(6))
		assert.Equal(t, want, histLine{text: "aaaa bbbb", flow: flowWrap}.rows(6))
	})
	t.Run("clip_stays_one_row", func(t *testing.T) {
		l := histLine{text: "aaaa bbbb", flow: flowClip}
		assert.Equal(t, []string{"aaaa bbbb"}, l.rows(6))
	})
}
