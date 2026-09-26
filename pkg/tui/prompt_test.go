package tui

import (
	"strconv"
	"testing"

	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRefilterMatches(t *testing.T) {
	t.Parallel()

	items := []PickItem{
		{Label: "aperture/moonshotai/kimi-k3", Detail: "Kimi K3"},
		{Label: "openrouter/google/gemini-3-pro", Detail: "Gemini 3 Pro"},
		{Label: "anthropic/claude-opus-4-5", Terms: []string{"Claude Opus 4.5"}},
	}

	tests := []struct {
		name   string
		filter string
		exp    []int
	}{
		{"empty_lists_all", "", []int{0, 1, 2}},
		{"verbatim_only", "pro", []int{1}},
		{"verbatim_in_terms", "claude opus", []int{2}},
		{"fuzzy_fallback", "amk", []int{0}},
		{"no_match", "zzzz", []int{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.exp, refilterMatches(items, tc.filter))
		})
	}

	t.Run("verbatim_outranks_fuzzy", func(t *testing.T) {
		// "kimi" is verbatim in item 0 and a subsequence of item 1
		// (moonshotai/**k**imi... vs gem**i**ni-3-pro), so only item 0 lists
		assert.Equal(t, []int{0}, refilterMatches(items, "kimi"))
	})

	t.Run("boundary_hit_ranks_first", func(t *testing.T) {
		ranked := []PickItem{
			{Label: "a-very-long-prefix-opus"},
			{Label: "opus-model"},
		}
		assert.Equal(t, []int{1, 0}, refilterMatches(ranked, "opus"))
	})
}

func TestListSection(t *testing.T) {
	t.Parallel()

	th := NewTheme(ColorNone, DefaultPalette())
	row := func(i int) string { return "item" + strconv.Itoa(i) }

	tests := []struct {
		name          string
		cursor, total int
		room          int
		want          []string
	}{
		{"all_items_fit", 0, 2, 4, []string{"item0", "item1"}},
		{"footer_takes_a_budget_row", 0, 5, 3, []string{"item0", "item1", selectIndent + moreLabel(3)}},
		{"one_row_prefers_the_cursor", 4, 9, 1, []string{"item4"}},
		{"no_room_renders_nothing", 0, 5, 0, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, listSection(th, tc.cursor, tc.total, tc.room, row))
		})
	}

	t.Run("empty_list_is_named", func(t *testing.T) {
		assert.Equal(t, []string{selectIndent + "no matches"}, listSection(th, 0, 0, 4, row))
	})

	t.Run("cursor_stays_visible_while_scrolling", func(t *testing.T) {
		for cursor := range 12 {
			rows := listSection(th, cursor, 12, 4, row)
			require.Len(t, rows, 4) // three items plus the footer
			assert.Contains(t, rows[:3], "item"+strconv.Itoa(cursor))
		}
	})
}

// optionsOf builds n single-line options.
func optionsOf(n int) []Option {
	out := make([]Option, n)
	for i := range out {
		out[i] = Option{Label: "opt-" + strconv.Itoa(i)}
	}
	return out
}

// pickItemsOf builds n single-line picker items in two contiguous groups.
func pickItemsOf(n int) []PickItem {
	out := make([]PickItem, n)
	for i := range out {
		group := "first"
		if i >= n/2 {
			group = "second"
		}
		out[i] = PickItem{Label: "model-" + strconv.Itoa(i), Group: group}
	}
	return out
}

func TestSelectStateRows(t *testing.T) {
	t.Parallel()

	th := NewTheme(ColorNone, DefaultPalette())
	s := &selectState{prompt: "Confirm?", options: optionsOf(30)}

	tests := []struct {
		name    string
		maxRows int
		want    []string
	}{
		{"one_row_keeps_the_prompt", 1, []string{"Confirm?"}},
		{"two_rows_show_one_option", 2, []string{"Confirm?", selectMarker + "opt-0"}},
		{"footer_counts_against_the_cap", 3, []string{"Confirm?", selectMarker + "opt-0", selectIndent + moreLabel(29)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows, _, _ := s.rows(th, 40, tc.maxRows)
			assert.Equal(t, tc.want, rows)
		})
	}

	t.Run("fills_the_cap_from_a_full_list", func(t *testing.T) {
		rows, _, _ := s.rows(th, 40, 6)
		require.Len(t, rows, 6) // prompt + four options + footer, never seven
		assert.Equal(t, "Confirm?", rows[0])
		assert.Equal(t, selectIndent+moreLabel(26), rows[5])
	})

	t.Run("short_list_draws_no_footer", func(t *testing.T) {
		s := &selectState{prompt: "Confirm?", options: optionsOf(3)}

		rows, _, _ := s.rows(th, 40, 6)
		assert.Equal(t, []string{"Confirm?", selectMarker + "opt-0", selectIndent + "opt-1", selectIndent + "opt-2"}, rows)
	})
}

func TestPickStateRows(t *testing.T) {
	t.Parallel()

	th := NewTheme(ColorNone, DefaultPalette())
	s := &pickState{prompt: "Model", items: pickItemsOf(30)}
	s.refilter()

	tests := []struct {
		name    string
		maxRows int
		wantLen int
	}{
		{"chrome_only_when_squeezed", 2, 2},
		{"one_row_prefers_the_cursor", 3, 3},
		{"footer_counts_against_the_cap", 4, 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows, caretRow, _ := s.rows(th, 40, tc.maxRows)
			assert.Len(t, rows, tc.wantLen)
			assert.Equal(t, 1, caretRow) // the filter row keeps its position under the header
		})
	}

	t.Run("header_survives_a_full_list", func(t *testing.T) {
		rows, _, _ := s.rows(th, 40, 6)
		require.Len(t, rows, 6)
		assert.Contains(t, strutil.StripANSI(rows[0]), "Model")
		assert.Equal(t, selectIndent+moreLabel(27), strutil.StripANSI(rows[5]))
	})

	t.Run("empty_filter_result", func(t *testing.T) {
		s := &pickState{prompt: "Model", items: pickItemsOf(30), filter: "zzz"}
		s.refilter()

		rows, _, _ := s.rows(th, 40, 6)
		require.Len(t, rows, 3) // header and filter plus the no-matches line; it fits the cap
		assert.Contains(t, strutil.StripANSI(rows[2]), "no matches")
	})
}

func TestMultiPickStateRows(t *testing.T) {
	t.Parallel()

	th := NewTheme(ColorNone, DefaultPalette())
	s := &multiPickState{prompt: "Tools", items: pickItemsOf(30), selected: make(map[int]struct{})}
	s.refilter()
	require.Greater(t, len(s.picks), len(s.items)) // group headers are rows too

	tests := []struct {
		name    string
		maxRows int
		wantLen int
	}{
		{"chrome_only_when_squeezed", 2, 2},
		{"one_row_prefers_the_cursor", 3, 3},
		{"footer_counts_against_the_cap", 4, 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows, caretRow, _ := s.rows(th, 60, tc.maxRows)
			assert.Len(t, rows, tc.wantLen)
			assert.Equal(t, 1, caretRow)
		})
	}

	t.Run("header_row_and_footer_share_the_cap", func(t *testing.T) {
		rows, _, _ := s.rows(th, 60, 7)
		require.Len(t, rows, 7)
		assert.Contains(t, strutil.StripANSI(rows[2]), "first") // the first group header is a row
		assert.Equal(t, selectIndent+moreLabel(len(s.picks)-4), strutil.StripANSI(rows[6]))
	})

	t.Run("empty_filter_result", func(t *testing.T) {
		s := &multiPickState{prompt: "Tools", items: pickItemsOf(30), filter: "zzz", selected: make(map[int]struct{})}
		s.refilter()

		rows, _, _ := s.rows(th, 60, 5)
		require.Len(t, rows, 3)
		assert.Contains(t, strutil.StripANSI(rows[2]), "no matches")
	})
}

func TestUIPickerCopy(t *testing.T) {
	t.Parallel()

	t.Run("ctrl_x_copies_highlighted_row", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		items := []PickItem{
			{Label: "user: hello", Copy: "hello world"},
			{Label: "agent: hi", Copy: "hi there"},
		}
		done := make(chan error, 1)
		go func() { _, _ = u.PickContext(t.Context(), "Rewind to", items, PickOptions{}); done <- nil }()

		waitFor(t, u, v, "Rewind to")
		press(t, pw, "\x18")
		assert.Equal(t, ControlCopySelection, <-u.Controls())

		text, ok := u.CopySelection()
		assert.True(t, ok)
		assert.Equal(t, "hello world", text)

		press(t, pw, "\x1b[B") // down: the payload follows the highlight
		waitFor(t, u, v, "> agent: hi")
		text, ok = u.CopySelection()
		assert.True(t, ok)
		assert.Equal(t, "hi there", text)
	})

	t.Run("rows_without_payload_are_ignored", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		go func() { _, _ = u.PickContext(t.Context(), "Rewind to", []PickItem{{Label: "user: hi"}}, PickOptions{}) }()

		waitFor(t, u, v, "Rewind to")
		press(t, pw, "\x18")
		assert.Equal(t, ControlCopySelection, <-u.Controls())

		_, ok := u.CopySelection()
		assert.False(t, ok)
	})

	t.Run("resolve_before_write_keeps_rows_distinct", func(t *testing.T) {
		// models the driver fix: each ctrl+x resolves its highlighted row at press
		// time (synchronously), and only the clipboard write runs later. Resolving
		// both presses before writing must keep the two rows' payloads distinct even
		// though navigation moved between them.
		u, v, pw := interactionUI(t)

		items := []PickItem{
			{Label: "user: hello", Copy: "hello world"},
			{Label: "agent: hi", Copy: "hi there"},
		}
		done := make(chan error, 1)
		go func() { _, _ = u.PickContext(t.Context(), "Rewind to", items, PickOptions{}); done <- nil }()

		waitFor(t, u, v, "Rewind to")

		// press ctrl+x on row 0 and resolve immediately (the fix resolves here,
		// before any write or navigation).
		press(t, pw, "\x18")
		assert.Equal(t, ControlCopySelection, <-u.Controls())
		first, ok := u.CopySelection()
		require.True(t, ok)

		// navigate to row 1 and press ctrl+x again; resolve immediately.
		press(t, pw, "\x1b[B")
		waitFor(t, u, v, "> agent: hi")
		press(t, pw, "\x18")
		assert.Equal(t, ControlCopySelection, <-u.Controls())
		second, ok := u.CopySelection()
		require.True(t, ok)

		// the two resolutions stayed distinct; a driver that re-resolved at write
		// time (after both presses and navigation) would collapse them to row 1.
		assert.Equal(t, "hello world", first)
		assert.Equal(t, "hi there", second)
	})

	t.Run("idle_ctrl_x_emits_nothing", func(t *testing.T) {
		u, _, pw := interactionUI(t)

		press(t, pw, "\x18")   // no picker: never a copy gesture
		press(t, pw, "\x1b[Z") // shift+tab, ordered behind: proves the ctrl+x settled
		assert.Equal(t, ControlModeCycle, <-u.Controls())
	})
}
