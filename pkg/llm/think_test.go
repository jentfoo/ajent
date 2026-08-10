package llm

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// feedAll runs every delta through the splitter and returns the joined result.
func feedAll(s *thinkSplitter, deltas ...string) (text, thinking string) {
	var tb, kb strings.Builder
	for _, d := range deltas {
		t, k := s.Write(d)
		tb.WriteString(t)
		kb.WriteString(k)
	}
	t, k := s.Flush()
	tb.WriteString(t)
	kb.WriteString(k)
	return tb.String(), kb.String()
}

func TestThinkSplitterWrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		deltas   []string
		text     string
		thinking string
	}{
		{"plain_text", []string{"hello world"}, "hello world", ""},
		{"whole_tag_in_one_delta", []string{"<think>musing</think>answer"}, "answer", "musing"},
		{"tag_split_across_two", []string{"<thi", "nk>musing</think>ok"}, "ok", "musing"},
		{"tag_split_across_three", []string{"<th", "in", "k>musing</think>ok"}, "ok", "musing"},
		{"close_tag_split", []string{"<think>musing</thi", "nk>ok"}, "ok", "musing"},
		{"close_tag_split_three_ways", []string{"<think>a</", "thi", "nk>b"}, "b", "a"},
		{"false_start_emits_as_text", []string{"a < b"}, "a < b", ""},
		{"false_start_resolved_later", []string{"a <", "thanks"}, "a <thanks", ""},
		{"lone_angle_at_end_of_stream", []string{"done <"}, "done <", ""},
		{"partial_tag_at_end_of_stream", []string{"done <thin"}, "done <thin", ""},
		{"close_without_open_is_text", []string{"</think>plain"}, "</think>plain", ""},
		{"reasoning_only", []string{"<think>all of it"}, "", "all of it"},
		{"unterminated_thinking_flushes_as_thinking", []string{"<think>abc", "def"}, "", "abcdef"},
		{"two_regions", []string{"<think>a</think>x<think>b</think>y"}, "xy", "ab"},
		{"delta_is_exactly_the_tag", []string{"<think>", "inner", "</think>", "out"}, "out", "inner"},
		{"empty_deltas_ignored", []string{"", "hi", ""}, "hi", ""},
		{"text_before_thinking", []string{"pre<think>mid</think>post"}, "prepost", "mid"},
		{"one_byte_at_a_time", strings.Split("<think>hi</think>yo", ""), "yo", "hi"},
		{"nested_open_stays_inside", []string{"<think>a<think>b</think>c"}, "c", "a<think>b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text, thinking := feedAll(newThinkSplitter("", ""), tc.deltas...)
			assert.Equal(t, tc.text, text)
			assert.Equal(t, tc.thinking, thinking)
		})
	}

	t.Run("custom_tags", func(t *testing.T) {
		text, thinking := feedAll(newThinkSplitter("<reasoning>", "</reasoning>"),
			"<reason", "ing>why</reasoning>because")
		assert.Equal(t, "because", text)
		assert.Equal(t, "why", thinking)
	})
	t.Run("never_leaks_a_partial_tag_as_text", func(t *testing.T) {
		s := newThinkSplitter("", "")
		for _, d := range []string{"<", "t", "h", "i", "n", "k"} {
			text, _ := s.Write(d)
			assert.Empty(t, text)
		}
	})
}

func TestPartialTagLen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		buf      string
		tag      string
		expected int
	}{
		{"no_overlap", "hello", "<think>", 0},
		{"one_char", "hi <", "<think>", 1},
		{"three_chars", "hi <th", "<think>", 3},
		{"almost_whole_tag", "<think", "<think>", 6},
		{"buf_shorter_than_tag", "<t", "<think>", 2},
		{"empty_buf", "", "<think>", 0},
		{"full_tag_not_partial", "<think>", "<think>", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, partialTagLen(tc.buf, tc.tag))
		})
	}
}
