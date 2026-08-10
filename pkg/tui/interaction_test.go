package tui

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// interactionUI drives a UI with a writable key pipe, for prompt tests.
func interactionUI(t *testing.T) (*UI, *vt, *io.PipeWriter) {
	t.Helper()

	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	return newTestUI(t, v, pr), v, pw
}

// press writes raw bytes as if typed.
func press(t *testing.T, pw *io.PipeWriter, s string) {
	t.Helper()

	_, err := io.WriteString(pw, s)
	require.NoError(t, err)
}

// waitFor blocks until the screen satisfies cond.
func waitFor(t *testing.T, u *UI, v *vt, want string) {
	t.Helper()

	require.Eventually(t, func() bool {
		return strings.Contains(u.snapshot(v), want)
	}, time.Second, testPoll, "screen never showed %q", want)
}

func TestUISelect(t *testing.T) {
	t.Parallel()

	t.Run("renders_and_returns_the_choice", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		result := make(chan int, 1)
		go func() {
			i, err := u.Select("Permission:", []Option{{Label: "Allow"}, {Label: "Deny"}})
			assert.NoError(t, err)
			result <- i
		}()

		waitFor(t, u, v, "Permission:")
		waitFor(t, u, v, "Allow")

		press(t, pw, "\x1b[B") // down
		waitFor(t, u, v, "> Deny")
		press(t, pw, "\r")

		assert.Equal(t, 1, <-result)
	})
	t.Run("escape_cancels", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		errCh := make(chan error, 1)
		go func() {
			_, err := u.Select("Pick:", []Option{{Label: "A"}, {Label: "B"}})
			errCh <- err
		}()

		waitFor(t, u, v, "Pick:")
		press(t, pw, "\x1b")

		assert.ErrorIs(t, <-errCh, ErrCancelled)
	})
	t.Run("number_key_selects_directly", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		result := make(chan int, 1)
		go func() {
			i, _ := u.Select("Pick:", []Option{{Label: "A"}, {Label: "B"}, {Label: "C"}})
			result <- i
		}()

		waitFor(t, u, v, "Pick:")
		press(t, pw, "3")

		assert.Equal(t, 2, <-result)
	})
	t.Run("commits_one_summary_line", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		done := make(chan struct{})
		go func() {
			_, _ = u.Select("Permission:", []Option{{Label: "Allow"}})
			close(done)
		}()

		waitFor(t, u, v, "Permission:")
		press(t, pw, "\r")
		<-done

		waitFor(t, u, v, "! Permission: Allow")
		// the live block reverted to the input prompt
		waitFor(t, u, v, userMarker)
	})
	t.Run("cursor_wraps_at_the_ends", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		result := make(chan int, 1)
		go func() {
			i, _ := u.Select("Pick:", []Option{{Label: "A"}, {Label: "B"}})
			result <- i
		}()

		waitFor(t, u, v, "Pick:")
		press(t, pw, "\x1b[A") // up from the first entry
		waitFor(t, u, v, "> B")
		press(t, pw, "\r")

		assert.Equal(t, 1, <-result)
	})
	t.Run("empty_options_cancel", func(t *testing.T) {
		u, _, _ := interactionUI(t)
		_, err := u.Select("Pick:", nil)
		assert.ErrorIs(t, err, ErrCancelled)
	})
}

func TestUIConfirm(t *testing.T) {
	t.Parallel()

	u, v, pw := interactionUI(t)

	result := make(chan bool, 1)
	go func() {
		ok, _ := u.Confirm("Proceed?")
		result <- ok
	}()

	waitFor(t, u, v, "Proceed?")
	press(t, pw, "\r") // Yes is first

	assert.True(t, <-result)
}

func TestUIInputPrompt(t *testing.T) {
	t.Parallel()

	t.Run("collects_typed_text", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		result := make(chan string, 1)
		go func() {
			s, err := u.Input("Name:", "your name")
			assert.NoError(t, err)
			result <- s
		}()

		waitFor(t, u, v, "Name:")
		press(t, pw, "ajent")
		waitFor(t, u, v, "ajent")
		press(t, pw, "\r")

		assert.Equal(t, "ajent", <-result)
	})
	t.Run("backspace_deletes", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		result := make(chan string, 1)
		go func() {
			s, _ := u.Input("Name:", "")
			result <- s
		}()

		waitFor(t, u, v, "Name:")
		press(t, pw, "abc\x7f")
		waitFor(t, u, v, "ab")
		press(t, pw, "\r")

		assert.Equal(t, "ab", <-result)
	})
}

func TestUIPick(t *testing.T) {
	t.Parallel()

	items := []PickItem{
		{Label: "anthropic/claude-opus-4-5", Detail: "200k", Terms: []string{"opus"}},
		{Label: "lmstudio/qwen3.6-27b", Detail: "65k", Terms: []string{"qwen"}},
		{Label: "openrouter/z-ai/glm-5.2", Detail: "800k"},
	}

	t.Run("filters_as_you_type", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		result := make(chan int, 1)
		go func() {
			i, err := u.Pick("Model", items, PickOptions{})
			assert.NoError(t, err)
			result <- i
		}()

		waitFor(t, u, v, "Model")
		press(t, pw, "qwen")
		waitFor(t, u, v, "lmstudio/qwen3.6-27b")
		press(t, pw, "\r")

		assert.Equal(t, 1, <-result)
	})
	t.Run("matches_hidden_terms", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		result := make(chan int, 1)
		go func() {
			i, _ := u.Pick("Model", items, PickOptions{})
			result <- i
		}()

		waitFor(t, u, v, "Model")
		press(t, pw, "opus") // an alias, not shown in the row
		waitFor(t, u, v, "anthropic/claude-opus")
		press(t, pw, "\r")

		assert.Equal(t, 0, <-result)
	})
	t.Run("initial_selection_honoured", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		result := make(chan int, 1)
		go func() {
			i, _ := u.Pick("Model", items, PickOptions{Initial: 2})
			result <- i
		}()

		waitFor(t, u, v, "> openrouter/z-ai/glm-5.2")
		press(t, pw, "\r")

		assert.Equal(t, 2, <-result)
	})
	t.Run("no_matches_blocks_enter", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		errCh := make(chan error, 1)
		go func() {
			_, err := u.Pick("Model", items, PickOptions{})
			errCh <- err
		}()

		waitFor(t, u, v, "Model")
		press(t, pw, "zzzz")
		waitFor(t, u, v, "no matches")
		press(t, pw, "\r") // does nothing
		press(t, pw, "\x1b")

		assert.ErrorIs(t, <-errCh, ErrCancelled)
	})
	t.Run("counts_shown_and_total", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		go func() { _, _ = u.Pick("Model", items, PickOptions{}) }()

		waitFor(t, u, v, "3 of 3")
		press(t, pw, "qwen")
		waitFor(t, u, v, "1 of 3")
	})
	t.Run("fits_a_five_row_terminal", func(t *testing.T) {
		// the tightest layout the cap has to survive
		v := newVT(60, 5)
		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pw.Close() })
		u := newTestUI(t, v, pr)

		many := make([]PickItem, 40)
		for i := range many {
			many[i] = PickItem{Label: "model-" + strconv.Itoa(i)}
		}
		result := make(chan int, 1)
		go func() {
			i, _ := u.Pick("Model", many, PickOptions{})
			result <- i
		}()

		waitFor(t, u, v, "Model")
		require.Eventually(t, func() bool {
			return liveRowCount(u.snapshot(v)) <= 5
		}, time.Second, testPoll, "live block overflowed a 5 row screen")

		press(t, pw, "\r")
		assert.Equal(t, 0, <-result)
	})
	t.Run("long_list_scrolls_within_the_cap", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		many := make([]PickItem, 300)
		for i := range many {
			many[i] = PickItem{Label: "model-" + strconv.Itoa(i)}
		}
		go func() { _, _ = u.Pick("Model", many, PickOptions{}) }()

		waitFor(t, u, v, "300 of 300")
		waitFor(t, u, v, "more")

		// the live block must stay inside its share of a 12 row screen
		require.Eventually(t, func() bool {
			return liveRowCount(u.snapshot(v)) <= 12
		}, time.Second, testPoll)

		press(t, pw, "\x1b")
	})
}

// liveRowCount counts non blank screen rows.
func liveRowCount(screen string) int {
	var n int
	for _, line := range strings.Split(screen, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func TestUISelectAltMode(t *testing.T) {
	t.Parallel()

	// the interaction layer only composes rows for the live block, so it must
	// behave the same whichever renderer owns the screen
	v := newVT(80, 12)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	u := &UI{
		theme:  NewTheme(ColorNone),
		render: newTestAlt(v),
		mode:   ModeAlt,
		status: Status{Model: "test", MaxTokens: 1000},
		in:     pr,
		inFd:   -1,
		msgs:   make(chan string),
		done:   make(chan struct{}),
	}
	u.reader = newInputReader(pr)
	go u.reader.run()
	go u.readKeys()
	u.repaint()
	t.Cleanup(u.Close)

	result := make(chan int, 1)
	go func() {
		i, err := u.Select("Permission:", []Option{{Label: "Allow"}, {Label: "Deny"}})
		assert.NoError(t, err)
		result <- i
	}()

	waitFor(t, u, v, "Permission:")
	press(t, pw, "\x1b[B")
	waitFor(t, u, v, "> Deny")
	press(t, pw, "\r")

	assert.Equal(t, 1, <-result)
	waitFor(t, u, v, "! Permission: Deny")
}

func TestInteractionQueue(t *testing.T) {
	t.Parallel()

	t.Run("second_request_waits_its_turn", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		first := make(chan int, 1)
		second := make(chan int, 1)
		go func() {
			i, _ := u.Select("First:", []Option{{Label: "A"}})
			first <- i
		}()
		waitFor(t, u, v, "First:")

		go func() {
			i, _ := u.Select("Second:", []Option{{Label: "B"}})
			second <- i
		}()

		// the queued prompt is not shown while the first one holds the block
		assert.NotContains(t, u.snapshot(v), "Second:")

		press(t, pw, "\r")
		<-first
		waitFor(t, u, v, "Second:")

		press(t, pw, "\r")
		<-second
	})
	t.Run("close_unblocks_everything_pending", func(t *testing.T) {
		u, v, _ := interactionUI(t)

		errs := make(chan error, 2)
		go func() {
			_, err := u.Select("First:", []Option{{Label: "A"}})
			errs <- err
		}()
		waitFor(t, u, v, "First:")
		go func() {
			_, err := u.Select("Second:", []Option{{Label: "B"}})
			errs <- err
		}()

		u.Close()
		require.ErrorIs(t, <-errs, ErrCancelled)
		require.ErrorIs(t, <-errs, ErrCancelled)
	})
	t.Run("context_cancel_clears_the_live_block", func(t *testing.T) {
		u, v, _ := interactionUI(t)

		ctx, cancel := context.WithCancel(t.Context())
		errCh := make(chan error, 1)
		go func() {
			_, err := u.SelectContext(ctx, "Waiting:", []Option{{Label: "A"}})
			errCh <- err
		}()

		waitFor(t, u, v, "Waiting:")
		cancel()

		require.ErrorIs(t, <-errCh, ErrCancelled)
		require.Eventually(t, func() bool {
			return !strings.Contains(u.snapshot(v), "Waiting:") ||
				strings.Contains(u.snapshot(v), userMarker)
		}, time.Second, testPoll)
	})
	t.Run("interaction_on_a_closed_ui", func(t *testing.T) {
		u, _, _ := interactionUI(t)
		u.Close()

		_, err := u.Select("Pick:", []Option{{Label: "A"}})
		assert.ErrorIs(t, err, ErrCancelled)
	})
}

func TestPendingResolve(t *testing.T) {
	t.Parallel()

	t.Run("resolves_only_once", func(t *testing.T) {
		p := newPending(&selectState{})
		first := errors.New("first")
		p.resolve(first)
		p.resolve(errors.New("second"))

		<-p.done
		assert.ErrorIs(t, p.err, first)
	})
}
