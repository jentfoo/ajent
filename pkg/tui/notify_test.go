package tui

import (
	"io"
	"slices"
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

	t.Run("levels_styled_distinctly", func(t *testing.T) {
		th := NewTheme(Color256, DefaultPalette())
		for _, tc := range []struct {
			name  string
			level Level
			style Style
		}{
			{"info", LevelInfo, th.Dim},
			{"warn", LevelWarn, th.Warn},
			{"error", LevelError, th.Error},
		} {
			t.Run(tc.name, func(t *testing.T) {
				u := withTimer(&UI{theme: th})

				got := u.noticeLine("something", tc.level)
				assert.Equal(t, tc.style.Wrap(noticeMarker+" "+"something"), got)
			})
		}
	})
}

func TestUINotifyKeyed(t *testing.T) {
	t.Parallel()

	// countLines counts screen rows containing sub, so a duplicated live-block
	// ghost (the invariant-3 bug) is caught rather than hidden by assert.Contains.
	countLines := func(screen, sub string) int {
		var n int
		for _, l := range strings.Split(screen, "\n") {
			if strings.Contains(l, sub) {
				n++
			}
		}
		return n
	}

	t.Run("same_key_collapses_in_place", func(t *testing.T) {
		v := newVT(80, 12)
		u := newTestUI(t, v, strings.NewReader(""))

		u.NotifyKeyed("scan", "scanning 1", LevelInfo)
		u.NotifyKeyed("scan", "scanning 2", LevelInfo)

		screen := u.snapshot(v)
		assert.Contains(t, screen, "scanning 2")
		assert.NotContains(t, screen, "scanning 1")
	})

	t.Run("flushed_notice_has_no_live_ghost", func(t *testing.T) {
		v := newVT(80, 12)
		u := newTestUI(t, v, strings.NewReader(""))

		u.NotifyKeyed("scan", "scanning 1", LevelInfo)
		u.UserEcho("hello") // flushes the notice and does not repaint afterwards

		// snapshot immediately: a later same-key notify would repaint r.live and
		// hide a duplicate a buggy flush left below the committed content
		assert.Equal(t, 1, countLines(u.snapshot(v), "scanning 1"))
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
		// a commit must drop the notice from r.live before it lands in history,
		// or the inline renderer recomposes it below the new content as a duplicate
		assert.Equal(t, 1, countLines(screen, "scanning 1"))
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
	u.SetModel("lmstudio/qwen", "qwen", 65536)

	screen := u.snapshot(v)
	assert.Contains(t, screen, "lmstudio/qwen")
	assert.Contains(t, screen, "subagents: 1") // segments survive a model change
}

func TestUISetStatusReplacesEverything(t *testing.T) {
	t.Parallel()

	t.Run("set_status_still_replaces_everything", func(t *testing.T) {
		v := newVT(80, 12)
		u := newTestUI(t, v, strings.NewReader(""))

		u.SetModel("lmstudio/qwen", "qwen", 65536)
		u.SetStatus(Status{Tokens: 10, MaxTokens: 100})
		assert.NotContains(t, u.snapshot(v), "lmstudio/qwen")
	})
}

func TestUISetStatusAliasing(t *testing.T) {
	t.Parallel()

	// storedSegments reads the UI's own copy of the segments under the lock.
	storedSegments := func(u *UI) []Segment {
		u.mu.Lock()
		defer u.mu.Unlock()
		return slices.Clone(u.status.Segments)
	}

	t.Run("edits_do_not_leak", func(t *testing.T) {
		v := newVT(80, 12)
		u := newTestUI(t, v, strings.NewReader(""))

		segs := []Segment{{Key: "a", Text: "alpha"}, {Key: "b", Text: "beta"}}
		u.SetStatus(Status{Model: "m", Segments: segs})
		segs[0].Text = "hacked" // a host reusing its slice must not reach the UI

		assert.Equal(t, []Segment{{Key: "a", Text: "alpha"}, {Key: "b", Text: "beta"}}, storedSegments(u))
	})

	t.Run("removal_keeps_caller_slice", func(t *testing.T) {
		v := newVT(80, 12)
		u := newTestUI(t, v, strings.NewReader(""))

		segs := []Segment{{Key: "a", Text: "alpha"}, {Key: "b", Text: "beta"}}
		u.SetStatus(Status{Model: "m", Segments: segs})

		// empty Text removes, shifting elements in place
		u.SetStatusSegment(Segment{Key: "a"})
		assert.Equal(t, []Segment{{Key: "a", Text: "alpha"}, {Key: "b", Text: "beta"}}, segs)
		screen := u.snapshot(v)
		assert.Contains(t, screen, "beta")
		assert.NotContains(t, screen, "alpha")
	})

	t.Run("append_spares_caller_capacity", func(t *testing.T) {
		v := newVT(80, 12)
		u := newTestUI(t, v, strings.NewReader(""))

		segs := make([]Segment, 0, 4)
		segs = append(segs, Segment{Key: "a", Text: "alpha"})
		u.SetStatus(Status{Model: "m", Segments: segs})

		// a keyed add must not write into the spare capacity past the caller's
		// len, which reusing the backing array here would expose
		u.SetStatusSegment(Segment{Key: "b", Text: "beta"})
		reused := append(segs[:0], Segment{Key: "c", Text: "gamma"})

		assert.Equal(t, []Segment{{Key: "a", Text: "alpha"}, {Key: "b", Text: "beta"}}, storedSegments(u))
		assert.Equal(t, []Segment{{Key: "c", Text: "gamma"}}, reused)
	})
}

func TestUIPlainInteraction(t *testing.T) {
	t.Parallel()

	// plain mode has no live block, so a prompt is written to history and the
	// answer is taken from the next submitted line
	newPlainUI := func(t *testing.T, in io.Reader) (*UI, *vt) {
		t.Helper()

		v := newVT(80, 12)
		u := withTimer(&UI{
			theme:  NewTheme(ColorNone, DefaultPalette()),
			render: newTestInline(v),
			mode:   ModePlain,
			in:     in,
			inFd:   -1,
			msgs:   make(chan string, 4),
			done:   make(chan struct{}),
		})
		go u.readLines()
		t.Cleanup(u.Close)
		return u, v
	}

	t.Run("select_by_number", func(t *testing.T) {
		u, _ := newPlainUI(t, strings.NewReader("2\n"))

		i, err := u.SelectContext(t.Context(), "Pick:", []Option{{Label: "A"}, {Label: "B"}})
		require.NoError(t, err)
		assert.Equal(t, 1, i)
	})

	t.Run("input_takes_the_line", func(t *testing.T) {
		u, _ := newPlainUI(t, strings.NewReader("ajent\n"))

		got, err := u.InputContext(t.Context(), "Name:", "")
		require.NoError(t, err)
		assert.Equal(t, "ajent", got)
	})

	t.Run("pick_takes_the_best_match", func(t *testing.T) {
		u, _ := newPlainUI(t, strings.NewReader("qwen\n"))

		i, err := u.PickContext(t.Context(), "Model", []PickItem{
			{Label: "anthropic/opus"}, {Label: "lmstudio/qwen"},
		}, PickOptions{})
		require.NoError(t, err)
		assert.Equal(t, 1, i)
	})

	t.Run("out_of_range_number_cancels", func(t *testing.T) {
		u, _ := newPlainUI(t, strings.NewReader("9\n"))

		_, err := u.SelectContext(t.Context(), "Pick:", []Option{{Label: "A"}})
		assert.ErrorIs(t, err, ErrCancelled)
	})

	t.Run("no_match_cancels", func(t *testing.T) {
		u, _ := newPlainUI(t, strings.NewReader("zzz\n"))

		_, err := u.PickContext(t.Context(), "Model", []PickItem{{Label: "opus"}}, PickOptions{})
		assert.ErrorIs(t, err, ErrCancelled)
	})

	t.Run("prompt_is_written_to_history", func(t *testing.T) {
		u, v := newPlainUI(t, strings.NewReader("1\n"))

		_, err := u.SelectContext(t.Context(), "Permission:", []Option{{Label: "Allow"}})
		require.NoError(t, err)
		assert.Contains(t, v.Screen(), "Permission:")
	})
}
