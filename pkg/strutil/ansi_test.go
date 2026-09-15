package strutil

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStripANSI(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain_text", "plain text", "plain text"},
		{"csi_color", "\x1b[31mred\x1b[0m", "red"},
		{"osc_bel_terminator", "a\x1b]0;title\x07b", "ab"},
		{"osc_st_terminator", "a\x1b]0;title\x1b\\b", "ab"},
		{"two_byte_escape", "a\x1bMb", "ab"},
		{"repeated_esc", "a\x1b\x1b[31mb", "ab"},
		{"trailing_esc_dropped", "a\x1b", "a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, StripANSI(tc.in))
		})
	}
}

func TestANSIFilter(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		chunks []string
		want   string
	}{
		{"csi_split_mid_params", []string{"\x1b[3", "1mred\x1b[0m"}, "red"},
		{"esc_alone_in_chunk", []string{"red\x1b", "[31mx", "\x1b[0m!"}, "redx!"},
		{"csi_split_before_final", []string{"\x1b[31", "m", "done"}, "done"},
		{"osc_split_across_chunks", []string{"a\x1b]0;ti", "tle\x07b"}, "ab"},
		{"osc_st_split_after_esc", []string{"a\x1b]0;t\x1b", "\\b"}, "ab"},
		{"plain_chunks_pass_through", []string{"one ", "two\n"}, "one two\n"},
		{"late_final_byte_resumes_text", []string{"a\x1b[31;", ";4", "mseen"}, "aseen"},
		{"unterminated_stays_dropped", []string{"a\x1b[31", ";;;"}, "a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f ANSIFilter
			var got strings.Builder
			for _, c := range tc.chunks {
				got.WriteString(f.Strip(c))
			}
			assert.Equal(t, tc.want, got.String())
		})
	}

	// any chunk boundary gives the same text as stripping the whole stream once
	t.Run("splits_match_whole_strip", func(t *testing.T) {
		stream := "a\x1b[31mred\x1b[0m b\x1b]0;title\x07c\x1bM d\x1b[K\nend"
		for i := 0; i <= len(stream); i++ {
			var f ANSIFilter
			got := f.Strip(stream[:i]) + f.Strip(stream[i:])
			assert.Equal(t, StripANSI(stream), got)
		}
	})
}
