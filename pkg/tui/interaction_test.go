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

	"github.com/jentfoo/ajent/pkg/strutil"
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
			i, err := u.SelectContext(t.Context(), "Permission:", []Option{{Label: "Allow"}, {Label: "Deny"}})
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
			_, err := u.SelectContext(t.Context(), "Pick:", []Option{{Label: "A"}, {Label: "B"}})
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
			i, _ := u.SelectContext(t.Context(), "Pick:", []Option{{Label: "A"}, {Label: "B"}, {Label: "C"}})
			result <- i
		}()

		waitFor(t, u, v, "Pick:")
		press(t, pw, "3")

		assert.Equal(t, 2, <-result)
	})

	t.Run("cancel_commits_nothing", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		errCh := make(chan error, 1)
		go func() {
			_, err := u.SelectContext(t.Context(), "Enable compaction?", []Option{{Label: "Yes"}, {Label: "No"}})
			errCh <- err
		}()

		waitFor(t, u, v, "Enable compaction?")
		press(t, pw, "\x1b")
		require.ErrorIs(t, <-errCh, ErrCancelled)

		// the prompt is back and the never-made choice was not recorded
		waitFor(t, u, v, userMarker)
		assert.NotContains(t, u.snapshot(v), "Enable compaction? Yes")
	})

	t.Run("commits_one_summary_line", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		done := make(chan struct{})
		go func() {
			_, _ = u.SelectContext(t.Context(), "Permission:", []Option{{Label: "Allow"}})
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
			i, _ := u.SelectContext(t.Context(), "Pick:", []Option{{Label: "A"}, {Label: "B"}})
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
		_, err := u.SelectContext(t.Context(), "Pick:", nil)
		assert.ErrorIs(t, err, ErrCancelled)
	})
}

func TestUIConfirm(t *testing.T) {
	t.Parallel()

	u, v, pw := interactionUI(t)

	result := make(chan bool, 1)
	go func() {
		ok, _ := u.ConfirmContext(t.Context(), "Proceed?")
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
			s, err := u.InputContext(t.Context(), "Name:", "your name")
			assert.NoError(t, err)
			result <- s
		}()

		waitFor(t, u, v, "Name:")
		press(t, pw, "ajent")
		waitFor(t, u, v, "ajent")
		press(t, pw, "\r")

		assert.Equal(t, "ajent", <-result)
	})

	t.Run("secret_never_echoes_the_value", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		result := make(chan string, 1)
		go func() {
			s, err := u.InputSecretContext(t.Context(), "Key:", "paste key")
			assert.NoError(t, err)
			result <- s
		}()

		waitFor(t, u, v, "Key:")
		waitFor(t, u, v, "paste key")
		press(t, pw, "hunter2")
		require.Eventually(t, func() bool {
			return strings.Count(u.snapshot(v), secretMask) == 7
		}, time.Second, testPoll, "screen never showed 7 bullets")
		assert.NotContains(t, u.snapshot(v), "hunter2")
		press(t, pw, "\r")

		assert.Equal(t, "hunter2", <-result)
		// the committed summary keeps the mask too, never the value
		waitFor(t, u, v, noticeMarker+" Key: "+strings.Repeat(secretMask, 7))
		assert.NotContains(t, u.snapshot(v), "hunter2")
	})

	t.Run("backspace_deletes", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		result := make(chan string, 1)
		go func() {
			s, _ := u.InputContext(t.Context(), "Name:", "")
			result <- s
		}()

		waitFor(t, u, v, "Name:")
		press(t, pw, "abc\x7f")
		waitFor(t, u, v, "ab")
		press(t, pw, "\r")

		assert.Equal(t, "ab", <-result)
	})
}

func TestPickItemRowRoleTags(t *testing.T) {
	t.Parallel()

	th := NewTheme(Color256, DefaultPalette())
	lay := pickLayout{tagCol: 5, marked: true}

	t.Run("user_tag_hue", func(t *testing.T) {
		row := pickItemRow(th, PickItem{Tag: "user", Mark: MarkUser, Label: "the question"}, false, 80, lay)
		assert.Equal(t, selectIndent+th.UserTag.Wrap("user")+"  "+Style{}.Wrap("the question"), row)
	})

	t.Run("agent_tag_hue", func(t *testing.T) {
		row := pickItemRow(th, PickItem{Tag: "agent", Mark: MarkAssistant, Label: "a reply"}, false, 80, lay)
		assert.Equal(t, selectIndent+th.Assist.Wrap("agent")+" "+Style{}.Wrap("a reply"), row)
	})

	t.Run("tool_tag_hue", func(t *testing.T) {
		row := pickItemRow(th, PickItem{Tag: "tool", Mark: MarkTool, Label: "[ls] docs/"}, false, 80, lay)
		assert.Equal(t, selectIndent+th.ToolTag.Wrap("tool")+"  "+Style{}.Wrap("[ls] docs/"), row)
	})

	t.Run("off_branch_faint", func(t *testing.T) {
		row := pickItemRow(th, PickItem{Tag: "user", Mark: MarkUser, Label: "abandoned", Off: true}, false, 80, lay)
		assert.Equal(t, selectIndent+th.UserTagOff.Wrap("user")+"  "+th.Dim.Wrap("abandoned"), row)
	})

	t.Run("selected_accents_body", func(t *testing.T) {
		row := pickItemRow(th, PickItem{Tag: "user", Mark: MarkUser, Label: "picked"}, true, 80, lay)
		assert.Equal(t, selectMarker+th.UserTag.Wrap("user")+"  "+th.Accent.Wrap("picked"), row)
	})

	t.Run("untagged_dims_among_tags", func(t *testing.T) {
		row := pickItemRow(th, PickItem{Label: "an untagged row"}, false, 80, pickLayout{marked: true})
		assert.Equal(t, selectIndent+th.Dim.Wrap("an untagged row"), row)
	})

	t.Run("unmarked_list_stays_plain", func(t *testing.T) { // gray is reserved for disabled rows
		row := pickItemRow(th, PickItem{Label: "a model"}, false, 80, pickLayout{})
		assert.Equal(t, selectIndent+Style{}.Wrap("a model"), row)
	})
}

func TestPickItemRowAlignment(t *testing.T) {
	t.Parallel()

	th := NewTheme(Color256, DefaultPalette())

	items := []PickItem{
		{Tag: "user", Mark: MarkUser, Label: "├── a question"},
		{Tag: "agent", Mark: MarkAssistant, Label: "│   a reply"},
		{Tag: "tool", Mark: MarkTool, Label: "│   [ls] docs/"},
		{Tag: "compact", Mark: MarkTool, Label: "│   12k → 4k"},
		{Label: "an untagged row"},
	}
	col := tagColumn(items)
	assert.Equal(t, 7, col)

	want := -1
	for _, it := range items {
		row := strutil.StripANSI(pickItemRow(th, it, false, 80, pickLayout{tagCol: col, marked: true}))
		guide := strings.Index(row, it.Label)
		require.GreaterOrEqual(t, guide, 0)
		if want < 0 {
			want = guide
		}
		assert.Equalf(t, want, guide, "label column must not move with the tag (tag=%q)", it.Tag)
	}
}

func TestPickItemRowNoColorMarksActive(t *testing.T) {
	t.Parallel()

	th := NewTheme(ColorNone, DefaultPalette())

	live := pickItemRow(th, PickItem{Tag: "user", Mark: MarkUser, Label: "in context"}, false, 80, pickLayout{tagCol: 4, marked: true})
	off := pickItemRow(th, PickItem{Tag: "user", Mark: MarkUser, Label: "abandoned", Off: true}, false, 80, pickLayout{tagCol: 4, marked: true})
	assert.Equal(t, selectIndent+"* user in context", live)
	assert.Equal(t, selectIndent+"  user abandoned", off)
	assert.Equal(t, displayWidth(live)-len("in context"), displayWidth(off)-len("abandoned"))

	// lists with no tags keep their gutter-free rendering and a plain body
	plain := pickItemRow(th, PickItem{Label: "a model"}, false, 80, pickLayout{})
	assert.Equal(t, selectIndent+"a model", plain)
}

func TestPickItemDisabledStaysGrayUnderCursor(t *testing.T) {
	t.Parallel()

	th := NewTheme(Color256, DefaultPalette())

	// a disabled row keeps its dim body under the cursor so no accent suggests
	// the locked setting can be chosen; an editable one accents as always.
	dim := pickItemRow(th, PickItem{Label: "Reasoning", Disabled: true}, true, 80, pickLayout{})
	assert.Equal(t, selectMarker+th.Dim.Wrap("Reasoning"), dim)

	cur := pickItemRow(th, PickItem{Label: "Model"}, true, 80, pickLayout{})
	assert.Equal(t, selectMarker+th.Accent.Wrap("Model"), cur)
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
			i, err := u.PickContext(t.Context(), "Model", items, PickOptions{})
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
			i, _ := u.PickContext(t.Context(), "Model", items, PickOptions{})
			result <- i
		}()

		waitFor(t, u, v, "Model")
		press(t, pw, "opus") // an alias, not shown in the row
		waitFor(t, u, v, "anthropic/claude-opus")
		press(t, pw, "\r")

		assert.Equal(t, 0, <-result)
	})

	t.Run("matches_role_tag", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		tagged := []PickItem{
			{Tag: "agent", Mark: MarkAssistant, Label: "a reply"},
			{Tag: "user", Mark: MarkUser, Label: "a question"},
		}
		result := make(chan int, 1)
		go func() {
			i, _ := u.PickContext(t.Context(), "Rewind to", tagged, PickOptions{})
			result <- i
		}()

		waitFor(t, u, v, "Rewind to")
		press(t, pw, "user") // the role word lives in Tag, not Label
		waitFor(t, u, v, "user  a question")
		press(t, pw, "\r")

		assert.Equal(t, 1, <-result)
	})

	t.Run("initial_selection_honoured", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		result := make(chan int, 1)
		go func() {
			i, _ := u.PickContext(t.Context(), "Model", items, PickOptions{Initial: 2})
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
			_, err := u.PickContext(t.Context(), "Model", items, PickOptions{})
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

		go func() { _, _ = u.PickContext(t.Context(), "Model", items, PickOptions{}) }()

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
			i, _ := u.PickContext(t.Context(), "Model", many, PickOptions{})
			result <- i
		}()

		waitFor(t, u, v, "Model")
		require.Eventually(t, func() bool {
			return liveRowCount(u.snapshot(v)) <= 5
		}, time.Second, testPoll)

		press(t, pw, "\r")
		assert.Equal(t, 0, <-result)
	})

	t.Run("keeps_its_header_on_a_four_row_screen", func(t *testing.T) {
		// the cap floor plus an unreserved overflow footer used to push the block
		// past the screen, and the clamp then dropped the picker's own header row
		v := newVT(60, 4)
		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pw.Close() })
		u := newTestUI(t, v, pr)

		go func() { _, _ = u.PickContext(t.Context(), "Model", pickItemsOf(30), PickOptions{}) }()

		waitFor(t, u, v, "30 of 30")
		assert.LessOrEqual(t, liveRowCount(u.snapshot(v)), 4)
		press(t, pw, "\x1b")
	})

	t.Run("long_list_scrolls_within_the_cap", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		many := make([]PickItem, 300)
		for i := range many {
			many[i] = PickItem{Label: "model-" + strconv.Itoa(i)}
		}
		go func() { _, _ = u.PickContext(t.Context(), "Model", many, PickOptions{}) }()

		waitFor(t, u, v, "300 of 300")
		waitFor(t, u, v, "more")

		// the live block must stay inside its share of a 12 row screen
		require.Eventually(t, func() bool {
			return liveRowCount(u.snapshot(v)) <= 12
		}, time.Second, testPoll)

		press(t, pw, "\x1b")
	})

	t.Run("silent_pick_skips_the_summary_line", func(t *testing.T) {
		u, v, pw := interactionUI(t)

		items := []PickItem{{Label: "zai/glm-5.2"}, {Label: "openrouter/claude"}}
		done := make(chan struct{})
		go func() {
			_, _ = u.PickContext(t.Context(), "Model", items, PickOptions{Silent: true})
			close(done)
		}()

		waitFor(t, u, v, "Model")
		press(t, pw, "\r")
		<-done

		// the picker still resolves and reverts to the input prompt, but no
		// `! Model <key>` line is committed when a caller announces itself.
		waitFor(t, u, v, userMarker)
		assert.NotContains(t, u.snapshot(v), "! Model")
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
		theme:  NewTheme(ColorNone, DefaultPalette()),
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
		i, err := u.SelectContext(t.Context(), "Permission:", []Option{{Label: "Allow"}, {Label: "Deny"}})
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
			i, _ := u.SelectContext(t.Context(), "First:", []Option{{Label: "A"}})
			first <- i
		}()
		waitFor(t, u, v, "First:")

		go func() {
			i, _ := u.SelectContext(t.Context(), "Second:", []Option{{Label: "B"}})
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
			_, err := u.SelectContext(t.Context(), "First:", []Option{{Label: "A"}})
			errs <- err
		}()
		waitFor(t, u, v, "First:")
		go func() {
			_, err := u.SelectContext(t.Context(), "Second:", []Option{{Label: "B"}})
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

	t.Run("one_free_row_keeps_the_block", func(t *testing.T) {
		// a three row screen leaves one free row for the interaction: the waiting
		// indicator must yield instead of pushing every redraw past the clamp
		v := newVT(80, 3)
		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pw.Close() })
		u := newTestUI(t, v, pr)

		first := make(chan int, 1)
		go func() {
			i, _ := u.SelectContext(t.Context(), "First:", []Option{{Label: "A"}})
			first <- i
		}()
		waitFor(t, u, v, "First:")

		second := make(chan int, 1)
		go func() {
			i, _ := u.SelectContext(t.Context(), "Second:", []Option{{Label: "B"}})
			second <- i
		}()
		require.Eventually(t, func() bool {
			u.mu.Lock()
			defer u.mu.Unlock()
			return len(u.queue) == 2
		}, time.Second, testPoll)

		screen := u.snapshot(v)
		assert.NotContains(t, screen, "waiting")
		assert.Contains(t, screen, "First:")
		// nothing was shaved off the top: the rule still leads the block
		assert.Equal(t, strings.Repeat(ruleChar, 79), v.Line(0))
	})

	t.Run("approval_queue_on_three_rows", func(t *testing.T) {
		v := newVT(80, 3)
		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pw.Close() })
		u := newTestUI(t, v, pr)

		// a decision dialog always shows one option, so its fixed chrome spends the
		// reservation itself and the prompt survives without the indicator row
		d1 := u.OpenDecision(DecisionRequest{Prompt: "Approve one:", Options: []Option{{Label: "Allow"}, {Label: "Deny"}}})
		d2 := u.OpenDecision(DecisionRequest{Prompt: "Approve two:", Options: []Option{{Label: "Allow"}, {Label: "Deny"}}})
		t.Cleanup(func() { d1.Close(); d2.Close() }) // still open, resolve nothing

		screen := u.snapshot(v)
		assert.NotContains(t, screen, "waiting")
		assert.Contains(t, screen, "Approve one:")
	})

	t.Run("interaction_on_a_closed_ui", func(t *testing.T) {
		u, _, _ := interactionUI(t)
		u.Close()

		_, err := u.SelectContext(t.Context(), "Pick:", []Option{{Label: "A"}})
		assert.ErrorIs(t, err, ErrCancelled)
	})
}

func TestUIWait(t *testing.T) {
	t.Parallel()

	// a select whose ctx and answer are both ready picks at random, so the answer
	// must survive whichever branch runs: repeat until every branch has been taken
	t.Run("answer_beats_a_cancelled_ctx", func(t *testing.T) {
		u, _, _ := interactionUI(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		for range 50 {
			p := newPending(&selectState{options: []Option{{Label: "A"}}})
			require.NoError(t, u.enqueue(p))
			p.resolve(nil) // answered before wait looks at the ctx
			require.NoError(t, u.wait(ctx, p))
		}
	})

	t.Run("cancelled_ctx_reports_cancelled", func(t *testing.T) {
		u, _, _ := interactionUI(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		p := newPending(&selectState{options: []Option{{Label: "A"}}})
		require.NoError(t, u.enqueue(p))

		assert.ErrorIs(t, u.wait(ctx, p), ErrCancelled)
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

func TestInteractionMaxRows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		free, editorCap int
		waiting         bool
		want            int
	}{
		{"two_thirds_of_free_rows", 30, 3, false, 20},
		{"floor_beats_a_small_share", 4, 1, false, minInteractionRows},
		{"never_over_the_editor_cap_plus_one", 8, 5, false, 6},
		{"queued_row_shares_the_rows", 6, 3, true, 4},
		{"queued_row_at_two_free", 2, 1, true, 1},
		{"floor_defeats_the_reservation", 1, 0, true, 1},
		{"squeezed_below_its_share", 2, 10, false, 2},
		{"one_row_when_squeezed_flat", 0, 5, false, 1},
		{"never_zero", -3, 0, true, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, interactionMaxRows(tc.free, tc.editorCap, tc.waiting))
		})
	}

	t.Run("always_outgrows_the_input", func(t *testing.T) {
		// spec: an interaction's cap is a larger fraction of the screen than the
		// input's, so on any terminal with room it beats the editor's own share
		for h := 5; h <= 60; h++ {
			editorCap := max(1, (h-1)/maxInputRatio)
			free := h - 2 // a rule row and a status row are drawn above and below
			assert.Greater(t, interactionMaxRows(free, editorCap, false), editorCap, "height %d", h)
		}
	})

	t.Run("leaves_room_for_the_queued_row", func(t *testing.T) {
		for free := 3; free <= 40; free++ {
			rows := interactionMaxRows(free, 3, true)
			assert.LessOrEqual(t, rows+1, free) // the indicator still shares those rows
		}
	})
}

func TestInteractionRows(t *testing.T) {
	t.Parallel()

	// renders act active at queue[0] with queued more waiting behind it, into an
	// exact row budget, as repaint does.
	render := func(t *testing.T, act interactor, queued, free int) ([]string, int, int) {
		t.Helper()

		u := newTestUI(t, newVT(60, 12), strings.NewReader(""))
		u.mu.Lock()
		defer u.mu.Unlock()

		p := newPending(act)
		u.act = p
		u.queue = append(u.queue, p) // promote keeps the active one at queue[0]
		for range queued {
			u.queue = append(u.queue, newPending(&selectState{options: []Option{{Label: "x"}}}))
		}
		return u.interactionRows(59, free)
	}

	waitingShown := func(rows []string) bool {
		return strings.Contains(strings.Join(rows, "\n"), "waiting")
	}

	selects := &selectState{prompt: "Pick:", options: []Option{{Label: "A"}, {Label: "B"}, {Label: "C"}}}

	t.Run("indicator_shares_the_budget", func(t *testing.T) {
		rows, _, _ := render(t, selects, 1, 6)
		require.NotEmpty(t, rows)
		assert.True(t, waitingShown(rows))
		assert.LessOrEqual(t, len(rows), 6)
	})

	t.Run("no_queue_draws_no_indicator", func(t *testing.T) {
		rows, _, _ := render(t, selects, 0, 6)
		require.NotEmpty(t, rows)
		assert.False(t, waitingShown(rows))
	})

	t.Run("indicator_keeps_the_caret", func(t *testing.T) {
		in := &inputState{label: "Name:", value: "ajent"}
		_, soloRow, soloCol := render(t, in, 0, 4)
		rows, caretRow, caretCol := render(t, in, 1, 4)
		require.Len(t, rows, 2)            // the input row plus the waiting indicator
		assert.Equal(t, soloRow, caretRow) // sits below the interaction
		assert.Equal(t, soloCol, caretCol)
	})

	t.Run("one_free_row_drops_the_indicator", func(t *testing.T) {
		// the floor of one spends the reservation: the dialog keeps its only row
		for _, free := range []int{0, 1} {
			rows, _, _ := render(t, selects, 1, free)
			require.Len(t, rows, 1)
			assert.False(t, waitingShown(rows))
		}
	})

	t.Run("fixed_chrome_drops_the_indicator", func(t *testing.T) {
		// a decision dialog always shows one option, so at a cap of one it emits
		// two rows and spends the reservation itself (two parallel approvals)
		dialog := &decisionState{prompt: "Approve:", options: []Option{{Label: "Allow"}, {Label: "Deny"}}}
		rows, _, _ := render(t, dialog, 1, 2)
		require.NotEmpty(t, rows)
		assert.LessOrEqual(t, len(rows), 2)
		assert.False(t, waitingShown(rows))
	})
}
