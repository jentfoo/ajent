package sink

import (
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tui"
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

// TestContextInfo pins ContextInfo's field list against tokens.ContextState, so
// a new ledger term fails here until the conversion propagates it.
func TestContextInfo(t *testing.T) {
	t.Parallel()

	t.Run("fields_track_context_state", func(t *testing.T) {
		src, dst := reflect.TypeFor[tokens.ContextState](), reflect.TypeFor[tui.ContextInfo]()
		require.Equal(t, src.NumField(), dst.NumField())
		for i := range src.NumField() {
			assert.Equal(t, src.Field(i).Name, dst.Field(i).Name)
			assert.Equal(t, src.Field(i).Type, dst.Field(i).Type)
		}
	})

	t.Run("maps_every_field", func(t *testing.T) {
		var in tokens.ContextState
		// fill every field with a distinct value, so an unmapped one cannot slip through as zero
		v := reflect.ValueOf(&in).Elem()
		for i := range v.NumField() {
			switch f := v.Field(i); f.Kind() {
			case reflect.Bool:
				f.SetBool(true)
			case reflect.String:
				f.SetString("x")
			default:
				f.SetInt(int64(i + 1))
			}
		}
		out := ContextInfo(in)
		src, dst := reflect.ValueOf(in), reflect.ValueOf(out)
		for i := range src.NumField() {
			name := src.Type().Field(i).Name
			assert.Equal(t, src.Field(i).Interface(), dst.FieldByName(name).Interface())
		}
	})
}

func TestToolStartDisplay(t *testing.T) {
	t.Parallel()

	// the resolved label drives the header
	t.Run("uses_label_in_header", func(t *testing.T) {
		h := newHeadless(t)
		_ = h.s.ToolStart(agent.ToolCall{Name: "bash"}, "bash: go test ./...", false)
		require.Eventually(t, func() bool { return strings.Contains(h.rendered(), "bash: go test ./...") },
			2*time.Second, time.Millisecond)
	})

	// a successful completion commits its Display string to history
	t.Run("completion_commits_display_on_success", func(t *testing.T) {
		h := newHeadless(t)
		done := h.s.ToolStart(agent.ToolCall{Name: "edit"}, "", false)
		res := agent.ToolResult{Content: llm.BlockList{}, Display: "applied 1 edit to main.go"}
		assert.NotPanics(t, func() { done(res) })
		require.Eventually(t, func() bool { return strings.Contains(h.rendered(), "applied 1 edit to main.go") },
			2*time.Second, time.Millisecond)
	})

	// an errored completion surfaces its message
	t.Run("completion_error_shows_message", func(t *testing.T) {
		h := newHeadless(t)
		done := h.s.ToolStart(agent.ToolCall{Name: "bash"}, "", false)
		res := agent.ToolResult{
			Content: llm.BlockList{llm.TextBlock{Text: "command not found"}},
			IsError: true,
		}
		assert.NotPanics(t, func() { done(res) })
		require.Eventually(t, func() bool { return strings.Contains(h.rendered(), "command not found") },
			2*time.Second, time.Millisecond)
	})

	// a completion with no Display commits nothing extra
	t.Run("completion_no_display_commits_nothing_extra", func(t *testing.T) {
		h := newHeadless(t)
		done := h.s.ToolStart(agent.ToolCall{Name: "read"}, "", false)
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

	// an interrupt mid-thinking never delivers EventThinkingEnd, so TurnEnd must flush
	t.Run("flushes_unterminated_partial", func(t *testing.T) {
		h := newHeadless(t)
		h.s.Thinking("unterminated partial")
		h.s.TurnEnd(agent.TurnResult{})

		require.Eventually(t, func() bool { return strings.Contains(h.rendered(), "unterminated partial\n") },
			2*time.Second, time.Millisecond)
	})

	// a clean turn already flushed via EndThinking: TurnEnd must not duplicate it
	t.Run("clean_end_thinking_is_not_duplicated", func(t *testing.T) {
		h := newHeadless(t)
		h.s.Thinking("reasoning line")
		h.s.EndThinking()

		// sync on the async drain so `before` captures everything EndThinking wrote
		require.Eventually(t, func() bool { return strings.Contains(h.rendered(), "reasoning line\n") },
			2*time.Second, time.Millisecond)
		before := h.rendered()
		h.s.TurnEnd(agent.TurnResult{})
		assert.Equal(t, before, h.rendered())
	})
}

func TestTurnEndFlushesText(t *testing.T) {
	t.Parallel()

	// an interrupt mid-reply never delivers EventTextEnd, so TurnEnd must commit
	// the tail so the preview cannot linger into the next turn.
	t.Run("flushes_unterminated_tail", func(t *testing.T) {
		h := newHeadless(t)
		h.s.TurnStart(agent.TurnInfo{})
		h.s.Text("an answer cut short")
		h.s.TurnEnd(agent.TurnResult{Stop: llm.StopAborted})

		require.Eventually(t, func() bool { return strings.Contains(h.rendered(), "short\n") },
			2*time.Second, time.Millisecond)
	})

	// a clean turn already flushed via EndText: TurnEnd must not duplicate it
	t.Run("clean_end_text_is_not_duplicated", func(t *testing.T) {
		h := newHeadless(t)
		h.s.TurnStart(agent.TurnInfo{})
		h.s.Text("full answer")
		h.s.EndText()

		// sync on the async drain so `before` captures everything EndText wrote
		require.Eventually(t, func() bool { return strings.Contains(h.rendered(), "full answer\n") },
			2*time.Second, time.Millisecond)
		before := h.rendered()
		h.s.TurnEnd(agent.TurnResult{})
		assert.Equal(t, before, h.rendered())
	})
}
