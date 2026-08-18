package strutil

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFirstLine(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"no_newline", "hello world", "hello world"},
		{"trailing_newline_stripped", "hello\n", "hello"},
		{"stops_at_first_newline", "first\nsecond\nthird", "first"},
		{"newline_at_start", "\nrest", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, FirstLine(tc.in))
		})
	}
}

func TestTrimZero(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"1.0", "1"},
		{"3.14", "3.14"}, // no trailing .0: unchanged
		{"12", "12"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, TrimZero(tc.in))
	}
}

func TestFormatTokens(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1k"},
		{12000, "12k"},
		{68_200, "68.2k"},
		{1_000_000, "1M"},
		{1_260_000, "1.3M"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, FormatTokens(tc.in))
	}
}

func TestClip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"", 4, ""},
		{"abcde", 5, "abcde"},      // at the limit: unchanged
		{"abcdef", 4, "abcd…"},     // cut appends an ellipsis rune
		{"héllo wörld", 3, "hél…"}, // truncates on a rune boundary
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, Clip(tc.in, tc.n))
	}
}

func TestFirstArgText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   json.RawMessage
		want string
	}{
		{json.RawMessage(`{"path":"a.go"}`), "a.go"},
		{json.RawMessage(`{}`), ""},
		{json.RawMessage("not json"), ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, FirstArgText(tc.in))
	}
}

func TestStripANSI(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"plain text", "plain text"},
		{"\x1b[31mred\x1b[0m", "red"}, // CSI color
		{"a\x1b]0;title\x07b", "ab"},  // OSC with BEL terminator
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, StripANSI(tc.in))
	}
}
