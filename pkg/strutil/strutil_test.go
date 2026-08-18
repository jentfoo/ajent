package strutil

import (
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
