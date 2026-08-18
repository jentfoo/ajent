package tui

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tallUI drives a UI on a screen tall enough to show the full elided context
// block plus its cut marker, so those rows are deterministic in tests.
func tallUI(t *testing.T) (*UI, *vt, *io.PipeWriter) {
	t.Helper()
	v := newVT(80, 30)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	return newTestUI(t, v, pr), v, pw
}

func TestUIDecisionRenders(t *testing.T) {
	t.Parallel()

	req := DecisionRequest{
		Prompt:  "Run command?",
		Context: "rm -rf /tmp/x\nsecond line of subject output",
		Options: []Option{{Label: "Allow"}, {Label: "Deny"}},
	}

	t.Run("shows_subject_and_numbered_options", func(t *testing.T) {
		// the divider row costs one live-block line; a taller screen keeps both
		// numbered options on it.
		u, v, _ := tallUI(t)
		d := u.OpenDecision(req)
		t.Cleanup(d.Close)

		ctx := t.Context()
		go func() { _, _ = d.Wait(ctx) }()
		waitFor(t, u, v, "Run command?")
		waitFor(t, u, v, "rm -rf /tmp/x")
		assert.Contains(t, u.snapshot(v), "> 1 Allow", "cursor marks the first numbered option")
		assert.Contains(t, u.snapshot(v), "  2 Deny")
	})
	t.Run("elides_long_lines_to_the_width", func(t *testing.T) {
		u, v, _ := interactionUI(t)
		long := strings.Repeat("x", 200)
		d := u.OpenDecision(DecisionRequest{Prompt: "P", Context: long, Options: []Option{{Label: "A"}}})
		t.Cleanup(d.Close)

		ctx := t.Context()
		go func() { _, _ = d.Wait(ctx) }()
		waitFor(t, u, v, strings.Repeat("x", 79))
		assert.NotContains(t, stripANSI(u.snapshot(v)), long)
	})
	t.Run("marks_when_the_subject_is_cut_by_height", func(t *testing.T) {
		u, v, _ := tallUI(t)
		var lines []string
		for i := 0; i < decisionContextRows+3; i++ {
			lines = append(lines, "line-"+strings.Repeat("a", i))
		}
		d := u.OpenDecision(DecisionRequest{Prompt: "P", Context: strings.Join(lines, "\n"), Options: []Option{{Label: "A"}}})
		t.Cleanup(d.Close)

		ctx := t.Context()
		go func() { _, _ = d.Wait(ctx) }()
		waitFor(t, u, v, "…+3 lines")
		assert.NotContains(t, stripANSI(u.snapshot(v)), "line-"+strings.Repeat("a", decisionContextRows+2))
	})
	t.Run("cuts_the_subject_to_its_char_budget", func(t *testing.T) {
		u, v, _ := tallUI(t)
		var lines []string
		for i := 0; i < 20; i++ {
			lines = append(lines, strings.Repeat("y", decisionContextChars/10))
		}
		d := u.OpenDecision(DecisionRequest{Prompt: "P", Context: strings.Join(lines, "\n"), Options: []Option{{Label: "A"}}})
		t.Cleanup(d.Close)

		ctx := t.Context()
		go func() { _, _ = d.Wait(ctx) }()
		// the total exceeds decisionContextChars, so some lines are dropped
		assert.Eventually(t, func() bool {
			s := stripANSI(u.snapshot(v))
			return strings.Contains(s, "+") && !strings.Contains(s, "yyyy yyyy")
		}, time.Second, testPoll)
	})
}

func TestUIDecisionKeys(t *testing.T) {
	t.Parallel()

	mk := func(prompt string, opts []Option) DecisionRequest {
		return DecisionRequest{Prompt: prompt, Context: "subject", Options: opts}
	}

	t.Run("digit_selects_directly_and_not_external", func(t *testing.T) {
		u, v, pw := interactionUI(t)
		d := u.OpenDecision(mk("Pick:", []Option{{Label: "A"}, {Label: "B"}, {Label: "C"}}))
		t.Cleanup(d.Close)

		ctx := t.Context()
		resCh := make(chan DecisionResult, 1)
		errCh := make(chan error, 1)
		go func() {
			r, err := d.Wait(ctx)
			resCh <- r
			errCh <- err
		}()
		waitFor(t, u, v, "Pick:")
		press(t, pw, "3")

		require.NoError(t, <-errCh)
		assert.Equal(t, DecisionResult{Index: 2}, <-resCh, "a keystroke is not an external resolve")
	})
	t.Run("arrows_then_enter", func(t *testing.T) {
		u, v, pw := interactionUI(t)
		d := u.OpenDecision(mk("Pick:", []Option{{Label: "A"}, {Label: "B"}}))
		t.Cleanup(d.Close)

		ctx := t.Context()
		resCh := make(chan DecisionResult, 1)
		go func() {
			r, _ := d.Wait(ctx)
			resCh <- r
		}()
		waitFor(t, u, v, "Pick:")
		press(t, pw, "\x1b[B") // down to B
		waitFor(t, u, v, "> 2 B")
		press(t, pw, "\r")

		assert.Equal(t, DecisionResult{Index: 1}, <-resCh)
	})
	t.Run("escape_cancels", func(t *testing.T) {
		u, v, pw := interactionUI(t)
		d := u.OpenDecision(mk("Pick:", []Option{{Label: "A"}}))
		t.Cleanup(d.Close)

		ctx := t.Context()
		errCh := make(chan error, 1)
		go func() {
			_, err := d.Wait(ctx)
			errCh <- err
		}()
		waitFor(t, u, v, "Pick:")
		press(t, pw, "\x1b")

		assert.ErrorIs(t, <-errCh, ErrCancelled)
	})
}

func TestUIDecisionExternalResolve(t *testing.T) {
	t.Parallel()

	t.Run("caller_settles_the_dialog", func(t *testing.T) {
		u, v, _ := interactionUI(t)
		d := u.OpenDecision(DecisionRequest{Prompt: "Go?", Context: "cmd", Options: []Option{{Label: "Yes"}, {Label: "No"}}})
		t.Cleanup(d.Close)

		resCh := make(chan DecisionResult, 1)
		errCh := make(chan error, 1)
		ctx := t.Context()
		go func() {
			r, err := d.Wait(ctx)
			resCh <- r
			errCh <- err
		}()
		waitFor(t, u, v, "Go?")
		d.Resolve(0)

		require.NoError(t, <-errCh)
		assert.Equal(t, DecisionResult{Index: 0, External: true}, <-resCh)
	})
	t.Run("a_keystroke_wins_a_later_resolve_is_noop", func(t *testing.T) {
		u, v, pw := interactionUI(t)
		d := u.OpenDecision(DecisionRequest{Prompt: "Go?", Context: "cmd", Options: []Option{{Label: "Yes"}, {Label: "No"}}})
		t.Cleanup(d.Close)

		resCh := make(chan DecisionResult, 1)
		errCh := make(chan error, 1)
		ctx := t.Context()
		go func() {
			r, err := d.Wait(ctx)
			resCh <- r
			errCh <- err
		}()
		waitFor(t, u, v, "Go?")
		press(t, pw, "\r") // keystroke resolves first

		require.NoError(t, <-errCh)
		assert.Equal(t, DecisionResult{Index: 0}, <-resCh)

		d.Resolve(1) // already settled; must not double-commit
		assert.Eventually(t, func() bool {
			return strings.Contains(u.snapshot(v), "! Go? Yes")
		}, time.Second, testPoll)
		count := strings.Count(stripANSI(u.snapshot(v)), "Go? Yes")
		assert.Equal(t, 1, count, "the summary is committed exactly once")
	})
	t.Run("resolving_a_queued_dialog_promotes_the_next", func(t *testing.T) {
		u, v, _ := interactionUI(t)
		d1 := u.OpenDecision(DecisionRequest{Prompt: "First?", Context: "", Options: []Option{{Label: "A"}}})
		t.Cleanup(d1.Close)
		d2 := u.OpenDecision(DecisionRequest{Prompt: "Second?", Context: "", Options: []Option{{Label: "B"}}})
		t.Cleanup(d2.Close)

		ctx := t.Context()
		resCh := make(chan DecisionResult, 1)
		go func() {
			r, _ := d1.Wait(ctx)
			resCh <- r
		}()
		waitFor(t, u, v, "+1 waiting")
		d1.Resolve(0)

		assert.Equal(t, DecisionResult{Index: 0, External: true}, <-resCh)
		waitFor(t, u, v, "Second?")
	})
}

func TestUIDecisionQueueDepth(t *testing.T) {
	t.Parallel()

	t.Run("reports_waiters_behind_the_active_dialog", func(t *testing.T) {
		u, v, _ := interactionUI(t)
		d1 := u.OpenDecision(DecisionRequest{Prompt: "First?", Context: "", Options: []Option{{Label: "A"}}})
		t.Cleanup(d1.Close)
		ctx := t.Context()
		go func() { _, _ = d1.Wait(ctx) }()

		d2 := u.OpenDecision(DecisionRequest{Prompt: "Second?", Context: "", Options: []Option{{Label: "B"}}})
		t.Cleanup(d2.Close)
		go func() { _, _ = d2.Wait(ctx) }()

		waitFor(t, u, v, "+1 waiting")
		assert.NotContains(t, u.snapshot(v), "Second?", "the queued prompt is not drawn")

		d3 := u.OpenDecision(DecisionRequest{Prompt: "Third?", Context: "", Options: []Option{{Label: "C"}}})
		t.Cleanup(d3.Close)
		go func() { _, _ = d3.Wait(ctx) }()

		waitFor(t, u, v, "+2 waiting")
		assert.NotContains(t, u.snapshot(v), "Third?")
	})
	t.Run("escape_resolves_only_the_active_and_promotes", func(t *testing.T) {
		u, v, pw := interactionUI(t)
		d1 := u.OpenDecision(DecisionRequest{Prompt: "First?", Context: "", Options: []Option{{Label: "A"}}})
		t.Cleanup(d1.Close)
		d2 := u.OpenDecision(DecisionRequest{Prompt: "Second?", Context: "", Options: []Option{{Label: "B"}}})
		t.Cleanup(d2.Close)

		ctx := t.Context()
		errCh := make(chan error, 1)
		go func() {
			_, err := d1.Wait(ctx)
			errCh <- err
		}()
		waitFor(t, u, v, "+1 waiting")

		press(t, pw, "\x1b") // cancels the active (first) dialog only
		require.ErrorIs(t, <-errCh, ErrCancelled)

		res2 := make(chan DecisionResult, 1)
		go func() {
			r, _ := d2.Wait(ctx)
			res2 <- r
		}()
		waitFor(t, u, v, "Second?")
		press(t, pw, "\r")

		assert.Equal(t, DecisionResult{Index: 0}, <-res2)
	})
}

func TestUIDecisionHeightCap(t *testing.T) {
	t.Parallel()

	v := newVT(60, 5)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	u := newTestUI(t, v, pr)

	many := DecisionRequest{Prompt: "Approve?", Context: strings.Repeat("context line\n", decisionContextRows+5), Options: []Option{{Label: "Allow"}}}
	d := u.OpenDecision(many)
	t.Cleanup(d.Close)

	ctx := t.Context()
	go func() { _, _ = d.Wait(ctx) }()
	require.Eventually(t, func() bool { return strings.Contains(u.snapshot(v), "Approve?") }, time.Second, testPoll)
	assert.LessOrEqual(t, liveRowCount(u.snapshot(v)), 5, "the dialog fits a five row terminal")
}

func TestUIDecisionSummary(t *testing.T) {
	t.Parallel()

	u, v, pw := interactionUI(t)
	d := u.OpenDecision(DecisionRequest{Prompt: "Approve edit?", Context: "pkg/x.go", Options: []Option{{Label: "Allow"}, {Label: "Deny"}}})
	t.Cleanup(d.Close)

	ctx := t.Context()
	resCh := make(chan DecisionResult, 1)
	go func() {
		r, _ := d.Wait(ctx)
		resCh <- r
	}()
	waitFor(t, u, v, "Approve edit?")
	press(t, pw, "\x1b[B") // down to Deny
	waitFor(t, u, v, "> 2 Deny")
	press(t, pw, "\r")

	assert.Equal(t, DecisionResult{Index: 1}, <-resCh)

	// a one-line history summary names the decision and its subject choice
	waitFor(t, u, v, "! Approve edit? Deny")
}

func TestUIDecisionNoUI(t *testing.T) {
	t.Parallel()

	t.Run("plain_mode_reports_no_ui", func(t *testing.T) {
		v := newVT(40, 10)
		d := &UI{theme: NewTheme(ColorNone), render: newTestInline(v), mode: ModePlain,
			in: strings.NewReader(""), msgs: make(chan string), done: make(chan struct{}), controls: make(chan Control)}
		t.Cleanup(d.Close)

		dd := d.OpenDecision(DecisionRequest{Prompt: "P", Options: []Option{{Label: "A"}}})
		t.Cleanup(dd.Close)
		_, err := dd.Wait(t.Context())
		assert.ErrorIs(t, err, ErrNoUI)
	})
	t.Run("no_options_reports_no_ui", func(t *testing.T) {
		u, _, _ := interactionUI(t)
		dd := u.OpenDecision(DecisionRequest{Prompt: "P"})
		t.Cleanup(dd.Close)
		_, err := dd.Wait(t.Context())
		assert.ErrorIs(t, err, ErrNoUI)
	})
	t.Run("closed_ui_cancels", func(t *testing.T) {
		u, v, _ := interactionUI(t)
		dd := u.OpenDecision(DecisionRequest{Prompt: "P", Options: []Option{{Label: "A"}}})
		t.Cleanup(dd.Close)
		waitFor(t, u, v, "P") // ensure the dialog was enqueued before close
		u.Close()
		_, err := dd.Wait(t.Context())
		assert.ErrorIs(t, err, ErrCancelled)
	})
	t.Run("open_after_close_reports_no_ui", func(t *testing.T) {
		u, _, _ := interactionUI(t)
		u.Close()
		dd := u.OpenDecision(DecisionRequest{Prompt: "P", Options: []Option{{Label: "A"}}})
		t.Cleanup(dd.Close)
		_, err := dd.Wait(t.Context())
		assert.ErrorIs(t, err, ErrNoUI) // a closed UI has nobody to ask
	})
}
