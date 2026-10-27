package strutil

import (
	"strings"
	"testing"
)

// BenchmarkStripANSI measures the escape filter under the shapes that reach it:
// plain and styled lines stripped per wrapped row, a contaminated stream, and a
// chunked feed carrying an open sequence across calls.
func BenchmarkStripANSI(b *testing.B) {
	for _, tc := range []struct {
		name   string
		chunks []string
	}{
		{"plain_ascii", []string{strings.Repeat("lorem ipsum dolor sit amet ", 4)}},
		{"styled", []string{"\x1b[2;48;5;236msub-1  investigating the render path\x1b[0m · \x1b[1mwrite\x1b[0m notes.go"}},
		{"contaminated", []string{"sub \x1b[2B\ttab\r\n\u009b2B\x1b]0;t\x07 world \x1b[38:5;196mred\x1b[0m"}},
		{"chunked_carry", []string{"text \x1b[38;5;", "196mred", "\x1b[0m and more text"}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for b.Loop() {
				var f ANSIFilter
				for _, c := range tc.chunks {
					f.Strip(c)
				}
			}
		})
	}
}
