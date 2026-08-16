package subagent

import (
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSinkToolStartPublishesLabel verifies a tool call shows its label under the
// job id, and the done hook restores the idle fallback.
func TestSinkToolStartPublishesLabel(t *testing.T) {
	t.Parallel()
	c := newCapture()
	s := newChildSink("sub-2", c.recordRow)

	done := s.ToolStart(agent.ToolCall{ID: "c1", Name: "grep"}, `grep "func New" pkg`)
	assert.Equal(t, `sub-2  grep "func New" pkg`, c.rowText("sub-2"))

	done(agent.ToolResult{})
	assert.Equal(t, "sub-2  thinking…", c.rowText("sub-2"), "the done hook falls back to the idle line")
}

// TestSinkOneLineCollapsesWhitespace verifies a label with newlines becomes one row.
func TestSinkOneLineCollapsesWhitespace(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "a b c", oneLine("  a\n\tb   \nc "))
}

// TestSinkTurnEndClearsRow verifies the row disappears on turn end.
func TestSinkTurnEndClearsRow(t *testing.T) {
	t.Parallel()
	c := newCapture()
	s := newChildSink("sub-3", c.recordRow)
	s.set(thinkingRow(s.id), true)
	assert.Equal(t, "sub-3  thinking…", c.rowText("sub-3"))

	s.TurnEnd(agent.TurnResult{})
	require.Eventually(t, func() bool { return c.rowText("sub-3") == "" }, time.Second, 5*time.Millisecond)
}

// TestSinkThinkingCoalesces verifies rapid deltas settle on a single publish.
func TestSinkThinkingCoalesces(t *testing.T) {
	t.Parallel()
	c := newCapture()
	s := newChildSink("sub-4", c.recordRow)
	for i := 0; i < 50; i++ { // all within the flush window -> one coalesced row
		s.set(thinkingRow(s.id), false)
	}
	assert.Equal(t, "sub-4  thinking…", c.rowText("sub-4"))
}

// TestSinkTextShowsLatestOutput verifies Text passes the child's output through
// rather than collapsing to the static thinking placeholder.
func TestSinkTextShowsLatestOutput(t *testing.T) {
	t.Parallel()
	c := newCapture()
	s := newChildSink("sub-5", c.recordRow)

	s.Text("inspecting\n  pkg/tui/ui.go")
	assert.Equal(t, "sub-5  inspecting pkg/tui/ui.go", c.rowText("sub-5"))
}

// TestSinkThinkingStaysPlaceholder verifies reasoning deltas still settle on the
// thinking line even though Text now shows real output.
func TestSinkThinkingStaysPlaceholder(t *testing.T) {
	t.Parallel()
	c := newCapture()
	s := newChildSink("sub-6", c.recordRow)

	s.Thinking("some reasoning")
	assert.Equal(t, "sub-6  thinking…", c.rowText("sub-6"))
}

// TestSinkNilPubIsSafe verifies a nil publisher never dereferences.
func TestSinkNilPubIsSafe(t *testing.T) {
	t.Parallel()
	s := newChildSink("sub-1", nil)
	done := s.ToolStart(agent.ToolCall{ID: "c", Name: "read"}, "read")
	done(agent.ToolResult{})
	s.Thinking("x")
	s.Text("y")
	s.TurnEnd(agent.TurnResult{})
}
