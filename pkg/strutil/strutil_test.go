package strutil

import (
	"encoding/json"
	"testing"
	"time"

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
		assert.Equal(t, tc.want, trimZero(tc.in))
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
		{"abcde", 5, "abcde"},     // at the limit: unchanged
		{"abcdef", 4, "abc…"},     // cut: the ellipsis fills the last rune of the budget
		{"héllo wörld", 3, "hé…"}, // truncates on a rune boundary
		{"abcdef", 1, "…"},        // a one-rune budget holds only the ellipsis
		{"abcdef", 0, ""},         // zero width clips to nothing
		{"abcdef", -1, ""},
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

func TestHumanSize(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   int64
		want string
	}{
		{0, "0b"},
		{259, "259b"},
		{3686, "3.6kb"},
		{1024 * 1200, "1.2mb"},
		{1024*1024 - 512, "1023.5kb"}, // just below 1mb stays in kb
		{1048524, "1023.9kb"},         // last value whose kb rendering stays under 1024
		{1048575, "1mb"},              // a kb rendering of 1024kb becomes 1mb
		{1024 * 1024, "1mb"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, HumanSize(tc.in))
	}
}

func TestElapsed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{200 * time.Millisecond, "0s"},
		{41 * time.Second, "41s"},
		{2*time.Minute + 3*time.Second, "2m3s"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, Elapsed(tc.in))
	}
}
