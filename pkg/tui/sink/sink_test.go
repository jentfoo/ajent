package sink

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// headlessUI wraps a plain-mode UI plus the pipe reader that drains its output,
// so tests can snapshot what was rendered.
type headlessUI struct {
	s *Sink

	out strings.Builder
	mu  sync.Mutex
}

func newHeadless(t *testing.T) *headlessUI {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)

	h := &headlessUI{}
	go func() { // drain the render pipe into out until it closes
		buf := make([]byte, 4096)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				h.mu.Lock()
				h.out.Write(buf[:n])
				h.mu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { _ = w.Close(); _ = r.Close() })

	ui, err := tui.New(tui.Options{Mode: tui.ModePlain, In: os.Stdin, Out: w})
	if err != nil {
		t.Fatalf("cannot build headless ui: %v", err)
	}
	h.s = New(ui)
	return h
}

// rendered returns a snapshot of everything written to the render pipe so far.
func (h *headlessUI) rendered() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Clone(h.out.String())
}

func TestToolStartDisplay(t *testing.T) {
	t.Parallel()

	// the resolved label drives the header.
	t.Run("uses_label_in_header", func(t *testing.T) {
		h := newHeadless(t)
		_ = h.s.ToolStart(agent.ToolCall{Name: "bash"}, "bash: go test ./...")
		require.Eventually(t, func() bool { return strings.Contains(h.rendered(), "bash: go test ./...") },
			2*time.Second, time.Millisecond)
	})

	// a successful completion commits its Display string to history.
	t.Run("completion_commits_display_on_success", func(t *testing.T) {
		h := newHeadless(t)
		done := h.s.ToolStart(agent.ToolCall{Name: "edit"}, "")
		res := agent.ToolResult{Content: llm.BlockList{}, Display: "applied 1 edit to main.go"}
		assert.NotPanics(t, func() { done(res) })
		require.Eventually(t, func() bool { return strings.Contains(h.rendered(), "applied 1 edit to main.go") },
			2*time.Second, time.Millisecond)
	})

	// an errored completion surfaces its message.
	t.Run("completion_error_shows_message", func(t *testing.T) {
		h := newHeadless(t)
		done := h.s.ToolStart(agent.ToolCall{Name: "bash"}, "")
		res := agent.ToolResult{
			Content: llm.BlockList{llm.TextBlock{Text: "command not found"}},
			IsError: true,
		}
		assert.NotPanics(t, func() { done(res) })
		require.Eventually(t, func() bool { return strings.Contains(h.rendered(), "command not found") },
			2*time.Second, time.Millisecond)
	})

	// a completion with no Display commits nothing extra.
	t.Run("completion_no_display_commits_nothing_extra", func(t *testing.T) {
		h := newHeadless(t)
		done := h.s.ToolStart(agent.ToolCall{Name: "read"}, "")
		res := agent.ToolResult{Content: llm.BlockList{llm.TextBlock{Text: "data"}}}
		assert.NotPanics(t, func() { done(res) })

		// sync on the async drain so `after` captures everything done wrote.
		require.Eventually(t, func() bool { return strings.Contains(h.rendered(), "read") },
			2*time.Second, time.Millisecond)
		assert.NotContains(t, h.rendered(), "\n  data\n")
	})
}

func TestTurnEndFlushesThinking(t *testing.T) {
	t.Parallel()

	// an interrupt mid-thinking never delivers EventThinkingEnd; TurnEnd must flush.
	t.Run("flushes_unterminated_partial", func(t *testing.T) {
		h := newHeadless(t)
		h.s.Thinking("unterminated partial")
		h.s.TurnEnd(agent.TurnResult{})

		require.Eventually(t, func() bool { return strings.Contains(h.rendered(), "unterminated partial\n") },
			2*time.Second, time.Millisecond)
	})

	// a clean turn already flushed via EndThinking: TurnEnd must not duplicate it.
	t.Run("clean_end_thinking_is_not_duplicated", func(t *testing.T) {
		h := newHeadless(t)
		h.s.Thinking("reasoning line")
		h.s.EndThinking()

		// sync on the async drain so `before` captures everything EndThinking wrote.
		require.Eventually(t, func() bool { return strings.Contains(h.rendered(), "reasoning line\n") },
			2*time.Second, time.Millisecond)
		before := h.rendered()
		h.s.TurnEnd(agent.TurnResult{})
		assert.Equal(t, before, h.rendered(), "TurnEnd adds nothing after a clean flush")
	})
}
