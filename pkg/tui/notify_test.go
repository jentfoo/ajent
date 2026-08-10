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
	t.Run("styled_when_color_enabled", func(t *testing.T) {
		t.Parallel()
		theme := NewTheme(Color256)
		assert.NotEqual(t, theme.Warn.Open(), theme.Error.Open())
		assert.NotEmpty(t, theme.Warn.Open())
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

	t.Run("adds_and_replaces", func(t *testing.T) {
		v := newVT(80, 12)
		u := newTestUI(t, v, strings.NewReader(""))

		u.SetStatusSegment("agents", "subagents: 1")
		assert.Contains(t, u.snapshot(v), "subagents: 1")

		u.SetStatusSegment("agents", "subagents: 2")
		screen := u.snapshot(v)
		assert.Contains(t, screen, "subagents: 2")
		assert.NotContains(t, screen, "subagents: 1")
	})
	t.Run("empty_text_removes", func(t *testing.T) {
		v := newVT(80, 12)
		u := newTestUI(t, v, strings.NewReader(""))

		u.SetStatusSegment("agents", "subagents: 1")
		u.SetStatusSegment("agents", "")
		assert.NotContains(t, u.snapshot(v), "subagents")
	})
	t.Run("removing_an_absent_key_is_a_no_op", func(t *testing.T) {
		v := newVT(80, 12)
		u := newTestUI(t, v, strings.NewReader(""))

		u.SetStatusSegment("nothing", "")
		assert.NotContains(t, u.snapshot(v), "nothing")
	})
}

func TestUISetModel(t *testing.T) {
	t.Parallel()

	v := newVT(80, 12)
	u := newTestUI(t, v, strings.NewReader(""))

	u.SetStatusSegment("agents", "subagents: 1")
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
		u.SetStatusSegment("agents", "subagents: 1")
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
	s := Status{
		Model: "opus-5", Tokens: 68200, MaxTokens: 200000,
		Segments: []Segment{{Key: "a", Text: "plan: reviewing"}, {Key: "b", Text: "subagents: 2"}},
	}

	t.Run("everything_fits", func(t *testing.T) {
		got := s.render(plain, 120)
		assert.Contains(t, got, "opus-5")
		assert.Contains(t, got, "plan: reviewing")
		assert.Contains(t, got, "subagents: 2")
	})
	t.Run("last_segment_drops_first", func(t *testing.T) {
		got := s.render(plain, 55)
		assert.Contains(t, got, "opus-5")
		assert.NotContains(t, got, "subagents: 2")
	})
	t.Run("context_bar_survives_longest", func(t *testing.T) {
		got := s.render(plain, 22)
		assert.Contains(t, got, "68.2k/200k") // the token totals outlive segments and model
	})
	t.Run("empty_segment_text_skipped", func(t *testing.T) {
		got := Status{Model: "m", Segments: []Segment{{Key: "a", Text: ""}}}.render(plain, 80)
		assert.Equal(t, "m", got)
	})
	t.Run("no_segments_matches_the_bar_shape", func(t *testing.T) {
		got := Status{Model: "opus-5", Tokens: 68200, MaxTokens: 200000}.render(plain, 80)
		assert.Equal(t, "▓▓▓▓░░░░░░ 68.2k/200k · opus-5", got)
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
