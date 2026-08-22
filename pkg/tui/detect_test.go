package tui

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestToneFromEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		value    string
		expected Tone
	}{
		{"dark_background", "15;0", ToneDark},
		{"light_background", "0;15", ToneLight},
		{"three_fields", "15;default;0", ToneDark},
		{"grey_seven_is_light", "0;7", ToneLight},
		{"bright_black_is_dark", "15;8", ToneDark},
		{"unset", "", ToneUnknown},
		{"single_field", "15", ToneUnknown},
		{"not_a_number", "0;default", ToneUnknown},
		{"out_of_range", "0;99", ToneUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := func(k string) string {
				if k == "COLORFGBG" {
					return tc.value
				}
				return ""
			}
			assert.Equal(t, tc.expected, toneFromEnv(env))
		})
	}
}

func TestParseBackground(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		spec     string
		expected Tone
		valid    bool
	}{
		{"white_is_light", "rgb:ffff/ffff/ffff", ToneLight, true},
		{"black_is_dark", "rgb:0000/0000/0000", ToneDark, true},
		{"short_components", "rgb:ff/ff/ff", ToneLight, true},
		{"solarized_light", "rgb:fdfd/f6f6/e3e3", ToneLight, true},
		{"solarized_dark", "rgb:0000/2b2b/3636", ToneDark, true},
		{"hex_form", "#ffffff", ToneLight, true},
		{"short_hex_form", "#000", ToneDark, true},
		{"trailing_space", "rgb:ffff/ffff/ffff ", ToneLight, true},
		{"just_below_half_is_dark", "rgb:7f7f/7f7f/7f7f", ToneDark, true},
		{"just_above_half_is_light", "rgb:8080/8080/8080", ToneLight, true},
		{"missing_component", "rgb:ffff/ffff", ToneUnknown, false},
		{"not_a_color", "rgb:zz/zz/zz", ToneUnknown, false},
		{"unknown_prefix", "cmy:1/1/1", ToneUnknown, false},
		{"empty", "", ToneUnknown, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tone, ok := parseBackground(tc.spec)
			assert.Equal(t, tc.valid, ok)
			assert.Equal(t, tc.expected, tone)
		})
	}
}

func TestDetectTone(t *testing.T) {
	t.Run("env_answers_without_query", func(t *testing.T) {
		u, out := newRecordingUI(t, strings.NewReader(""))
		swapEnv(t, map[string]string{"COLORFGBG": "0;15"})

		assert.Equal(t, ToneLight, u.DetectTone())
		assert.Empty(t, out.String())
	})
	t.Run("light_reply", func(t *testing.T) {
		u, out := newRecordingUI(t, strings.NewReader("\x1b]11;rgb:ffff/ffff/ffff\x07"))
		swapEnv(t, nil)

		assert.Equal(t, ToneLight, u.DetectTone())
		assert.Contains(t, out.String(), backgroundQuery+attrsQuery)
	})
	t.Run("dark_reply", func(t *testing.T) {
		u, _ := newRecordingUI(t, strings.NewReader("\x1b]11;rgb:1c1c/1c1c/1c1c\x1b\\"))
		swapEnv(t, nil)

		assert.Equal(t, ToneDark, u.DetectTone())
	})
	t.Run("attrs_fence_ends_wait", func(t *testing.T) {
		u, _ := newRecordingUI(t, strings.NewReader("\x1b[?62;c"))
		swapEnv(t, nil)

		assert.Equal(t, ToneUnknown, u.DetectTone())
	})
	t.Run("no_reply_times_out", func(t *testing.T) {
		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pw.Close() })
		u, _ := newRecordingUI(t, pr)
		swapEnv(t, nil)
		// fire the deadline inline; nothing will ever answer this reader
		u.afterDelay = func(_ time.Duration, fn func()) *time.Timer {
			fn()
			return time.NewTimer(time.Hour)
		}

		assert.Equal(t, ToneUnknown, u.DetectTone())
	})
	t.Run("plain_mode_never_queries", func(t *testing.T) {
		u, out := newRecordingUI(t, strings.NewReader("\x1b]11;rgb:ffff/ffff/ffff\x07"))
		u.mode = ModePlain
		swapEnv(t, nil)

		assert.Equal(t, ToneUnknown, u.DetectTone())
		assert.Empty(t, out.String())
	})
}

// swapEnv points the package env lookup at a fixed map for one test.
func swapEnv(tb testing.TB, vars map[string]string) {
	tb.Helper()
	prev := osEnv
	osEnv = func(k string) string { return vars[k] }
	tb.Cleanup(func() { osEnv = prev })
}
