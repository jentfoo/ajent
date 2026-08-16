package subagent

import (
	"encoding/json"
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

// TestSinkToolStartEnrichesBareLabel verifies a built-in whose Label is just the
// tool name still reads as real work by adding its first argument.
func TestSinkToolStartEnrichesBareLabel(t *testing.T) {
	t.Parallel()
	c := newCapture()
	s := newChildSink("sub-7", c.recordRow)

	// read/grep/find/ls return a bare-word label, so the row must pull its target.
	done := s.ToolStart(agent.ToolCall{ID: "c1", Name: "read", Input: json.RawMessage(`{"path":"pkg/tui/ui.go"}`)}, "read")
	assert.Equal(t, `sub-7  read pkg/tui/ui.go`, c.rowText("sub-7"))

	done(agent.ToolResult{})
}

// TestSinkToolStartPrefersRichLabel verifies a multi-word label is kept whole.
func TestSinkToolStartPrefersRichLabel(t *testing.T) {
	t.Parallel()
	c := newCapture()
	s := newChildSink("sub-8", c.recordRow)

	// an MCP tool already names its work; the argument should not displace it.
	done := s.ToolStart(agent.ToolCall{ID: "c1", Name: "my_tool"}, `search docs for widget`)
	assert.Equal(t, `sub-8  search docs for widget`, c.rowText("sub-8"))

	done(agent.ToolResult{})
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
// rather than collapsing to the static thinking placeholder: a multi-line delta
// scrolls past completed lines, showing only the current one.
func TestSinkTextShowsLatestOutput(t *testing.T) {
	t.Parallel()
	c := newCapture()
	s := newChildSink("sub-5", c.recordRow)

	s.Text("inspecting\npkg/tui/ui.go")
	assert.Equal(t, "sub-5  pkg/tui/ui.go", c.rowText("sub-5"))
}

// TestSinkTextScrollsPerLine verifies streaming deltas show only the current
// in-progress line: completed lines scroll past and each newline starts fresh.
func TestSinkTextScrollsPerLine(t *testing.T) {
	t.Parallel()
	c := newCapture()
	s := newChildSink("sub-9", c.recordRow)

	for _, d := range []string{"first line ", "scrolled\nsecond ", "line grows"} {
		s.Text(d)
	}
	// the first completed line is gone; only the active one remains on screen
	require.Eventually(t, func() bool { return c.rowText("sub-9") == "sub-9  second line grows" }, time.Second, 5*time.Millisecond)

	s.TurnEnd(agent.TurnResult{}) // next turn starts a fresh line
	require.Eventually(t, func() bool { return c.rowText("sub-9") == "" }, time.Second, 5*time.Millisecond)
}

// TestSinkTextIgnoresWhitespaceLines verifies a blank or whitespace-only current
// line never publishes a row, so empty streaming lines don't flash.
func TestSinkTextIgnoresWhitespaceLines(t *testing.T) {
	t.Parallel()
	c := newCapture()
	s := newChildSink("sub-10", c.recordRow)

	s.Text("\n   \t\n")
	assert.Empty(t, c.rowText("sub-10"), "a whitespace-only line publishes nothing")

	s.Text("real content")
	assert.Equal(t, "sub-10  real content", c.rowText("sub-10"))
}

// TestSinkThinkingShowsReasoning verifies the child's chain-of-thought is surfaced
// rather than collapsed to a placeholder.
func TestSinkThinkingShowsReasoning(t *testing.T) {
	t.Parallel()
	c := newCapture()
	s := newChildSink("sub-6", c.recordRow)

	s.Thinking("look at pkg/tui\nthen decide")
	assert.Equal(t, "sub-6  then decide", c.rowText("sub-6"))
}

// TestSinkStreamSwitchStartsFresh verifies moving from thinking to text resets the
// row so prose does not append onto leftover reasoning.
func TestSinkStreamSwitchStartsFresh(t *testing.T) {
	t.Parallel()
	c := newCapture()
	s := newChildSink("sub-11", c.recordRow)

	s.Thinking("reasoning here")
	s.Text("the answer")
	require.Eventually(t, func() bool { return c.rowText("sub-11") == "sub-11  the answer" }, time.Second, 5*time.Millisecond)
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
