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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testPoll = time.Millisecond

// newTestUI drives a UI in inline mode against the emulator, with no real
// terminal behind it.
func newTestUI(t *testing.T, v *vt, in io.Reader) *UI {
	t.Helper()
	u := &UI{
		theme:    NewTheme(ColorNone),
		render:   newTestInline(v),
		mode:     ModeInline,
		status:   Status{Model: "test", MaxTokens: 1000},
		in:       in,
		inFd:     -1,
		msgs:     make(chan string),
		controls: make(chan Control, 4),
		done:     make(chan struct{}),
	}
	u.reader = newInputReader(in)
	go u.reader.run()
	go u.readKeys()
	u.mu.Lock() // a key may already be decoding; serialize with readKeys
	u.repaint()
	u.mu.Unlock()
	t.Cleanup(u.Close)
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
		assert.Equal(t, promptFirst+inputHint, v.Line(0))
		// the leftmost working glyph always leads the status line
		assert.Equal(t, spinnerFrames[0]+" · ░░░░░░░░░░ 0/1k · test", v.Line(1))
	})
	t.Run("typed_text_replaces_hint", func(t *testing.T) {
		_, err := io.WriteString(pw, "hi")
		require.NoError(t, err)
		waitLine(0, promptFirst+"hi")
	})
	t.Run("enter_submits", func(t *testing.T) {
		_, err := io.WriteString(pw, "\r")
		require.NoError(t, err)
		assert.Equal(t, "hi", <-u.Messages())
		waitLine(0, promptFirst+inputHint)
	})
	t.Run("status_follows_the_input", func(t *testing.T) {
		u.SetStatus(Status{Model: "opus-5"})
		// the working glyph always leads, then the model
		assert.Equal(t, spinnerFrames[0]+" · opus-5", v.Line(1))
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
		waitLine(0, promptFirst+"one tw")

		_, err = io.WriteString(pw, "\x17")
		require.NoError(t, err)
		waitLine(0, promptFirst+"one")
	})
	t.Run("alt_enter_grows_the_block", func(t *testing.T) {
		_, err := io.WriteString(pw, "\x1b\rsecond")
		require.NoError(t, err)
		waitLine(1, promptCont+"second")
		waitLine(2, spinnerFrames[0]+" · ░░░░░░░░░░ 0/1k · test")
	})
	t.Run("ctrl_c_clears_buffer", func(t *testing.T) {
		_, err := io.WriteString(pw, "\x03")
		require.NoError(t, err)
		waitLine(0, promptFirst+inputHint)
		assert.Empty(t, u.line(v, 2), "the extra input row is gone")
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
		assert.Contains(t, v2.Line(1), promptFirst, "preview row is released after commit")
	})
	t.Run("block_follows_the_last_line", func(t *testing.T) {
		assert.Contains(t, v.Line(12), promptFirst)
		assert.Contains(t, v.Line(13), "0/1k · test")
	})
}

func TestUIDiff(t *testing.T) {
	t.Parallel()

	v := newVT(60, 12)
	u := newTestUI(t, v, strings.NewReader(""))

	u.Diff("pkg/client/retry.go", "a\nb\n", "a\nB\n")

	assert.Equal(t, "pkg/client/retry.go +1 -1", v.Line(0))
	assert.Contains(t, v.Line(1), "@@")
	assert.Equal(t, " a", v.Line(2))
	assert.Equal(t, "-b", v.Line(3))
	assert.Equal(t, "+B", v.Line(4))

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
	statusRow := u.line(v, 2) // committed header on row 0, live block starts at row 1: input then status
	assert.True(t, strings.HasPrefix(stripANSI(statusRow), spinnerFrames[0]),
		"spinner still leads the bottom-left status line while a tool runs")
	assert.Contains(t, stripANSI(statusRow), "bash: go test ./...",
		"running tool label sits next to the working glyph in the status bar")

	done("ok  0.4s")

	assert.Equal(t, "  ok  0.4s", v.Line(1))
	assert.Contains(t, v.Line(2), promptFirst, "no spinner row is left behind")
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
	statusRow := u.line(v, 2) // committed header on row 0; live block starts at row 1 (input), status on row 2
	assert.Contains(t, stripANSI(statusRow), "bash: go test ./...")
	doneTool("ok  0.4s")

	stop()
}

func TestUIStatusSpinnerLeftmost(t *testing.T) {
	t.Parallel()

	v := newVT(60, 10)
	u := newTestUI(t, v, strings.NewReader(""))

	// the spinner is the first element of the status line, before model and tokens.
	statusRow := u.line(v, 1) // live block: input on row 0, status on row 1
	assert.True(t, strings.HasPrefix(stripANSI(statusRow), spinnerFrames[0]),
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
		assert.Contains(t, stripANSI(v.Line(0)), "(reverse-i-search)`':")
	})
	t.Run("typing_narrows", func(t *testing.T) {
		press(key{typ: keyRune, text: "retry"})
		// the header shows the query and detail; the full match renders below it
		assert.Contains(t, stripANSI(v.Line(0)), "(reverse-i-search)`retry':  2026-01-02 03:04 UTC")
		assert.Contains(t, v.Line(1), "fix the retry loop")
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
	for i := 0; i < 10000; i++ { // spin until submit lands (readKeys writes it)
		u.mu.Lock()
		done := len(u.editor.history) == 1
		u.mu.Unlock()
		if done {
			break
		}
	}

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
