package tui

import (
	"context"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testPoll = time.Millisecond

// newTestUI drives a UI in inline mode against the emulator, with no real
// terminal behind it.
func newTestUI(tb testing.TB, v *vt, in io.Reader) *UI {
	tb.Helper()
	return newTestUIWith(tb, v, in, NewTheme(ColorNone))
}

// newTestUIWith is newTestUI with an explicit theme, so shade and colour
// paths are reachable from tests.
func newTestUIWith(tb testing.TB, v *vt, in io.Reader, theme Theme) *UI {
	tb.Helper()
	u := &UI{
		theme:    theme,
		render:   newTestInline(v),
		mode:     ModeInline,
		status:   Status{Model: "test", MaxTokens: 1000},
		in:       in,
		inFd:     -1,
		msgs:     make(chan string),
		controls: make(chan Control, 4),
		done:     make(chan struct{}),
		// afterDelay is the probe-timeout seam; tests override it per case
		afterDelay: time.AfterFunc,
	}
	u.reader = newInputReader(in)
	go u.reader.run()
	go u.readKeys()
	go u.watchStatus()
	u.mu.Lock() // a key may already be decoding; serialize with readKeys
	u.repaint()
	u.mu.Unlock()
	tb.Cleanup(u.Close)
	return u
}

// snapshot reads the emulator screen while holding the UI lock.
func (u *UI) snapshot(v *vt) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return v.Screen()
}

// line reads one emulator row while holding the UI lock.
func (u *UI) line(v *vt, row int) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return v.Line(row)
}

// cursor reads the emulator's caret while holding the UI lock, for park
// assertions: after any settled draw it sits on the live block's first row.
func (u *UI) cursor(v *vt) (int, int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return v.row, v.col
}

func TestUIInput(t *testing.T) {
	t.Parallel()

	v := newVT(40, 10)
	pr, pw := io.Pipe()
	u := newTestUI(t, v, pr)

	waitLine := func(row int, want string) {
		require.Eventually(t, func() bool {
			return u.line(v, row) == want
		}, time.Second, testPoll, "waiting for row %d to be %q", row, want)
	}

	t.Run("block_starts_at_the_top", func(t *testing.T) {
		// the divider rule leads, then input and status beneath it
		assert.Equal(t, strings.Repeat(ruleChar, 39), v.Line(0)) // one column reserved, see repaint
		assert.Equal(t, promptFirst+inputHint, v.Line(1))
		// the leftmost working glyph always leads the status line
		assert.Equal(t, spinnerFrames[0]+" · ░░░░░░░░░░ 0/1k · test", v.Line(2))
	})
	t.Run("typed_text_replaces_hint", func(t *testing.T) {
		_, err := io.WriteString(pw, "hi")
		require.NoError(t, err)
		waitLine(1, promptFirst+"hi")
	})
	t.Run("enter_submits", func(t *testing.T) {
		_, err := io.WriteString(pw, "\r")
		require.NoError(t, err)
		assert.Equal(t, "hi", <-u.Messages())
		waitLine(1, promptFirst+inputHint)
	})
	t.Run("status_follows_the_input", func(t *testing.T) {
		u.SetStatus(Status{Model: "opus-5"})
		// the working glyph always leads, then the model
		assert.Equal(t, spinnerFrames[0]+" · opus-5", v.Line(2))
	})
	require.NoError(t, pw.Close())
}

func TestUIInputEditing(t *testing.T) {
	t.Parallel()

	v := newVT(40, 10)
	pr, pw := io.Pipe()
	u := newTestUI(t, v, pr)

	waitLine := func(row int, want string) {
		require.Eventually(t, func() bool {
			return u.line(v, row) == want
		}, time.Second, testPoll, "waiting for row %d to be %q", row, want)
	}

	t.Run("backspace_and_word_kill", func(t *testing.T) {
		_, err := io.WriteString(pw, "one two\x7f")
		require.NoError(t, err)
		waitLine(1, promptFirst+"one tw")

		_, err = io.WriteString(pw, "\x17")
		require.NoError(t, err)
		waitLine(1, promptFirst+"one")
	})
	t.Run("alt_enter_grows_the_block", func(t *testing.T) {
		_, err := io.WriteString(pw, "\x1b\rsecond")
		require.NoError(t, err)
		waitLine(2, promptCont+"second")
		waitLine(3, spinnerFrames[0]+" · ░░░░░░░░░░ 0/1k · test")
	})
	t.Run("ctrl_c_clears_buffer", func(t *testing.T) {
		_, err := io.WriteString(pw, "\x03")
		require.NoError(t, err)
		waitLine(1, promptFirst+inputHint)
		assert.Empty(t, u.line(v, 3), "the extra input row is gone")
	})
	t.Run("ctrl_c_on_empty_emits_interrupt", func(t *testing.T) {
		_, err := io.WriteString(pw, "\x03")
		require.NoError(t, err)
		assert.Equal(t, ControlInterrupt, <-u.Controls())
	})
	t.Run("ctrl_d_on_empty_emits_eof", func(t *testing.T) {
		_, err := io.WriteString(pw, "\x04")
		require.NoError(t, err)
		assert.Equal(t, ControlEOF, <-u.Controls())
	})
	require.NoError(t, pw.Close())
}

// TestUIModeCycle checks Shift+Tab reaches the control channel both idle and
// while a dialog owns the keyboard.
func TestUIModeCycle(t *testing.T) {
	t.Parallel()

	u, v, pw := interactionUI(t)

	t.Run("idle_emits_mode_cycle", func(t *testing.T) {
		press(t, pw, "\x1b[Z")
		assert.Equal(t, ControlModeCycle, <-u.Controls())
	})

	t.Run("while_dialog_open_reaches_controls", func(t *testing.T) {
		d := u.OpenDecision(DecisionRequest{
			Prompt:  "Approve:",
			Context: "run rm -rf /tmp/x",
			Options: []Option{{Label: "Allow"}, {Label: "Deny"}},
		})
		t.Cleanup(d.Close)

		waitFor(t, u, v, "Approve:")
		press(t, pw, "\x1b[Z")
		assert.Equal(t, ControlModeCycle, <-u.Controls())
	})
}

func TestUIHistory(t *testing.T) {
	t.Parallel()

	v := newVT(60, 20)
	u := newTestUI(t, v, strings.NewReader(""))

	t.Run("user_echo", func(t *testing.T) {
		u.UserEcho("fix the retry logic")
		assert.Equal(t, "❯ fix the retry logic", v.Line(0))
	})
	t.Run("thinking_streams_lines", func(t *testing.T) {
		u.Thinking("the retry helper loops\nwith no backoff")
		assert.Equal(t, "✻ thinking", v.Line(2))
		assert.Equal(t, "the retry helper loops", v.Line(3))

		u.EndThinking()
		assert.Equal(t, "with no backoff", v.Line(4))
	})
	t.Run("markdown_commits_complete_blocks", func(t *testing.T) {
		u.Text("## Retry\n\nfirst para")
		assert.Equal(t, "## Retry", v.Line(6))

		u.Text(" continues\n\n")
		assert.Equal(t, "first para continues", v.Line(8))

		u.Text("- one\n- two\n")
		u.EndText()
		assert.Equal(t, "• one", v.Line(10))
		assert.Equal(t, "• two", v.Line(11))
	})
	t.Run("partial_text_streams_live_then_commits", func(t *testing.T) {
		v2 := newVT(60, 20)
		u2 := newTestUI(t, v2, strings.NewReader(""))

		// a single paragraph not yet closed stays as a live preview above input,
		// so the reply reads word by word instead of only at block boundaries.
		u2.Text("first para ")
		assert.Equal(t, "first para", v2.Line(0))

		u2.EndText()
		assert.Equal(t, "first para", v2.Line(0), "the preview commits to history")
		assert.Contains(t, v2.Line(2), promptFirst, "preview row is released after commit")
	})
	t.Run("block_follows_the_last_line", func(t *testing.T) {
		// the committed transcript ends at row 11; divider on 12, input then status
		assert.Contains(t, v.Line(13), promptFirst)
		assert.Contains(t, v.Line(14), "0/1k · test")
	})
}

func TestUIDiff(t *testing.T) {
	t.Parallel()

	v := newVT(60, 12)
	u := newTestUI(t, v, strings.NewReader(""))

	u.Diff("pkg/client/retry.go", "a\nb\n", "a\nB\n")

	assert.Equal(t, "pkg/client/retry.go +1 -1", v.Line(0))
	assert.Equal(t, "@@ -1,2 +1,2 @@", v.Line(1))
	assert.Equal(t, " 1   a", v.Line(2))
	assert.Equal(t, " 2 - b", v.Line(3))
	assert.Equal(t, " 2 + B", v.Line(4))

	t.Run("unchanged_writes_nothing", func(t *testing.T) {
		before := u.snapshot(v)
		u.Diff("x.go", "same\n", "same\n")
		assert.Equal(t, before, u.snapshot(v))
	})
}

func TestUIToolStart(t *testing.T) {
	t.Parallel()

	v := newVT(40, 10)
	u := newTestUI(t, v, strings.NewReader(""))

	done := u.ToolStart("bash: go test ./...")
	assert.Equal(t, "⏺ bash: go test ./...", v.Line(0), "the header commits up front")
	// no separate spinner row above the input; the tool rides in the status bar.
	statusRow := u.line(v, 3) // committed header on row 0, live block starts at row 1: divider, then input and status
	assert.True(t, strings.HasPrefix(strutil.StripANSI(statusRow), spinnerFrames[0]),
		"spinner still leads the bottom-left status line while a tool runs")
	assert.Contains(t, strutil.StripANSI(statusRow), "bash: go test ./...",
		"running tool label sits next to the working glyph in the status bar")

	done("ok  0.4s")

	assert.Equal(t, "  ok  0.4s", v.Line(1))
	assert.Contains(t, v.Line(3), promptFirst, "no spinner row is left behind")
}

func TestUIBusy(t *testing.T) {
	t.Parallel()

	v := newVT(40, 10)
	u := newTestUI(t, v, strings.NewReader(""))

	// a bare glyph sits at the left of the status line even when idle; no label.
	assert.NotContains(t, u.snapshot(v), "working", "no working label is ever shown")
	assert.Contains(t, u.snapshot(v), spinnerFrames[0], "a resting frame is always visible")

	stop := u.Busy()
	// advancing the frame while busy repaints a different glyph: it animates.
	u.mu.Lock()
	u.spinner++
	u.repaint()
	u.mu.Unlock()
	assert.Contains(t, u.snapshot(v), spinnerFrames[1%len(spinnerFrames)], "busy advances the frame")

	// a running tool shares the bottom-left status line with the busy glyph.
	doneTool := u.ToolStart("bash: go test ./...")
	assert.Equal(t, "⏺ bash: go test ./...", v.Line(0))
	statusRow := u.line(v, 3) // committed header on row 0; live block starts at row 1 (divider), input then status
	assert.Contains(t, strutil.StripANSI(statusRow), "bash: go test ./...")
	doneTool("ok  0.4s")

	stop()
}

func TestUIStatusSpinnerLeftmost(t *testing.T) {
	t.Parallel()

	v := newVT(60, 10)
	u := newTestUI(t, v, strings.NewReader(""))

	// the spinner is the first element of the status line, before model and tokens.
	statusRow := u.line(v, 2) // live block: divider on row 0, input on row 1, status on row 2
	assert.True(t, strings.HasPrefix(strutil.StripANSI(statusRow), spinnerFrames[0]),
		"spinner occupies the leftmost column of the status bar")
}

func TestUIOutput(t *testing.T) {
	t.Parallel()

	v := newVT(40, 12)
	u := newTestUI(t, v, strings.NewReader(""))

	t.Run("streams_under_the_tool_header", func(t *testing.T) {
		done := u.ToolStart("bash: go test -v")
		u.Output("=== RUN   TestRetry\n--- PASS: TestRetry (0.01s)\n")
		u.EndOutput()
		done("")

		assert.Equal(t, "⏺ bash: go test -v", v.Line(0))
		assert.Equal(t, "=== RUN   TestRetry", v.Line(1), "no blank line splits the output")
		assert.Equal(t, "--- PASS: TestRetry (0.01s)", v.Line(2))
	})
	t.Run("holds_partial_lines", func(t *testing.T) {
		u.Output("ok  gith")
		assert.NotContains(t, u.snapshot(v), "ok  gith")
		u.Output("ub.com/x\n")
		assert.Equal(t, "ok  github.com/x", v.Line(3))
	})
	t.Run("flushes_unterminated_line", func(t *testing.T) {
		u.Output("no newline")
		u.EndOutput()
		assert.Equal(t, "no newline", v.Line(4))
	})
	t.Run("not_parsed_as_markdown", func(t *testing.T) {
		u.Output("--- PASS: TestX (0.00s)\n")
		u.EndOutput()
		assert.Equal(t, "--- PASS: TestX (0.00s)", v.Line(5), "would be a thematic break as markdown")
	})
}

func TestUICommit(t *testing.T) {
	t.Parallel()

	v := newVT(40, 10)
	u := newTestUI(t, v, strings.NewReader(""))

	t.Run("sections_separated_by_one_blank", func(t *testing.T) {
		u.UserEcho("one")
		u.UserEcho("two")
		assert.Equal(t, "❯ one", v.Line(0))
		assert.Empty(t, v.Line(1))
		assert.Equal(t, "❯ two", v.Line(2))
	})
	t.Run("no_leading_blank_line", func(t *testing.T) {
		v2 := newVT(40, 10)
		u2 := newTestUI(t, v2, strings.NewReader(""))
		u2.UserEcho("first")
		assert.Equal(t, "❯ first", v2.Line(0))
	})
}

func TestUIScrollKeys(t *testing.T) {
	t.Parallel()

	v := newVT(40, 10)
	u := newTestUI(t, v, strings.NewReader(""))

	// inline mode leaves scrolling to the terminal, so the keys are inert
	u.mu.Lock()
	defer u.mu.Unlock()
	assert.False(t, u.render.scroll(u.page()))
	assert.Positive(t, u.page())
}

// TestNew covers the wiring from Options through to a live renderer. Pipes are
// not terminals, so the mode resolves to plain without needing a pty.
func TestNew(t *testing.T) {
	t.Parallel()

	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
	})

	u, err := New(Options{In: inR, Out: outW, Model: "test", MaxTokens: 1000})
	require.NoError(t, err)
	t.Cleanup(u.Close)

	assert.Equal(t, ModePlain, u.Mode())
	assert.Equal(t, defaultWidth, u.Width())

	t.Run("input_reaches_messages", func(t *testing.T) {
		_, err := inW.WriteString("  \nhello\n") // blank lines are skipped
		require.NoError(t, err)

		select {
		case msg := <-u.Messages():
			assert.Equal(t, "hello", msg)
		case <-time.After(time.Second):
			t.Fatal("no message")
		}
	})
	t.Run("output_reaches_the_writer", func(t *testing.T) {
		u.UserEcho("echoed")

		buf := make([]byte, 64)
		n, err := outR.Read(buf)
		require.NoError(t, err)
		assert.Contains(t, string(buf[:n]), "echoed")
	})
	t.Run("close_is_idempotent", func(t *testing.T) {
		u.Close()
		u.Close()
	})
}

func TestUIResume(t *testing.T) {
	t.Parallel()

	v := newVT(40, 10)
	u := newTestUI(t, v, strings.NewReader(""))
	u.UserEcho("kept")
	require.Equal(t, "❯ kept", u.line(v, 0))

	u.mu.Lock()
	u.render.suspend(u.inFd)
	u.mu.Unlock()
	require.Empty(t, u.line(v, 1), "the live block is handed back")

	u.resume()
	assert.Equal(t, "❯ kept", u.line(v, 0), "history is untouched")
	assert.Contains(t, u.snapshot(v), "/1k · test", "the live status bar is repainted")
}

// TestUISafeGo checks the recover path in a subprocess, since it re-panics by
// design. The risk it guards is a deadlock on the UI lock instead of an exit.
func TestUISafeGo(t *testing.T) {
	if os.Getenv("TUI_PANIC_CHILD") == "1" {
		u := newTestUI(t, newVT(40, 10), strings.NewReader(""))
		u.safeGo(func() {
			u.mu.Lock()
			defer u.mu.Unlock()
			panic("boom")
		})
		select {} // the panic must take the process down
	}
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestUISafeGo", "-test.timeout=20s")
	cmd.Env = append(os.Environ(), "TUI_PANIC_CHILD=1")
	out, err := cmd.CombinedOutput()

	require.Error(t, err, "the child must die from the panic")
	assert.Contains(t, string(out), "boom")
	assert.NotContains(t, string(out), "test timed out", "Close must not deadlock")
}

func TestUIReadLines(t *testing.T) {
	t.Parallel()

	// well past bufio.Scanner's 64 KB default, which used to drop the line
	huge := strings.Repeat("x", 200_000)

	pr, pw := io.Pipe()
	u := &UI{mode: ModePlain, in: pr, msgs: make(chan string), done: make(chan struct{})}
	go u.readLines()
	t.Cleanup(func() { _ = pr.Close() })

	go func() {
		_, _ = pw.Write([]byte(huge + "\n"))
		_ = pw.Close()
	}()

	select {
	case msg := <-u.Messages():
		assert.Equal(t, huge, msg)
	case <-time.After(time.Second):
		t.Fatal("no message")
	}
}

func TestUISearchOverlay(t *testing.T) {
	t.Parallel()

	v := newVT(60, 10)
	pr, _ := io.Pipe() // stays open so the key loop never emits a spurious EOF
	u := newTestUI(t, v, pr)
	u.SetHistorySearch(func() []SearchItem {
		return []SearchItem{
			{Text: "fix the retry loop", Detail: "2026-01-02 03:04 UTC"},
			{Text: "add a test\nfor retries"},
		}
	})

	deliver := func() {
		// openSearchLocked runs the provider off the key loop; wait for it here.
		require.Eventually(t, func() bool {
			u.mu.Lock()
			defer u.mu.Unlock()
			return u.search != nil && !u.search.pending
		}, time.Second, testPoll)
	}
	press := func(k key) *string { // applyKey then repaint, mirroring handleKey
		u.mu.Lock()
		defer u.mu.Unlock()
		submit, dirty, _ := u.applyKey(k)
		if dirty {
			u.repaint()
		}
		return submit
	}

	t.Run("ctrl_r_opens_overlay", func(t *testing.T) {
		press(key{typ: keyReverseSearch})
		deliver()
		assert.Contains(t, strutil.StripANSI(v.Line(1)), "(reverse-i-search)`':")
	})
	t.Run("typing_narrows", func(t *testing.T) {
		press(key{typ: keyRune, text: "retry"})
		// the header shows the query and detail; the full match renders below it
		assert.Contains(t, strutil.StripANSI(v.Line(1)), "(reverse-i-search)`retry':  2026-01-02 03:04 UTC")
		assert.Contains(t, v.Line(2), "fix the retry loop")
	})
	t.Run("enter_fills_editor_without_submitting", func(t *testing.T) {
		submit := press(key{typ: keyEnter})
		assert.Nil(t, submit)
		require.Nil(t, u.search)
		assert.Equal(t, "fix the retry loop", u.editor.Value(), "full prompt fills the editor")
	})
}

func TestUISearchOverlayEscapeLeavesBuffer(t *testing.T) {
	t.Parallel()

	v := newVT(60, 10)
	pr, _ := io.Pipe() // stays open so the key loop never emits a spurious EOF
	u := newTestUI(t, v, pr)
	u.SetHistorySearch(func() []SearchItem { return []SearchItem{{Text: "typed already"}} })

	press := func(k key) {
		u.mu.Lock()
		defer u.mu.Unlock()
		_, dirty, _ := u.applyKey(k)
		if dirty {
			u.repaint()
		}
	}
	// a pre-existing buffer must survive Esc from the search overlay
	press(key{typ: keyRune, text: "keep me"}) // seed the buffer through the editor
	press(key{typ: keyReverseSearch})
	require.Eventually(t, func() bool {
		u.mu.Lock()
		defer u.mu.Unlock()
		return u.search != nil && !u.search.pending
	}, time.Second, testPoll)

	press(key{typ: keyEscape})

	assert.Nil(t, u.search)
	assert.Equal(t, "keep me", u.editor.Value(), "Esc leaves whatever was typed untouched")
}

// openSearch waits until the Ctrl+R overlay is up and its provider delivered.
func (u *UI) waitOpenSearch(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		u.mu.Lock()
		defer u.mu.Unlock()
		return u.search != nil && !u.search.pending
	}, time.Second, testPoll)
}

func TestUISearchArrowCommitsAndScrolls(t *testing.T) {
	t.Parallel()

	v := newVT(60, 10)
	pr, _ := io.Pipe() // stays open so the key loop never emits a spurious EOF
	u := newTestUI(t, v, pr)
	u.SetHistorySearch(func() []SearchItem {
		return []SearchItem{
			{Text: "newest prompt"},
			{Text: "older prompt"},
		}
	})

	press := func(k key) *string { // applyKey then repaint, mirroring handleKey
		u.mu.Lock()
		defer u.mu.Unlock()
		submit, dirty, _ := u.applyKey(k)
		if dirty {
			u.repaint()
		}
		return submit
	}

	// type a filter so there is something to select; with an empty query nothing shows.
	press(key{typ: keyReverseSearch})
	u.waitOpenSearch(t)
	press(key{typ: keyRune, text: "ne"}) // narrows to the newest prompt
	// Down in reverse search selects the highlighted (newest) prompt into the field
	// and closes the overlay — it does not scroll on this same press.
	submit := press(key{typ: keyDown})
	assert.Nil(t, submit, "an arrow in search must not send the message")
	require.Nil(t, u.search, "down closes the overlay after selecting")
	assert.Equal(t, "newest prompt", u.editor.Value())
}

func TestUISearchArrowCommitsThenBrowsesOlder(t *testing.T) {
	t.Parallel()

	v := newVT(60, 10)
	pr, _ := io.Pipe() // stays open so the key loop never emits a spurious EOF
	u := newTestUI(t, v, pr)
	u.SetHistorySearch(func() []SearchItem {
		return []SearchItem{
			{Text: "newest recorded"},
			{Text: "older recorded"},
		}
	})

	press := func(k key) *string {
		u.mu.Lock()
		defer u.mu.Unlock()
		submit, dirty, _ := u.applyKey(k)
		if dirty {
			u.repaint()
		}
		return submit
	}

	press(key{typ: keyReverseSearch})
	u.waitOpenSearch(t)
	press(key{typ: keyRune, text: "ne"}) // narrow to the newest recorded prompt
	// Up selects the highlighted (newest) prompt and closes search — it must not
	// scroll on this first press.
	submit := press(key{typ: keyUp})
	assert.Nil(t, submit)
	require.Nil(t, u.search, "up closes the overlay after selecting")
	assert.Equal(t, "newest recorded", u.editor.Value(), "the selected prompt fills the field without scrolling")

	// subsequent presses walk older through the same recorded-prompt list.
	submit = press(key{typ: keyUp})
	assert.Nil(t, submit)
	assert.Equal(t, "newest recorded", u.editor.Value(), "second up re-shows the newest before stepping further")
	submit = press(key{typ: keyUp})
	assert.Nil(t, submit)
	assert.Equal(t, "older recorded", u.editor.Value(), "up scrolls older on later presses")
}

func TestUIPlainArrowsScrollRecordedPrompts(t *testing.T) {
	t.Parallel()

	v := newVT(40, 10)
	pr, _ := io.Pipe() // stays open so the key loop never emits a spurious EOF
	u := newTestUI(t, v, pr)
	u.SetHistorySearch(func() []SearchItem {
		return []SearchItem{
			{Text: "third"}, // newest first, as the provider returns them
			{Text: "second"},
			{Text: "first"}, // oldest
		}
	})

	press := func(k key) *string {
		u.mu.Lock()
		defer u.mu.Unlock()
		submit, dirty, _ := u.applyKey(k)
		if dirty {
			u.repaint()
		}
		return submit
	}

	// plain ↑ walks the recorded prompts newest → oldest without ever opening Ctrl+R.
	submit := press(key{typ: keyUp})
	assert.Nil(t, submit)
	assert.Equal(t, "third", u.editor.Value(), "first up recalls the newest prompt")

	press(key{typ: keyUp})
	assert.Equal(t, "second", u.editor.Value())

	press(key{typ: keyUp})
	assert.Equal(t, "first", u.editor.Value(), "third up reaches the oldest prompt")

	// at the oldest there is nothing more to recall; it stays put.
	press(key{typ: keyUp})
	assert.Equal(t, "first", u.editor.Value())

	// ↓ walks back toward the live buffer and finally restores the empty draft.
	press(key{typ: keyDown})
	assert.Equal(t, "second", u.editor.Value())
	press(key{typ: keyDown})
	assert.Equal(t, "third", u.editor.Value())
	press(key{typ: keyDown})
	assert.Empty(t, u.editor.Value(), "down returns to the live buffer")
}

func TestUIUpArrowFillsLastSentMessage(t *testing.T) {
	t.Parallel()

	v := newVT(40, 10)
	pr, pw := io.Pipe()
	u := newTestUI(t, v, pr)
	go func() {
		for range u.Messages() {
		}
	}() // drain submissions so the loop keeps running

	// type and submit a message; it is recorded into editor history live.
	_, err := io.WriteString(pw, "last sent")
	require.NoError(t, err)
	_, err = io.WriteString(pw, "\r") // enter submits
	require.NoError(t, err)
	require.Eventually(t, func() bool { // submit lands via readKeys in the key loop
		u.mu.Lock()
		defer u.mu.Unlock()
		return len(u.editor.history) == 1
	}, time.Second, testPoll)

	_, err = io.WriteString(pw, "\x1b[A") // up arrow: recall the last sent message
	require.NoError(t, err)

	assert.Eventually(t, func() bool {
		u.mu.Lock()
		defer u.mu.Unlock()
		return u.editor.Value() == "last sent"
	}, time.Second, testPoll, "up arrow fills the last sent message into the field")
}

func TestStyleLines(t *testing.T) {
	t.Parallel()

	s := Style{open: sgr(attrDim)}

	t.Run("plain_theme_passthrough", func(t *testing.T) {
		var none Style
		assert.Equal(t, "a\nb\n", styleLines(none, "a\nb\n"))
	})
	t.Run("each_line_wrapped", func(t *testing.T) {
		assert.Equal(t, "\x1b[2ma\x1b[0m\n\x1b[2mb\x1b[0m\n", styleLines(s, "a\nb\n"))
	})
	t.Run("blank_lines_untouched", func(t *testing.T) {
		assert.Equal(t, "\x1b[2ma\x1b[0m\n\n", styleLines(s, "a\n\n"))
	})
	t.Run("empty_input", func(t *testing.T) {
		assert.Empty(t, styleLines(s, ""))
	})
}

func TestUIOnEditFiresAsyncAndExpandsPastes(t *testing.T) {
	t.Parallel()

	v := newVT(40, 10)
	pr, pw := io.Pipe()
	u := newTestUI(t, v, pr)

	var mu sync.Mutex
	var got []string
	u.SetOnEdit(func(text string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, text)
	})

	// a large paste is stored under a placeholder; the editor holds the tiny marker.
	big := strings.Repeat("a", 3000)
	u.mu.Lock()
	ph := pastePlaceholder(big)
	u.pastes = map[string]string{ph: big}
	u.editor.Insert(ph) // what the buffer actually contains
	u.notifyEditLocked(u.expandPastes(u.editor.Value()))
	u.repaint()
	u.mu.Unlock()

	// OnEdit receives the expanded paste, not the placeholder.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(got, big)
	}, time.Second, testPoll)

	_ = pw.Close()
}

// TestUIResizeGate covers the burst gate: while a resize is in flight no draw
// may happen, because an erase landing mid-reflow under-shoots the top of the
// live block and strands rows (a duplicated divider) no later erase can reach.
func TestUIResizeGate(t *testing.T) {
	t.Parallel()

	setResizing := func(u *UI, on bool) {
		u.mu.Lock()
		u.resizing = on
		u.mu.Unlock()
	}

	t.Run("repaints_hold_until_the_burst_settles", func(t *testing.T) {
		v := newVT(40, 10)
		u := newTestUI(t, v, strings.NewReader(""))
		setResizing(u, true)

		u.SetTokens(777) // a repaint trigger; held back while resizing
		assert.NotContains(t, u.snapshot(v), "777")

		u.resize()
		assert.Contains(t, u.snapshot(v), "777")
		assert.False(t, u.resizing)
	})

	t.Run("commits_flush_in_order_at_settle", func(t *testing.T) {
		v := newVT(40, 10)
		u := newTestUI(t, v, strings.NewReader(""))
		setResizing(u, true)

		u.UserEcho("first held line")
		u.Print("second held line")
		screen := u.snapshot(v)
		assert.NotContains(t, screen, "first held line")
		assert.NotContains(t, screen, "second held line")

		u.resize()
		screen = u.snapshot(v)
		one := strings.Index(screen, "first held line")
		two := strings.Index(screen, "second held line")
		require.NotEqual(t, -1, one)
		require.NotEqual(t, -1, two)
		assert.Less(t, one, two, "held commits keep their order")
	})

	t.Run("stream_commit_during_burst_leaves_one_divider", func(t *testing.T) {
		v := newVT(40, 10)
		u := newTestUI(t, v, strings.NewReader(""))
		setResizing(u, true)

		u.Text("streamed during the burst\n\nacross two blocks")
		u.EndText()
		u.resize()

		screen := u.snapshot(v)
		assert.Contains(t, screen, "streamed during the burst")
		assert.Contains(t, screen, "across two blocks")
		var dividers int
		for line := range strings.Lines(screen) {
			if strings.TrimRight(line, " \n") == strings.Repeat(ruleChar, 39) {
				dividers++
			}
		}
		assert.Equal(t, 1, dividers, "exactly one live divider after the settled redraw")
	})

	t.Run("width_change_repaints_our_rows", func(t *testing.T) {
		v := newVT(40, 10)
		u := newTestUI(t, v, strings.NewReader(""))
		u.render.(*inlineRenderer).t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }
		u.Print("committed before the resize")

		v.setSize(20, 10) // the emulator reflows, then the settled signal arrives
		u.resize()

		screen := u.snapshot(v)
		assert.Contains(t, screen, "committed before", "history survives, re-wrapped at the new width")
		assert.Equal(t, 1, strings.Count(screen, "resize"), "re-laid once, never doubled")
		var dividers int
		for line := range strings.Lines(screen) {
			if strings.TrimRight(line, " \n") == strings.Repeat(ruleChar, 19) {
				dividers++
			}
		}
		assert.Equal(t, 1, dividers, "the repaint rewrites the old divider row")
	})

	t.Run("reset_drops_held_commits", func(t *testing.T) {
		v := newVT(40, 10)
		u := newTestUI(t, v, strings.NewReader(""))
		setResizing(u, true)

		u.UserEcho("held then dropped")
		u.Reset()
		u.resize()

		assert.NotContains(t, u.snapshot(v), "held then dropped")
	})

	t.Run("close_flushes_held_commits", func(t *testing.T) {
		v := newVT(40, 10)
		u := newTestUI(t, v, strings.NewReader(""))
		setResizing(u, true)

		u.UserEcho("held at the end of the turn")
		u.Close() // a burst overlapping the end of a turn must not swallow output

		assert.Contains(t, v.Screen(), "held at the end of the turn")
	})
}

// TestCommitSanitizesToolOutput pins the committed boundary: Output streams
// caller bytes verbatim into the line buffer, so a motion or screen escape in
// tool output must be dropped at commit (an ESC[2J would fire the no-full-erase
// trap through caller data; a motion escape desyncs the park identically)
// while SGR survives so colored tools still read.
func TestCommitSanitizesToolOutput(t *testing.T) {
	t.Parallel()

	v := newVT(40, 10)
	u := newTestUI(t, v, strings.NewReader(""))

	u.Output("\x1b[2Jwiping\x1b[5A up\r\n\x1b[31mred line\x1b[0m\r\n")
	u.EndOutput()

	screen := u.snapshot(v)
	assert.NotContains(t, screen, "\x1b")
	assert.Contains(t, screen, "wiping up")
	assert.Contains(t, screen, "red line")
	for _, row := range v.scrollback {
		assert.NotContains(t, row, "\x1b[2J")
		assert.NotContains(t, row, "\x1b[5A")
	}
	row, col := u.cursor(v)
	assert.Equal(t, 0, col)
	assert.Equal(t, strings.Repeat(ruleChar, 39), v.Line(row), "the park sits on the divider row")
}

// countRules counts rows made up entirely of the divider glyph, at any width:
// a reflowed divider can be left spread across several rows.
func countRules(screen string) int {
	var n int
	for line := range strings.Lines(screen) {
		if l := strings.TrimRight(line, " \n"); l != "" && strings.Trim(l, ruleChar) == "" {
			n++
		}
	}
	return n
}

// TestUIResizeStorm drives a burst of resizes while tool progress updates, the
// shape of the reported corruption: gated draws never land mid-reflow, so the
// settled redraw leaves exactly one divider at every step and never duplicates
// a committed line. The history lines are short enough to survive the narrowest
// step unwrapped, so counting them stays independent of where the terminal wraps.
// The progress row carries an escape and a wide glyph, so the storm exercises
// contaminated caller text end to end.
func TestUIResizeStorm(t *testing.T) {
	t.Parallel()

	v := newVT(48, 12)
	u := newTestUI(t, v, strings.NewReader(""))
	u.render.(*inlineRenderer).t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }

	history := []string{"alpha", "bravo", "charlie", "delta"}
	for _, l := range history {
		u.Print(l)
	}
	done := u.ToolStart("write notes.go")
	defer done("")

	for _, w := range []int{20, 33, 15, 40, 26} {
		v.setSize(w, 12)
		u.mu.Lock()
		u.resizing = true // the SIGWINCH arrived; the burst is in flight
		u.mu.Unlock()
		u.SetActivity("write", "writing \u4e16\u754c notes.go \x1b[2B streaming") // gated: draws nothing
		u.resize()                                                                // the debounce settled

		screen := u.snapshot(v)
		assert.Equal(t, 1, countRules(screen), "width %d", w)
		for _, l := range history {
			// on screen or scrolled into the terminal's scrollback, never twice
			assert.LessOrEqual(t, strings.Count(screen, l), 1, "%q at width %d", l, w)
		}
		// the settled redraw parks on the block's top: the divider row itself
		row, col := u.cursor(v)
		assert.Equal(t, 0, col, "width %d", w)
		assert.Equal(t, strings.Repeat(ruleChar, max(v.w-1, 1)), v.Line(row), "width %d", w)
	}

	screen := u.snapshot(v)
	assert.Contains(t, screen, "write notes.go")
	assert.Contains(t, screen, "writing \u4e16\u754c notes.go")
}

// TestUIActivityNewlineKeepsRowCount guards invariant 2 against caller text: a
// live row carrying a newline moves the cursor a row nothing counted, so every
// later erase stops short of the block's top and strands the divider. Tool
// progress is arbitrary caller text, so this is reachable from the public API.
func TestUIActivityNewlineKeepsRowCount(t *testing.T) {
	t.Parallel()

	v := newVT(48, 12)
	u := newTestUI(t, v, strings.NewReader(""))
	u.render.(*inlineRenderer).t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }

	u.Print("committed reply line")
	done := u.ToolStart("bash: go test ./...")
	defer done("")
	u.SetActivity("bash", "running tests\nPASS pkg/tui") // multi line command output

	screen := u.snapshot(v)
	require.Equal(t, 1, countRules(screen), "the row folded onto one line")
	assert.Contains(t, screen, "running tests PASS pkg/tui")

	v.setSize(30, 12)
	u.resize()

	screen = u.snapshot(v)
	assert.Equal(t, 1, countRules(screen), "and the erase still finds the block's top")
	assert.Contains(t, screen, "committed reply line", "history is untouched")
}

// TestUILiveBlockFitsTheScreen guards the block's height. The streaming preview
// is the one part whose size follows the content rather than the terminal, so a
// reply longer than the screen is tall used to push the block past the bottom.
// Drawing a block taller than the screen scrolls it, and the previous frame's
// rows — divider included — land in scrollback, where no erase can ever reach
// them: one stranded copy per delta, compounding.
func TestUILiveBlockFitsTheScreen(t *testing.T) {
	t.Parallel()

	t.Run("long_reply_never_scrolls_the_block", func(t *testing.T) {
		v := newVT(40, 10)
		u := newTestUI(t, v, strings.NewReader(""))
		u.render.(*inlineRenderer).t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }

		for range 6 {
			u.Text("word " + strings.Repeat("filler ", 6) + "\n")
			require.Empty(t, v.scrollback, "the live block must never scroll")
			require.Equal(t, 1, countRules(u.snapshot(v)))
		}
		screen := u.snapshot(v)
		assert.Contains(t, screen, promptFirst, "the input survives the trim")
		assert.Contains(t, screen, "0/1k · test", "and so does the status row")
	})

	t.Run("narrowing_mid_stream_keeps_it_in", func(t *testing.T) {
		v := newVT(60, 10)
		u := newTestUI(t, v, strings.NewReader(""))
		u.render.(*inlineRenderer).t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }

		// narrowing re-wraps the buffered reply taller, which is how a resize
		// during streaming used to overflow the screen
		u.Text("alpha bravo charlie delta echo foxtrot golf hotel india juliet ")
		for _, w := range []int{40, 26, 18} {
			v.setSize(w, 10)
			u.Text("kilo lima mike november oscar papa quebec romeo sierra ")
			assert.Equal(t, 1, countRules(u.snapshot(v)), "width %d", w)
			assert.Contains(t, u.snapshot(v), promptFirst, "width %d", w)
		}
	})
}

// TestStreamingRowsLaysOutStructuredLines pins that a table or rule still
// buffered in textBuf previews as itself, not as a blank row: structured
// histLines carry no text, so the preview must lay them out like commit does.
func TestStreamingRowsLaysOutStructuredLines(t *testing.T) {
	t.Parallel()

	t.Run("table", func(t *testing.T) {
		u := &UI{theme: NewTheme(ColorNone), streaming: true, textBuf: "| A | B |\n|---|---|\n| 1 | 2 |"}
		rows := u.streamingRows(40)
		require.NotEmpty(t, rows)
		assert.Contains(t, strings.Join(rows, "\n"), "│ A │ B │", "the preview shows the table, not a blank")
	})
	t.Run("rule", func(t *testing.T) {
		u := &UI{theme: NewTheme(ColorNone), streaming: true, textBuf: "---"}
		rows := u.streamingRows(40)
		require.NotEmpty(t, rows)
		assert.Equal(t, strings.Repeat(ruleChar, 40), rows[0], "the preview shows the rule, not a blank")
	})
}

// TestUIResizeProbeGatesTheRedraw covers the barrier end to end: after a
// settled burst the redraw waits for the terminal's status reply, because the
// ioctl reports the new size before the emulator has reflowed to it — drawing
// on that say-so alone is what strands a divider.
func TestUIResizeProbeGatesTheRedraw(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	v := newVT(48, 12)
	u := newTestUI(t, v, pr)
	u.render.(*inlineRenderer).t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }
	u.Print("committed reply")

	v.setSize(30, 12)
	u.holdForResize() // the SIGWINCH arrived
	u.probeResize()   // the burst settled; the redraw waits on the terminal

	assert.Equal(t, 1, v.dsrCount, "the barrier probe was emitted")
	assert.Equal(t, 2, countRules(u.snapshot(v)),
		"nothing redraws until the terminal proves it caught up: the old divider stays reflowed in two rows")

	_, err := pw.Write([]byte("\x1b[0n"))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return countRules(u.snapshot(v)) == 1
	}, time.Second, 5*time.Millisecond, "the reply releases the redraw")
}

// TestUIResizeProbeTimeoutSettles covers terminals that never answer the
// status query: the grace timeout releases the redraw anyway.
func TestUIResizeProbeTimeoutSettles(t *testing.T) {
	t.Parallel()

	v := newVT(48, 12)
	u := newTestUI(t, v, strings.NewReader(""))
	u.render.(*inlineRenderer).t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }
	var fire func()
	u.afterDelay = func(_ time.Duration, fn func()) *time.Timer {
		fire = fn
		return time.NewTimer(time.Hour) // never fires inside the test
	}
	u.Print("committed reply")

	v.setSize(30, 12)
	u.holdForResize()
	u.probeResize()
	require.NotNil(t, fire)
	assert.Equal(t, 2, countRules(u.snapshot(v)), "no redraw before the grace expires")

	fire() // the terminal never answered; the grace expired
	assert.Equal(t, 2, countRules(u.snapshot(v)), "the draw still waits out its quiet grace")
	fire() // the quiet grace elapsed
	assert.Equal(t, 1, countRules(u.snapshot(v)), "the timeout releases the redraw")
}

// TestUIResizeProbeStaleReplyIgnored covers a reply answering a superseded
// probe: it proves the terminal caught up with the older burst only, so the
// redraw keeps waiting for the newer burst's own probe.
func TestUIResizeProbeStaleReplyIgnored(t *testing.T) {
	t.Parallel()

	t.Run("reply_before_newer_probe", func(t *testing.T) {
		v := newVT(48, 12)
		u := newTestUI(t, v, strings.NewReader(""))
		u.render.(*inlineRenderer).t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }
		u.Print("committed reply")

		v.setSize(30, 12)
		u.holdForResize()
		u.probeResize()
		u.holdForResize() // a newer SIGWINCH arrived before the reply
		u.probeAnswered() // the reply covers only the older probe
		assert.Equal(t, 2, countRules(u.snapshot(v)), "a stale reply proves nothing")

		u.probeResize() // the newer burst settles with its own probe
		u.probeAnswered()
		require.Eventually(t, func() bool {
			return countRules(u.snapshot(v)) == 1
		}, time.Second, testPoll, "and its fresh reply releases the redraw after the grace")
	})

	t.Run("reply_after_newer_probe", func(t *testing.T) {
		v := newVT(48, 12)
		u := newTestUI(t, v, strings.NewReader(""))
		u.render.(*inlineRenderer).t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }
		var graceArmed bool
		u.afterDelay = func(d time.Duration, _ func()) *time.Timer {
			if d == resizeDrawGrace {
				graceArmed = true
			}
			return time.NewTimer(time.Hour) // never fires inside the test
		}

		u.holdForResize()
		u.probeResize() // probe A, its reply still in flight
		u.holdForResize()
		u.probeResize()   // probe B written while A is still outstanding
		u.probeAnswered() // A's reply, not B's

		assert.False(t, graceArmed)
	})
}

// TestUIResizeDrawGraceCancelledBySignal covers the last window: the terminal
// answered and the quiet grace is running, but a new resize begins before the
// draw goes out. The draw must be abandoned — the frame would land on a grid
// the next reflow is about to move.
func TestUIResizeDrawGraceCancelledBySignal(t *testing.T) {
	t.Parallel()

	v := newVT(48, 12)
	u := newTestUI(t, v, strings.NewReader(""))
	u.render.(*inlineRenderer).t.sizeFn = func() (int, int, error) { return v.w, v.h, nil }
	var fire func()
	u.afterDelay = func(_ time.Duration, fn func()) *time.Timer {
		fire = fn
		return time.NewTimer(time.Hour) // never fires inside the test
	}
	u.Print("committed reply")

	v.setSize(30, 12)
	u.holdForResize()
	u.probeResize()
	u.probeAnswered() // the terminal caught up; the quiet grace starts
	u.holdForResize() // but a new resize begins before the draw goes out
	fire()            // the grace elapses

	assert.Equal(t, 2, countRules(u.snapshot(v)), "the draw was abandoned")
}
