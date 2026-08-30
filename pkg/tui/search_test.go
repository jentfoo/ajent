package tui

import (
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchOverlayKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		setup func(*searchOverlay)
		k     key
		want  searchAction
	}{
		{"typing_stays_open", nil, key{typ: keyRune, text: "x"}, searchStay},
		{"paste_appends_query", func(s *searchOverlay) { s.items = []SearchItem{{Text: "hello"}} },
			key{typ: keyPaste, text: "he\nllo"}, searchStay},
		{"backspace_trims", func(s *searchOverlay) { s.query = "ab"; s.refilter() },
			key{typ: keyBackspace}, searchStay},
		{"kill_line_clears_query", func(s *searchOverlay) { s.query = "abc" }, key{typ: keyKillLine}, searchStay},
		{"reverse_search_steps_older", func(s *searchOverlay) {
			s.items = []SearchItem{{Text: "a"}, {Text: "b"}}
			s.refilter()
		}, key{typ: keyReverseSearch}, searchStay},
		{"up_passes_to_editor", nil, key{typ: keyUp}, searchPass},
		{"down_passes_to_editor", nil, key{typ: keyDown}, searchPass},
		{"enter_accepts_with_match", func(s *searchOverlay) {
			s.query = "hi"
			s.items = []SearchItem{{Text: "hit"}}
			s.refilter()
		}, key{typ: keyEnter}, searchAccept},
		{"enter_closes_without_match", nil, key{typ: keyEnter}, searchClose},
		{"escape_accepts_with_match", func(s *searchOverlay) {
			s.query = "hi"
			s.items = []SearchItem{{Text: "hit"}}
			s.refilter()
		}, key{typ: keyEscape}, searchAccept},
		{"escape_closes_without_match", nil, key{typ: keyEscape}, searchClose},
		{"interrupt_closes", nil, key{typ: keyInterrupt}, searchClose},
		{"unhandled_passes", nil, key{typ: keyLeft}, searchPass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &searchOverlay{}
			if tc.setup != nil {
				tc.setup(s)
			}
			assert.Equal(t, tc.want, s.key(tc.k))
		})
	}
}

func TestSearchOverlayRefilter(t *testing.T) {
	t.Parallel()

	s := &searchOverlay{
		items: []SearchItem{
			{Text: "Fix the retry loop"},
			{Text: "Add RETRY coverage"},
			{Text: "ship v2"},
		},
	}
	s.refilter()
	assert.Empty(t, s.matches)

	s.query = "RETRY"
	s.refilter() // case-insensitive substring
	texts := make([]string, 0, len(s.matches))
	for _, m := range s.matches {
		texts = append(texts, m.Text)
	}
	assert.Equal(t, []string{"Fix the retry loop", "Add RETRY coverage"}, texts)

	s.query = "zzz"
	s.refilter()
	assert.Empty(t, s.matches)
}

func TestSearchOverlayRows(t *testing.T) {
	t.Parallel()

	theme := NewTheme(ColorNone, DefaultPalette())

	t.Run("header_echoes_query", func(t *testing.T) {
		s := &searchOverlay{query: "fix", items: []SearchItem{{Text: "Fix the loop"}}}
		s.refilter()
		assert.Contains(t, s.rows(theme, 80, 8)[0], "(reverse-i-search)`fix':")
	})
	t.Run("empty_query_stays_blank", func(t *testing.T) {
		s := &searchOverlay{items: []SearchItem{{Text: "line one\nline two"}}}
		s.refilter()
		rows := s.rows(theme, 80, 8)
		assert.Len(t, rows, 1)
	})
	t.Run("shows_full_multiline_prompt", func(t *testing.T) {
		s := &searchOverlay{query: "line", items: []SearchItem{{Text: "line one\nline two\nline three"}}}
		s.refilter()
		rows := s.rows(theme, 80, 8)
		assert.Contains(t, rows[1], "line one")
		assert.Contains(t, rows[2], "line two")
		assert.Contains(t, rows[3], "line three")
	})
	t.Run("no_match_under_header", func(t *testing.T) {
		s := &searchOverlay{query: "zzz"}
		s.refilter()
		rows := s.rows(theme, 80, 8)
		assert.Equal(t, selectIndent+"no match", strutil.StripANSI(rows[1]))
	})
	t.Run("searching_while_pending", func(t *testing.T) {
		s := &searchOverlay{pending: true}
		rows := s.rows(theme, 80, 8)
		assert.Equal(t, selectIndent+"searching…", strutil.StripANSI(rows[1]))
	})
	t.Run("detail_in_header", func(t *testing.T) {
		s := &searchOverlay{query: "h", items: []SearchItem{{Text: "hi", Detail: "2026-01-02 03:04 UTC"}}}
		s.refilter()
		assert.Contains(t, s.rows(theme, 80, 8)[0], "2026-01-02 03:04 UTC")
	})
	t.Run("long_prompt_collapses_to_more", func(t *testing.T) {
		s := &searchOverlay{query: "a", items: []SearchItem{{Text: strings.Join([]string{"a", "b", "c", "d", "e"}, "\n")}}}
		s.refilter()
		rows := s.rows(theme, 80, 3)
		assert.Contains(t, strutil.StripANSI(rows[4]), moreLabel(2)) // 5 lines, budget 3 -> 2 hidden
	})
}

func TestMatchSpans(t *testing.T) {
	t.Parallel()

	assert.Nil(t, matchSpans("anything", "")) // empty query highlights nothing
	assert.Equal(t, [][2]int{{0, 3}}, matchSpans("Fix the loop", "fix"))
	// case-insensitive across the whole text; non-overlapping occurrences in order
	// second word starts at index 10 (r=10..y=14)
	assert.Equal(t, [][2]int{{0, 5}, {6, 11}}, matchSpans("retry RETRY again", "RETRY"))
	// lowering İ changes the byte length, so the spans no longer index the original
	assert.Nil(t, matchSpans("İstanbul retry", "retry"))
}

func TestSearchOverlayMatchOffset(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, query, text string
		want              int
	}{
		{"first_occurrence", "retry", "fix the retry loop", 8},
		{"case_insensitive", "RETRY", "fix the retry loop", 8},
		{"no_match_is_negative", "zzz", "fix the retry loop", -1},
		{"empty_query_is_negative", "", "fix the retry loop", -1},
		{"shifted_lowering_negative", "retry", "\u0130stanbul retry", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &searchOverlay{query: tc.query}
			assert.Equal(t, tc.want, s.matchOffset(tc.text))
		})
	}
}

func TestTrimLastCluster(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, in, want string
	}{
		{name: "empty_stays_empty"},
		{name: "plain_rune", in: "abc", want: "ab"},
		{name: "multibyte_rune", in: "aé", want: "a"},
		{name: "emoji_with_modifier", in: "hi👍🏽", want: "hi"},
		{name: "zwj_sequence", in: "x👨‍👩‍👧", want: "x"},
		{name: "combining_mark", in: "ae\u0301", want: "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, trimLastCluster(tc.in))
		})
	}
}

// The search overlay wraps every matched occurrence of the query in the accent
// style across wrapped rows, and emits no emphasis escapes on a plain terminal.
func TestSearchOverlayRowsHighlightsMatch(t *testing.T) {
	t.Parallel()

	accent := NewTheme(ColorBasic, DefaultPalette()).Accent.Open()

	type highlight struct {
		row     int
		wrapped string
	}
	cases := []struct {
		name string
		item SearchItem
		want []highlight // (row index, expected accent-wrapped substring)
	}{
		{"single_line_match",
			SearchItem{Text: "Fix the retry loop"},
			[]highlight{{1, accent + "Fix" + sgrReset}}},
		{"across_wrapped_lines", SearchItem{Text: "Fix the loop\nthen fix again"}, []highlight{
			{1, accent + "Fix" + sgrReset}, // first occurrence on its line
			{2, accent + "fix" + sgrReset}, // second occurrence wraps to next row
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &searchOverlay{query: "fix", items: []SearchItem{tc.item}}
			s.refilter()
			rows := s.rows(NewTheme(ColorBasic, DefaultPalette()), 80, 8)
			for _, want := range tc.want {
				assert.Contains(t, rows[want.row], want.wrapped)
			}
		})
	}

	t.Run("plain_terminal_no_escapes", func(t *testing.T) {
		s := &searchOverlay{query: "fix", items: []SearchItem{{Text: "Fix the retry loop"}}}
		s.refilter()
		rows := s.rows(NewTheme(ColorNone, DefaultPalette()), 80, 8)
		assert.NotContains(t, strings.Join(rows, "\n"), sgrReset) // no emphasis escapes
	})
}
func TestSearchOverlayCurrentAndCursorWrap(t *testing.T) {
	t.Parallel()

	s := &searchOverlay{query: "x", items: []SearchItem{{Text: "xa"}, {Text: "xb"}, {Text: "xc"}}}
	s.refilter() // a shared substring so all three match
	assert.Equal(t, 0, s.cursor)

	it, ok := s.current()
	require.True(t, ok)
	assert.Equal(t, "xa", it.Text) // newest first

	for i := 1; i <= 3; i++ { // older wraps around
		s.key(key{typ: keyReverseSearch})
	}
	assert.Equal(t, 0, s.cursor)

	s.cursor = 2 // at the oldest match
	s.key(key{typ: keyReverseSearch})
	assert.Equal(t, 0, s.cursor) // wraps back to the newest
}
