package tui

import (
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatusRows(t *testing.T) {
	t.Parallel()

	plain := NewTheme(ColorNone, DefaultPalette())

	single := func(s Status, width int) []string { return s.rows(plain, width) }

	t.Run("full_line", func(t *testing.T) {
		s := Status{Model: "opus-5", Tokens: 68200, MaxTokens: 200000}
		assert.Equal(t, []string{"▓▓▓▓░░░░░░ 68.2k/200k · opus-5"}, single(s, 80))
	})

	t.Run("model_only", func(t *testing.T) {
		assert.Equal(t, []string{"opus-5"}, Status{Model: "opus-5"}.rows(plain, 40))
	})

	t.Run("empty_status_is_one_blank_row", func(t *testing.T) {
		// the live block always reserves one status row
		assert.Equal(t, []string{""}, Status{}.rows(plain, 80))
	})

	t.Run("context_survives_narrow_width", func(t *testing.T) {
		s := Status{Model: "opus-5", Tokens: 68200, MaxTokens: 200000}
		// the fixed part alone overflows, row one is clipped to width and stays single
		assert.Equal(t, []string{"▓▓▓▓░░░░░░ 68.2k/200k"}, single(s, 21))
	})

	t.Run("over_capacity_clamped", func(t *testing.T) {
		s := Status{Tokens: 300000, MaxTokens: 100}
		// tokens past the window clamp to a full bar rather than overflowing
		assert.Equal(t, []string{"▓▓▓▓▓▓▓▓▓▓ 300k/100"}, single(s, 80))
	})

	t.Run("styled_when_color_enabled", func(t *testing.T) {
		s := Status{Model: "opus-5", Tokens: 68200, MaxTokens: 200000}
		assert.Contains(t, s.rows(NewTheme(Color256, DefaultPalette()), 80)[0], "\x1b[2m")
	})

	t.Run("fills_to_budget_not_window", func(t *testing.T) {
		// a full budget (window minus reserve) fills the bar even though raw
		// usage is well under the window.
		s := Status{Tokens: 160000, MaxTokens: 200000, Reserve: 40000}
		assert.Equal(t, []string{"▓▓▓▓▓▓▓▓▓▓ 160k/200k"}, single(s, 80))
	})

	t.Run("estimated_prefixes_tilde", func(t *testing.T) {
		s := Status{Tokens: 4000, MaxTokens: 10000, Estimated: true}
		assert.Equal(t, []string{"▓▓▓▓░░░░░░ ~4k/10k"}, single(s, 120))
	})

	t.Run("zero_window_renders_no_bar", func(t *testing.T) {
		s := Status{Model: "opus-5"}
		assert.Equal(t, []string{"opus-5"}, s.rows(plain, 40)) // no MaxTokens -> no bar
	})

	t.Run("order_places_regardless_of_insertion", func(t *testing.T) {
		s := Status{Model: "opus-5", Segments: []Segment{
			{Key: "sub", Text: "sub 2", Order: 40},
			{Key: "mode", Text: "high", Order: 20},
		}}
		assert.Equal(t, []string{"opus-5 · high · sub 2"}, single(s, 80))
	})

	t.Run("lowest_priority_collapses_first", func(t *testing.T) {
		s := Status{Model: "opus-very-long-name", Segments: []Segment{
			{Key: "reasoning", Text: "xhigh", Short: "xh", Order: 20, Priority: 0},
			{Key: "mcp", Text: "mcp: 7/14", Short: "7/14", Order: 30, Priority: 5},
		}}
		all := displayWidth("opus-very-long-name · xhigh · mcp: 7/14")
		oneCollapsed := displayWidth("opus-very-long-name · xh · mcp: 7/14")
		got := single(s, (all+oneCollapsed)/2)
		assert.Equal(t, []string{"opus-very-long-name · xh · mcp: 7/14"}, got)
	})

	t.Run("negative_priority_never_collapses", func(t *testing.T) {
		// the width fits only if "hint text" stays short, which a negative priority forbids,
		// so the positive one collapses and the negative keeps its full text
		s := Status{Model: "m", Segments: []Segment{
			{Key: "hint", Text: "again to quit", Short: "again", Order: 70, Priority: -1},
			{Key: "plan", Text: "planning now", Short: "plan", Order: 50, Priority: 2},
		}}
		want := "m · plan · again to quit" // the positive one collapses, the negative holds
		assert.Equal(t, []string{want}, single(s, displayWidth(want)))
	})

	t.Run("missing_short_keeps_full_text", func(t *testing.T) {
		// no short form means the collapse ladder skips the segment even at priority 0, so
		// the later higher-priority one is what shortens
		s := Status{Model: "m", Segments: []Segment{
			{Key: "hint", Text: "again to quit", Order: 70},
			{Key: "plan", Text: "planning now", Short: "plan", Order: 50, Priority: 2},
		}}
		want := "m · plan · again to quit"
		assert.Equal(t, []string{want}, single(s, displayWidth(want)))
	})

	t.Run("model_collapses_before_higher_priorities", func(t *testing.T) {
		// reasoning yields first, then the long model label, plan keeps its full text
		s := Status{Model: "opus-very-long-name", ModelShort: "opus", Segments: []Segment{
			{Key: "reasoning", Text: "xhigh", Short: "xh", Order: 20, Priority: 0},
			{Key: "plan", Text: "planning now", Short: "plan", Order: 50, Priority: 2},
		}}
		want := "opus · xh · planning now"
		assert.Equal(t, []string{want}, single(s, displayWidth(want)))
	})

	t.Run("two_rows_drop_lowest_priority_first", func(t *testing.T) {
		// row two overflows even fully collapsed, so the lower-priority segment leaves and
		// the negative-priority hint survives
		s := Status{Model: "opus-5", Segments: []Segment{
			{Key: "perm", Text: "allow-all modes", Short: "allow-all", Order: 60, Priority: 8},
			{Key: "hint", Text: "ctrl+c again to quit", Order: 70, Priority: -1},
		}}
		width := displayWidth("allow-all · ctrl+c again to quit") - 1 // neither short form fits
		got := single(s, width)
		require.Len(t, got, 2)
		assert.Equal(t, "opus-5", got[0])
		assert.Equal(t, "ctrl+c again to quit", got[1])
	})

	t.Run("drop_ignores_missing_short", func(t *testing.T) {
		// a missing short form only blocks collapsing, so the low-priority segment without
		// one is what leaves row two while the higher-priority one shortens to fit
		s := Status{Model: "opus-5", Segments: []Segment{
			{Key: "reasoning", Text: "xhigh", Order: 20},
			{Key: "perm", Text: "allow-all modes", Short: "allow", Order: 60, Priority: 8},
		}}
		got := single(s, displayWidth("xhigh · allow")-1)
		require.Len(t, got, 2)
		assert.Equal(t, "allow", got[1])
	})

	t.Run("everything_fits_one_row", func(t *testing.T) {
		s := Status{
			Model: "opus-5", Tokens: 68200, MaxTokens: 200000,
			Segments: []Segment{{Key: "a", Text: "plan: reviewing"}, {Key: "b", Text: "subagents: 2"}},
		}
		got := single(s, 120)
		assert.Len(t, got, 1)
		row := strings.Join(got, " ")
		assert.Contains(t, row, "opus-5")
		assert.Contains(t, row, "plan: reviewing")
		assert.Contains(t, row, "subagents: 2")
	})

	t.Run("model_shortens_before_segments_split", func(t *testing.T) {
		s := Status{
			Model: "openrouter/z-ai/glm-5.2", ModelShort: "glm-5.2",
			Tokens: 68200, MaxTokens: 200000,
			Segments: []Segment{{Key: "a", Text: "plan: reviewing"}, {Key: "b", Text: "subagents: 2"}},
		}
		got := single(s, 70) // full model overflows, the short one still fits one row
		assert.Len(t, got, 1)
		assert.Contains(t, got[0], "glm-5.2") // the model shortens, never vanishes
		assert.NotContains(t, got[0], "openrouter")
		assert.Contains(t, got[0], "plan: reviewing") // segments stay at full text
		assert.Contains(t, got[0], "subagents: 2")
	})

	t.Run("segments_shorten_before_splitting", func(t *testing.T) {
		// full segments overflow the one-row attempt, but their short forms fit it:
		// the block must stay on one row rather than splitting
		s := Status{
			Model: "openrouter/z-ai/glm-5.2", ModelShort: "glm-5.2",
			Tokens: 68200, MaxTokens: 200000,
			Segments: []Segment{{Key: "a", Text: "plan: reviewing", Short: "plan"}},
		}
		got := single(s, 40)
		assert.Len(t, got, 1)
		assert.Contains(t, got[0], "glm-5.2") // the model is short
		assert.Contains(t, got[0], "plan")    // and so is the segment
		assert.NotContains(t, got[0], "reviewing")
	})

	t.Run("segments_expand_on_second_row", func(t *testing.T) {
		s := Status{
			Model: "opus-5", Tokens: 68200, MaxTokens: 200000,
			Segments: []Segment{{Key: "a", Text: "plan: reviewing"}, {Key: "b", Text: "subagents: 2"}},
		}
		got := single(s, 40) // even without the model one row cannot fit
		assert.Len(t, got, 2)
		assert.Contains(t, got[0], "opus-5")          // row one keeps the model while it fits
		assert.Contains(t, got[1], "plan: reviewing") // row two uses full text while it fits
		assert.Contains(t, got[1], "subagents: 2")
	})

	t.Run("row_one_shortens_model_before_clipping", func(t *testing.T) {
		s := Status{
			Model: "openrouter/z-ai/glm-5.2", ModelShort: "glm-5.2",
			Tokens: 68200, MaxTokens: 200000,
			Segments: []Segment{{Key: "a", Text: "plan: reviewing"}},
		}
		got := single(s, 31) // fixed plus full model overflow, the short one fits
		assert.Len(t, got, 2)
		assert.Contains(t, got[0], "glm-5.2") // the model shortens, not the token count
		assert.NotContains(t, got[0], "openrouter")
		assert.Contains(t, got[0], "68.2k/200k")
	})

	t.Run("fixed_part_never_drops_for_segments", func(t *testing.T) {
		// even at a width that clips the fixed part hard, segments do not evict it
		s := Status{
			Model: "opus-5", Tokens: 68200, MaxTokens: 200000,
			Segments: []Segment{{Key: "a", Text: "plan: reviewing"}},
		}
		got := single(s, 22)
		assert.Len(t, got, 2)
		assert.Contains(t, strings.Join(got, " "), "68.2k/200k") // token totals outlive segments
	})

	t.Run("priority_drops_lowest_first", func(t *testing.T) {
		// both short forms together overflow row two, the lower-priority one drops
		s := Status{
			Model: "opus-5", Tokens: 68200, MaxTokens: 200000,
			Segments: []Segment{
				{Key: "a", Text: "plan-reviewing", Short: "plan-reviewing-now", Priority: 10},
				{Key: "b", Text: "subagents-running", Short: "subagents-running", Priority: 1},
			},
		}
		both := displayWidth("plan-reviewing-now · subagents-running")
		singleShort := displayWidth("plan-reviewing-now")
		width := (both + singleShort) / 2 // fits one segment but not both
		got := single(s, width)
		assert.Len(t, got, 2)
		row2 := got[1]
		assert.NotContains(t, row2, "subagents")   // priority 1 drops before priority 10
		assert.Contains(t, row2, "plan-reviewing") // the survivor expands to full text
	})

	t.Run("tie_drops_later_insertion_first", func(t *testing.T) {
		// equal priorities, so the later insertion is dropped first (drop-last rule)
		s := Status{
			Model: "opus-5", Tokens: 68200, MaxTokens: 200000,
			Segments: []Segment{
				{Key: "a", Text: "plan-reviewing", Short: "plan-reviewing-now"},
				{Key: "b", Text: "subagents-running", Short: "subagents-running"},
			},
		}
		both := displayWidth("plan-reviewing-now · subagents-running")
		singleShort := displayWidth("subagents-running")
		width := (both + singleShort) / 2
		got := single(s, width)
		assert.Len(t, got, 2)
		row2 := got[1]
		assert.NotContains(t, row2, "subagents")   // later insertion drops first
		assert.Contains(t, row2, "plan-reviewing") // the survivor expands to full text
	})

	t.Run("short_form_used_when_full_does_not_fit", func(t *testing.T) {
		// the single segment overflows row two at full text, its short form is used
		s := Status{
			Model: "opus-5", Tokens: 68200, MaxTokens: 200000,
			Segments: []Segment{{Key: "a", Text: "plan-reviewing-in-progress-long", Short: "pr"}},
		}
		got := single(s, 30)
		assert.Len(t, got, 2) // one row overflows even without the model, so it splits
		row2 := got[1]
		assert.Equal(t, "pr", strutil.StripANSI(row2)) // short form, never a clipped full text
	})

	t.Run("empty_segment_text_skipped", func(t *testing.T) {
		got := single(Status{Model: "m", Segments: []Segment{{Key: "a", Text: ""}}}, 80)
		assert.Equal(t, []string{"m"}, got)
	})
}

func TestUsageBar(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		pct      int
		expected string
	}{
		{"zero", 0, "░░░░░░░░░░"},
		{"partial_rounds_up", 1, "▓░░░░░░░░░"},
		{"half", 50, "▓▓▓▓▓░░░░░"},
		{"full", 100, "▓▓▓▓▓▓▓▓▓▓"},
		{"over_full_clamped", 140, "▓▓▓▓▓▓▓▓▓▓"},
		{"negative_clamped", -5, "░░░░░░░░░░"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, usageBar(tc.pct))
		})
	}

	// the status row budgets statusBarCells columns for the bar, so both glyphs
	// have to measure one column at every fill or the row overruns its width
	t.Run("bar_glyphs_are_one_column", func(t *testing.T) {
		assert.Equal(t, 1, displayWidth(barFull))
		assert.Equal(t, 1, displayWidth(barEmpty))
	})

	t.Run("bar_spans_its_cells", func(t *testing.T) {
		for pct := -5; pct <= 140; pct += 5 {
			assert.Equal(t, statusBarCells, displayWidth(usageBar(pct)))
		}
	})

	t.Run("separator_is_three_columns", func(t *testing.T) {
		assert.Equal(t, 3, displayWidth(statusSep))
	})
}
