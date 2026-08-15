package tui

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUINotify(t *testing.T) {
	t.Parallel()

	t.Run("commits_a_marked_line", func(t *testing.T) {
		v := newVT(80, 12)
		u := newTestUI(t, v, strings.NewReader(""))

		u.Notify("model loaded", LevelInfo)
		assert.Contains(t, u.snapshot(v), "! model loaded")
	})
	t.Run("levels_render", func(t *testing.T) {
		for _, level := range []Level{LevelInfo, LevelWarn, LevelError} {
			v := newVT(80, 12)
			u := newTestUI(t, v, strings.NewReader(""))

			u.Notify("something", level)
			assert.Contains(t, u.snapshot(v), "! something")
		}
	})
}

func TestUINotifyKeyed(t *testing.T) {
	t.Parallel()

	t.Run("same_key_collapses_in_place", func(t *testing.T) {
		v := newVT(80, 12)
		u := newTestUI(t, v, strings.NewReader(""))

		u.NotifyKeyed("scan", "scanning 1", LevelInfo)
		u.NotifyKeyed("scan", "scanning 2", LevelInfo)

		screen := u.snapshot(v)
		assert.Contains(t, screen, "scanning 2")
		assert.NotContains(t, screen, "scanning 1")
	})
	t.Run("a_commit_between_makes_it_permanent", func(t *testing.T) {
		v := newVT(80, 12)
		u := newTestUI(t, v, strings.NewReader(""))

		u.NotifyKeyed("scan", "scanning 1", LevelInfo)
		u.UserEcho("hello") // anything committed flushes the notice to history
		u.NotifyKeyed("scan", "scanning 2", LevelInfo)

		screen := u.snapshot(v)
		assert.Contains(t, screen, "scanning 1")
		assert.Contains(t, screen, "scanning 2")
	})
	t.Run("different_key_flushes_the_previous", func(t *testing.T) {
		v := newVT(80, 12)
		u := newTestUI(t, v, strings.NewReader(""))

		u.NotifyKeyed("a", "first", LevelInfo)
		u.NotifyKeyed("b", "second", LevelInfo)

		screen := u.snapshot(v)
		assert.Contains(t, screen, "first")
		assert.Contains(t, screen, "second")
	})
}

func TestUISetStatusSegment(t *testing.T) {
	t.Parallel()

	seg := func(key, text string) Segment { return Segment{Key: key, Text: text} }

	t.Run("adds_and_replaces", func(t *testing.T) {
		v := newVT(80, 12)
		u := newTestUI(t, v, strings.NewReader(""))

		u.SetStatusSegment(seg("agents", "subagents: 1"))
		assert.Contains(t, u.snapshot(v), "subagents: 1")

		u.SetStatusSegment(Segment{Key: "agents", Text: "subagents: 2", Short: "ag:2", Priority: 5})
		screen := u.snapshot(v)
		assert.Contains(t, screen, "subagents: 2")
		assert.NotContains(t, screen, "subagents: 1")
	})
	t.Run("empty_text_removes", func(t *testing.T) {
		v := newVT(80, 12)
		u := newTestUI(t, v, strings.NewReader(""))

		u.SetStatusSegment(seg("agents", "subagents: 1"))
		u.SetStatusSegment(Segment{Key: "agents"})
		assert.NotContains(t, u.snapshot(v), "subagents")
	})
}

func TestUISetModel(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	u := newTestUI(t, v, strings.NewReader(""))

	u.SetStatusSegment(Segment{Key: "agents", Text: "subagents: 1"})
	u.SetModel("lmstudio/qwen", 65536)

	screen := u.snapshot(v)
	assert.Contains(t, screen, "lmstudio/qwen")
	assert.Contains(t, screen, "subagents: 1") // segments survive a model change
}

func TestUISetTokens(t *testing.T) {
	t.Parallel()

	t.Run("updates_usage_without_touching_the_model", func(t *testing.T) {
		// a partial status update used to replace the whole struct, which put
		// the model back to whatever the caller happened to hardcode
		v := newVT(80, 12)
		u := newTestUI(t, v, strings.NewReader(""))

		u.SetModel("openrouter/z-ai/glm-5.2", 800000)
		u.SetStatusSegment(Segment{Key: "agents", Text: "subagents: 1"})
		u.SetTokens(4200)

		screen := u.snapshot(v)
		assert.Contains(t, screen, "openrouter/z-ai/glm-5.2")
		assert.Contains(t, screen, "800k")
		assert.Contains(t, screen, "subagents: 1")
		assert.Contains(t, screen, "4.2k")
	})
	t.Run("repeated_updates_keep_the_model", func(t *testing.T) {
		v := newVT(80, 12)
		u := newTestUI(t, v, strings.NewReader(""))

		u.SetModel("lmstudio/qwen", 65536)
		for i := range 5 {
			u.SetTokens(1000 * (i + 1))
		}
		assert.Contains(t, u.snapshot(v), "lmstudio/qwen")
	})
	t.Run("set_status_still_replaces_everything", func(t *testing.T) {
		v := newVT(80, 12)
		u := newTestUI(t, v, strings.NewReader(""))

		u.SetModel("lmstudio/qwen", 65536)
		u.SetStatus(Status{Tokens: 10, MaxTokens: 100})
		assert.NotContains(t, u.snapshot(v), "lmstudio/qwen")
	})
}

func TestStatusSegmentDropOrder(t *testing.T) {
	t.Parallel()

	plain := NewTheme(ColorNone)

	t.Run("everything_fits_one_row", func(t *testing.T) {
		s := Status{
			Model: "opus-5", Tokens: 68200, MaxTokens: 200000,
			Segments: []Segment{{Key: "a", Text: "plan: reviewing"}, {Key: "b", Text: "subagents: 2"}},
		}
		got := s.rows(plain, 120)
		assert.Len(t, got, 1)
		row := strings.Join(got, " ")
		assert.Contains(t, row, "opus-5")
		assert.Contains(t, row, "plan: reviewing")
		assert.Contains(t, row, "subagents: 2")
	})
	t.Run("segments_move_to_a_second_row", func(t *testing.T) {
		s := Status{
			Model: "opus-5", Tokens: 68200, MaxTokens: 200000,
			Segments: []Segment{{Key: "a", Text: "plan: reviewing"}, {Key: "b", Text: "subagents: 2"}},
		}
		got := s.rows(plain, 55)
		assert.Len(t, got, 2) // fixed row one survives; segments wrap to short-form row two
		assert.Contains(t, strings.Join(got, " "), "opus-5")
	})
	t.Run("fixed_part_never_drops_for_segments", func(t *testing.T) {
		// even at a width that clips the fixed part hard, segments do not evict it
		s := Status{
			Model: "opus-5", Tokens: 68200, MaxTokens: 200000,
			Segments: []Segment{{Key: "a", Text: "plan: reviewing"}},
		}
		got := s.rows(plain, 22)
		assert.Len(t, got, 2)
		assert.Contains(t, strings.Join(got, " "), "68.2k/200k") // token totals outlive segments
	})
	t.Run("priority_drops_lowest_first", func(t *testing.T) {
		// both short forms together overflow row two; the lower-priority one drops
		s := Status{
			Model: "opus-5", Tokens: 68200, MaxTokens: 200000,
			Segments: []Segment{
				{Key: "a", Text: "plan-reviewing", Short: "plan-reviewing-now", Priority: 10},
				{Key: "b", Text: "subagents-running", Short: "subagents-running", Priority: 1},
			},
		}
		both := displayWidth("plan-reviewing-now · subagents-running")
		singleShort := displayWidth("plan-reviewing-now")
		width := (both + singleShort) / 2 // fits one short form but not both
		got := s.rows(plain, width)
		assert.Len(t, got, 2)
		row2 := got[1]
		assert.NotContains(t, row2, "subagents") // priority 1 drops before priority 10
		assert.Contains(t, row2, "plan-reviewing-now")
	})
	t.Run("tie_drops_later_insertion_first", func(t *testing.T) {
		// equal priorities; the later insertion is dropped first (drop-last rule)
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
		got := s.rows(plain, width)
		assert.Len(t, got, 2)
		row2 := got[1]
		assert.NotContains(t, row2, "subagents") // later insertion drops first
		assert.Contains(t, row2, "plan-reviewing-now")
	})
	t.Run("short_form_used_on_row_two", func(t *testing.T) {
		// full text overflows one row; the short form appears on row two
		s := Status{
			Model: "opus-5", Tokens: 68200, MaxTokens: 200000,
			Segments: []Segment{{Key: "a", Text: "plan-reviewing-in-progress-long", Short: "pr"}},
		}
		got := s.rows(plain, 30)
		assert.Len(t, got, 2) // one row overflows, so segments take a second row
		row2 := got[1]
		assert.Equal(t, "pr", stripANSI(row2)) // short form, never the full text
	})
	t.Run("empty_segment_text_skipped", func(t *testing.T) {
		got := Status{Model: "m", Segments: []Segment{{Key: "a", Text: ""}}}.rows(plain, 80)
		assert.Equal(t, []string{"m"}, got)
	})
	t.Run("no_segments_matches_the_bar_shape", func(t *testing.T) {
		got := Status{Model: "opus-5", Tokens: 68200, MaxTokens: 200000}.rows(plain, 80)
		assert.Equal(t, []string{"▓▓▓▓░░░░░░ 68.2k/200k · opus-5"}, got)
	})
}

func TestUIPlainInteraction(t *testing.T) {
	t.Parallel()

	// plain mode has no live block, so a prompt is written to history and the
	// answer is taken from the next submitted line
	newPlainUI := func(t *testing.T, in io.Reader) (*UI, *vt) {
		t.Helper()
		v := newVT(80, 12)
		u := &UI{
			theme:  NewTheme(ColorNone),
			render: newTestInline(v),
			mode:   ModePlain,
			in:     in,
			inFd:   -1,
			msgs:   make(chan string, 4),
			done:   make(chan struct{}),
		}
		go u.readLines()
		t.Cleanup(u.Close)
		return u, v
	}

	t.Run("select_by_number", func(t *testing.T) {
		u, _ := newPlainUI(t, strings.NewReader("2\n"))

		i, err := u.Select("Pick:", []Option{{Label: "A"}, {Label: "B"}})
		require.NoError(t, err)
		assert.Equal(t, 1, i)
	})
	t.Run("input_takes_the_line", func(t *testing.T) {
		u, _ := newPlainUI(t, strings.NewReader("ajent\n"))

		got, err := u.Input("Name:", "")
		require.NoError(t, err)
		assert.Equal(t, "ajent", got)
	})
	t.Run("pick_takes_the_best_match", func(t *testing.T) {
		u, _ := newPlainUI(t, strings.NewReader("qwen\n"))

		i, err := u.Pick("Model", []PickItem{
			{Label: "anthropic/opus"}, {Label: "lmstudio/qwen"},
		}, PickOptions{})
		require.NoError(t, err)
		assert.Equal(t, 1, i)
	})
	t.Run("out_of_range_number_cancels", func(t *testing.T) {
		u, _ := newPlainUI(t, strings.NewReader("9\n"))

		_, err := u.Select("Pick:", []Option{{Label: "A"}})
		assert.ErrorIs(t, err, ErrCancelled)
	})
	t.Run("no_match_cancels", func(t *testing.T) {
		u, _ := newPlainUI(t, strings.NewReader("zzz\n"))

		_, err := u.Pick("Model", []PickItem{{Label: "opus"}}, PickOptions{})
		assert.ErrorIs(t, err, ErrCancelled)
	})
	t.Run("prompt_is_written_to_history", func(t *testing.T) {
		u, v := newPlainUI(t, strings.NewReader("1\n"))

		_, err := u.Select("Permission:", []Option{{Label: "Allow"}})
		require.NoError(t, err)
		assert.Contains(t, v.Screen(), "Permission:")
	})
}
