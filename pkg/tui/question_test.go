package tui

import (
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUIAsk(t *testing.T) {
	t.Parallel()

	t.Run("free_text_collects_typed", func(t *testing.T) {
		u, v, pw := interactionUI(t)
		ctx := t.Context()

		result := make(chan Answer, 1)
		go func() {
			a, err := u.Ask(ctx, Question{Text: "What should I name the branch?"})
			assert.NoError(t, err)
			result <- a
		}()

		waitFor(t, u, v, "What should I name the branch?")
		press(t, pw, "feature/x")
		waitFor(t, u, v, "feature/x")
		press(t, pw, "\r")

		a := <-result
		assert.False(t, a.Declined)
		assert.Equal(t, "feature/x", a.Text)
	})
	t.Run("offered_options_selects", func(t *testing.T) {
		u, v, pw := interactionUI(t)
		ctx := t.Context()

		result := make(chan Answer, 1)
		go func() {
			a, err := u.Ask(ctx, Question{
				Text:    "Which approach?",
				Options: []Option{{Label: "Rewrite"}, {Label: "Patch"}},
			})
			assert.NoError(t, err)
			result <- a
		}()

		waitFor(t, u, v, "Which approach?")
		press(t, pw, "\x1b[B") // down to Patch
		waitFor(t, u, v, "> Patch")
		press(t, pw, "\r")

		a := <-result
		assert.False(t, a.Declined)
		assert.Equal(t, 1, a.Index)
	})
	t.Run("number_key_selects_directly", func(t *testing.T) {
		u, v, pw := interactionUI(t)
		ctx := t.Context()

		result := make(chan Answer, 1)
		go func() {
			a, _ := u.Ask(ctx, Question{
				Text:    "Pick a plan",
				Options: []Option{{Label: "A"}, {Label: "B"}},
			})
			result <- a
		}()

		waitFor(t, u, v, "Pick a plan")
		press(t, pw, "2")

		a := <-result
		assert.Equal(t, 1, a.Index)
	})
	t.Run("multi_line_prompt_renders", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		ctx := t.Context()
		go func() { _, _ = u.Ask(ctx, Question{Text: "Line one\nLine two"}) }()

		waitFor(t, u, v, "Line one")
		waitFor(t, u, v, "Line two")
		press(t, pw, "\r")
	})
	t.Run("long_prompt_elides_within_cap", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		var many strings.Builder
		for i := 0; i < 12; i++ {
			many.WriteString("question line " + strconv.Itoa(i) + "\n")
		}
		ctx := t.Context()
		go func() { _, _ = u.Ask(ctx, Question{Text: many.String()}) }()

		waitFor(t, u, v, "… +9 lines")
		// the live block stays inside its share of a 12 row screen, divider included
		require.Eventually(t, func() bool {
			return liveRowCount(u.snapshot(v)) <= 8
		}, time.Second, testPoll)
		press(t, pw, "\r")
	})
	t.Run("escape_declines_with_no_error", func(t *testing.T) {
		u, v, pw := interactionUI(t)
		ctx := t.Context()

		result := make(chan Answer, 1)
		errCh := make(chan error, 1)
		go func() {
			a, err := u.Ask(ctx, Question{Text: "Proceed?"})
			result <- a
			errCh <- err
		}()

		waitFor(t, u, v, "Proceed?")
		press(t, pw, "\x1b")

		require.NoError(t, <-errCh)
		a := <-result
		assert.True(t, a.Declined)
	})
	t.Run("commits_one_summary_line", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		ctx := t.Context()
		go func() {
			_, _ = u.Ask(ctx, Question{Text: "Color?", Options: []Option{{Label: "Red"}}})
		}()

		waitFor(t, u, v, "Color?")
		press(t, pw, "\r")

		// an answered question echoes no summary line (the caller logs its outcome)
		assert.NotContains(t, strutil.StripANSI(u.snapshot(v)), "answer:")
		// the live block reverted to the input prompt
		waitFor(t, u, v, userMarker)
	})
	t.Run("queues_behind_a_select", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		go func() { _, _ = u.Select("First:", []Option{{Label: "A"}}) }()
		waitFor(t, u, v, "First:")

		ctx := t.Context()
		result := make(chan Answer, 1)
		go func() {
			a, _ := u.Ask(ctx, Question{Text: "Second?"})
			result <- a
		}()

		assert.NotContains(t, u.snapshot(v), "Second?")
		waitFor(t, u, v, "+1 waiting")

		press(t, pw, "\r") // resolve the select, promoting the question
		waitFor(t, u, v, "Second?")

		press(t, pw, "yes\r")
		assert.Equal(t, "yes", (<-result).Text)
	})
}

func TestUIPlainAsk(t *testing.T) {
	t.Parallel()

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

	t.Run("free_text_takes_the_line", func(t *testing.T) {
		u, _ := newPlainUI(t, strings.NewReader("feature/x\n"))

		a, err := u.Ask(t.Context(), Question{Text: "Name?"})
		require.NoError(t, err)
		assert.Equal(t, "feature/x", a.Text)
	})
	t.Run("number_selects_an_option", func(t *testing.T) {
		u, _ := newPlainUI(t, strings.NewReader("2\n"))

		a, err := u.Ask(t.Context(), Question{
			Text:    "Approach?",
			Options: []Option{{Label: "Rewrite"}, {Label: "Patch"}},
		})
		require.NoError(t, err)
		assert.Equal(t, 1, a.Index)
	})
	t.Run("out_of_range_number_cancels", func(t *testing.T) {
		u, _ := newPlainUI(t, strings.NewReader("9\n"))

		a, err := u.Ask(t.Context(), Question{
			Text:    "Approach?",
			Options: []Option{{Label: "Rewrite"}},
		})
		require.ErrorIs(t, err, ErrCancelled)
		assert.False(t, a.Declined)
	})
}

func TestUIAskNoUITerminal(t *testing.T) {
	t.Parallel()

	t.Run("closed_ui_returns_no_ui_error", func(t *testing.T) {
		u, _, _ := interactionUI(t)
		u.Close()

		a, err := u.Ask(t.Context(), Question{Text: "Proceed?"})
		require.ErrorIs(t, err, ErrNoUI)
		assert.False(t, a.Declined)
	})
}
