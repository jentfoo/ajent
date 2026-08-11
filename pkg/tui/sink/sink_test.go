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

// eventuallyContains polls f until it contains want, since rendering lands on
// the pipe asynchronously.
func eventuallyContains(t *testing.T, f func() string, want string) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(f(), want) {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func TestToolStartUsesLabelInHeader(t *testing.T) {
	t.Parallel()

	h := newHeadless(t)
	_ = h.s.ToolStart(agent.ToolCall{Name: "bash"}, "bash: go test ./...")
	assert.True(t, eventuallyContains(t, h.rendered, "bash: go test ./..."),
		"the resolved label drives the header")
}

func TestToolCompletionCommitsDisplayOnSuccess(t *testing.T) {
	t.Parallel()

	h := newHeadless(t)
	done := h.s.ToolStart(agent.ToolCall{Name: "edit"}, "")
	res := agent.ToolResult{Content: llm.BlockList{}, Display: "applied 1 edit to main.go"}
	assert.NotPanics(t, func() { done(res) })
	assert.True(t, eventuallyContains(t, h.rendered, "applied 1 edit to main.go"),
		"history shows the display string")
}

func TestToolCompletionErrorShowsMessage(t *testing.T) {
	t.Parallel()

	h := newHeadless(t)
	done := h.s.ToolStart(agent.ToolCall{Name: "bash"}, "")
	res := agent.ToolResult{
		Content: llm.BlockList{llm.TextBlock{Text: "command not found"}},
		IsError: true,
	}
	assert.NotPanics(t, func() { done(res) })
	assert.True(t, eventuallyContains(t, h.rendered, "command not found"),
		"the error message reaches the user")
}

func TestToolCompletionNoDisplayCommitsNothingExtra(t *testing.T) {
	t.Parallel()

	h := newHeadless(t)
	done := h.s.ToolStart(agent.ToolCall{Name: "read"}, "")
	res := agent.ToolResult{Content: llm.BlockList{llm.TextBlock{Text: "data"}}}
	assert.NotPanics(t, func() { done(res) })
	assert.False(t, eventuallyContains(t, h.rendered, "\n  data\n"),
		"streamed content alone is not re-committed as a Display")
}
