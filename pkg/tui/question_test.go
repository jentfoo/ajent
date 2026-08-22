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
	t.Run("chat_row_takes_a_typed_reply", func(t *testing.T) {
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

		waitFor(t, u, v, chatOptionLabel)
		press(t, pw, "3") // the chat row sits below the offered options
		waitFor(t, u, v, chatPlaceholder)
		press(t, pw, "neither, split it in two\r")

		a := <-result
		assert.True(t, a.Chat)
		assert.False(t, a.Declined)
		assert.Equal(t, "neither, split it in two", a.Text)
	})
	t.Run("chat_escape_returns_to_options", func(t *testing.T) {
		u, v, pw := interactionUI(t)
		ctx := t.Context()

		result := make(chan Answer, 1)
		go func() {
			a, _ := u.Ask(ctx, Question{
				Text:    "Which approach?",
				Options: []Option{{Label: "Rewrite"}, {Label: "Patch"}},
			})
			result <- a
		}()

		waitFor(t, u, v, chatOptionLabel)
		press(t, pw, "3")
		waitFor(t, u, v, chatPlaceholder)
		press(t, pw, "half typed\x1b") // Esc abandons the reply, not the question
		waitFor(t, u, v, "Rewrite")
		press(t, pw, "2")

		a := <-result
		assert.False(t, a.Chat)
		assert.False(t, a.Declined)
		assert.Equal(t, 1, a.Index)
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

func TestQuestionStateRows(t *testing.T) {
	t.Parallel()
	th := NewTheme(ColorNone, DefaultPalette())

	// widest returns the widest rendered row.
	widest := func(rows []string) int {
		var w int
		for _, r := range rows {
			w = max(w, displayWidth(r))
		}
		return w
	}

	t.Run("prompt_wraps_to_width", func(t *testing.T) {
		s := &questionState{text: "should the retry budget be shared across providers", chatIndex: -1}

		rows, _, _ := s.rows(th, 20, 8)

		require.Greater(t, len(rows), 2)
		assert.LessOrEqual(t, widest(rows), 20)
		assert.Contains(t, strings.Join(rows, " "), "providers")
	})
	t.Run("options_wrap_under_the_label", func(t *testing.T) {
		s := &questionState{
			text:      "Which?",
			options:   []Option{{Label: "share one budget across every provider"}, {Label: "Patch"}},
			chatIndex: -1,
		}

		rows, _, _ := s.rows(th, 24, 10)

		assert.LessOrEqual(t, widest(rows), 24)
		joined := strings.Join(rows, "\n")
		assert.Contains(t, joined, "provider")
		assert.Contains(t, joined, "Patch")
		// the continuation of the first option is indented under its label
		assert.Contains(t, joined, "\n"+selectIndent)
	})
	t.Run("wrapped_options_stay_within_budget", func(t *testing.T) {
		var opts []Option
		for i := 0; i < 6; i++ {
			opts = append(opts, Option{Label: "a fairly long option label " + strconv.Itoa(i)})
		}
		s := &questionState{text: "Which?", options: opts, cursor: 5, chatIndex: -1}

		rows, _, _ := s.rows(th, 24, 6)

		assert.LessOrEqual(t, len(rows), 6)
		assert.Contains(t, strings.Join(rows, "\n"), "more") // the hidden options are named
		assert.Contains(t, strings.Join(rows, "\n"), "label 5")
	})
	t.Run("typed_reply_wraps", func(t *testing.T) {
		s := &questionState{
			text:      "Which?",
			options:   []Option{{Label: "A"}},
			chatIndex: 1,
			chatting:  true,
			value:     "split it in two so the retry budget stays per provider",
		}

		rows, caret, col := s.rows(th, 20, 8)

		require.Greater(t, len(rows), 2)
		assert.LessOrEqual(t, widest(rows), 20)
		assert.Equal(t, len(rows)-1, caret) // the caret sits at the end of the last row
		assert.Equal(t, displayWidth(rows[len(rows)-1]), col)
		assert.NotContains(t, strings.Join(rows, "\n"), "A") // the option list gave way to the reply
	})
}

func TestUIPlainAsk(t *testing.T) {
	t.Parallel()

	newPlainUI := func(t *testing.T, in io.Reader) (*UI, *vt) {
		t.Helper()
		v := newVT(80, 12)
		u := &UI{
			theme:  NewTheme(ColorNone, DefaultPalette()),
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
	t.Run("non_numeric_line_is_a_chat_reply", func(t *testing.T) {
		u, _ := newPlainUI(t, strings.NewReader("neither, split it in two\n"))

		a, err := u.Ask(t.Context(), Question{
			Text:    "Approach?",
			Options: []Option{{Label: "Rewrite"}, {Label: "Patch"}},
		})
		require.NoError(t, err)
		assert.True(t, a.Chat)
		assert.Equal(t, "neither, split it in two", a.Text)
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
