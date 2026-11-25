package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/strutil"
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
		{"unhandled_passes", nil, key{typ: keyTab}, searchPass},
		{"caret_keys_select_and_move", func(s *searchOverlay) {
			s.query = "re"
			s.items = []SearchItem{{Text: "fix the retry loop"}}
			s.refilter()
		}, key{typ: keyLeft}, searchAcceptMove},
		{"caret_moves_without_match", func(s *searchOverlay) { s.query = "zzz"; s.refilter() },
			key{typ: keyRight}, searchPass},
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

func TestSearchOverlayQueryPaste(t *testing.T) {
	t.Parallel()

	s := &searchOverlay{items: []SearchItem{{Text: "hello"}}}

	assert.Equal(t, searchStay, s.key(key{typ: keyPaste, text: "he\r\nllo\x00"}))
	assert.Equal(t, "hello", s.query)
	assert.Len(t, s.matches, 1)
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
		assert.Contains(t, s.rows(theme, 80)[0], "(reverse-i-search)`fix':")
	})

	t.Run("empty_query_stays_blank", func(t *testing.T) {
		s := &searchOverlay{items: []SearchItem{{Text: "line one\nline two"}}}
		s.refilter()
		rows := s.rows(theme, 80)
		assert.Len(t, rows, 1)
	})

	t.Run("match_inline_after_echo", func(t *testing.T) {
		s := &searchOverlay{query: "line", items: []SearchItem{{Text: "line one\nline two\nline three"}}}
		s.refilter()
		rows := s.rows(theme, 80)
		require.Len(t, rows, 1)
		plain := strutil.StripANSI(rows[0])
		assert.Contains(t, plain, "(reverse-i-search)`line': line one")
		assert.Contains(t, plain, moreLinesLabel(2)) // the hidden lines stay counted
		assert.NotContains(t, plain, "line two")
	})

	t.Run("no_match_inline", func(t *testing.T) {
		s := &searchOverlay{query: "zzz"}
		s.refilter()
		rows := s.rows(theme, 80)
		assert.Len(t, rows, 1)
		assert.Contains(t, strutil.StripANSI(rows[0]), "no match")
	})

	t.Run("searching_while_pending", func(t *testing.T) {
		s := &searchOverlay{pending: true}
		rows := s.rows(theme, 80)
		assert.Len(t, rows, 1)
		assert.Contains(t, strutil.StripANSI(rows[0]), "searching…")
	})

	t.Run("detail_in_header", func(t *testing.T) {
		s := &searchOverlay{query: "h", items: []SearchItem{{Text: "hi", Detail: "2026-01-02 03:04 UTC"}}}
		s.refilter()
		assert.Contains(t, s.rows(theme, 80)[0], "2026-01-02 03:04 UTC")
	})

	t.Run("height_constant_while_stepping", func(t *testing.T) {
		s := &searchOverlay{query: "e", items: []SearchItem{
			{Text: "one line here"},
			{Text: "several\nlines\nof body text\nand one more"},
			{Text: "two\nlines only"},
		}}
		s.refilter()
		for i := 0; i <= len(s.matches); i++ {
			assert.Len(t, s.rows(theme, 80), 1) // the block height never moves
			s.key(key{typ: keyReverseSearch})
		}
	})

	t.Run("long_row_truncated_to_width", func(t *testing.T) {
		s := &searchOverlay{query: "x", items: []SearchItem{{Text: strings.Repeat("x", 200)}}}
		s.refilter()
		rows := s.rows(theme, 30)
		require.Len(t, rows, 1)
		assert.LessOrEqual(t, displayWidth(rows[0]), 30)
	})
}

func TestMatchSpans(t *testing.T) {
	t.Parallel()

	assert.Nil(t, matchSpans("anything", "")) // empty query highlights nothing
	assert.Equal(t, [][2]int{{0, 3}}, matchSpans("Fix the loop", "fix"))
	// case-insensitive across the whole text, with non-overlapping occurrences in order.
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

func TestSearchOverlayRowsHighlightsMatch(t *testing.T) {
	t.Parallel()

	accent := NewTheme(ColorBasic, DefaultPalette()).Accent.Open()

	t.Run("single_line_match", func(t *testing.T) {
		s := &searchOverlay{query: "fix", items: []SearchItem{{Text: "Fix the retry loop"}}}
		s.refilter()
		rows := s.rows(NewTheme(ColorBasic, DefaultPalette()), 80)
		require.Len(t, rows, 1)
		assert.Contains(t, rows[0], accent+"Fix"+sgrReset)
	})

	t.Run("query_on_hidden_line_not_shown", func(t *testing.T) {
		// the echo shows the first line only; a match living deeper stays hidden
		s := &searchOverlay{query: "fix", items: []SearchItem{{Text: "plan the work\nthen fix again"}}}
		s.refilter()
		rows := s.rows(NewTheme(ColorBasic, DefaultPalette()), 80)
		require.Len(t, rows, 1)
		plain := strutil.StripANSI(rows[0])
		assert.NotContains(t, plain, "then fix again")
		assert.Contains(t, plain, moreLinesLabel(1))
	})

	t.Run("plain_terminal_no_escapes", func(t *testing.T) {
		s := &searchOverlay{query: "fix", items: []SearchItem{{Text: "Fix the retry loop"}}}
		s.refilter()
		rows := s.rows(NewTheme(ColorNone, DefaultPalette()), 80)
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
