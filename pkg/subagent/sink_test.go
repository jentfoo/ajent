package subagent

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolStart(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		id      string
		call    agent.ToolCall
		label   string
		wantRow string // row immediately after ToolStart
	}{
		{"bare builtin label", "sub-2",
			agent.ToolCall{ID: "c1", Name: "grep"}, `grep "func New" pkg`,
			`sub-2  grep "func New" pkg`},
		{"enriches bare label", "sub-7",
			agent.ToolCall{ID: "c1", Name: "read", Input: json.RawMessage(`{"path":"pkg/tui/ui.go"}`)}, "read",
			`sub-7  read pkg/tui/ui.go`},
		{"prefers rich label", "sub-8",
			agent.ToolCall{ID: "c1", Name: "my_tool"}, `search docs for widget`,
			`sub-8  search docs for widget`},
		{"collapses_whitespace_to_one_line", "sub-12",
			agent.ToolCall{ID: "c1", Name: "multi_line_tool"}, "first\tline\nsecond   \nthird",
			"sub-12  first line second third"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCapture()
			s := newChildSink(tc.id, numberOf(tc.id), c.recordRow)
			done := s.ToolStart(tc.call, tc.label)
			assert.Equal(t, tc.wantRow, c.rowText(tc.id))
			done(agent.ToolResult{})
			// a broken helper must not mask it: assert the literal idle line.
			assert.Equal(t, tc.id+"  thinking…", c.rowText(tc.id)) // done falls back to the idle line
		})
	}
}

func TestSinkTurnEndFallsBackToIdle(t *testing.T) {
	t.Parallel()
	c := newCapture()
	s := newChildSink("sub-3", 3, c.recordRow)
	s.set(rowLine(s.id, "read pkg/tui/ui.go"), true)
	assert.Equal(t, "sub-3  read pkg/tui/ui.go", c.rowText("sub-3"))

	s.TurnEnd(agent.TurnResult{})
	assert.Equal(t, "sub-3  thinking…", c.rowText("sub-3"))
	assert.NotContains(t, c.rows, "sub-3|", "a live job's row is never cleared by a turn ending")
}

func TestToolStartParallelCalls(t *testing.T) {
	t.Parallel()
	c := newCapture()
	s := newChildSink("sub-2", 2, c.recordRow)
	s.set(thinkingRow(s.id), true)

	doneA := s.ToolStart(agent.ToolCall{ID: "c1", Name: "read",
		Input: json.RawMessage(`{"path":"a.go"}`)}, "read")
	assert.Equal(t, "sub-2  read a.go", c.rowText("sub-2"))

	doneB := s.ToolStart(agent.ToolCall{ID: "c2", Name: "grep"}, `grep "New" pkg`)
	assert.Equal(t, `sub-2  grep "New" pkg`, c.rowText("sub-2"))

	doneA(agent.ToolResult{}) // b still runs; its label stays on the row
	assert.Equal(t, `sub-2  grep "New" pkg`, c.rowText("sub-2"))

	doneB(agent.ToolResult{}) // the last one out restores the idle line
	assert.Equal(t, "sub-2  thinking…", c.rowText("sub-2"))
}

func TestSinkThinkingCoalesces(t *testing.T) {
	t.Parallel()
	c := newCapture()
	s := newChildSink("sub-4", 4, c.recordRow)
	for i := 0; i < 50; i++ { // all within the flush window -> one coalesced row
		s.set(thinkingRow(s.id), false)
	}
	assert.Equal(t, "sub-4  thinking…", c.rowText("sub-4"))
}

func TestSinkText(t *testing.T) {
	t.Parallel()

	// Text passes the child's output through rather than collapsing to the static
	// thinking placeholder: a multi-line delta scrolls past completed lines, showing only the current one.
	t.Run("shows_latest_output", func(t *testing.T) {
		c := newCapture()
		s := newChildSink("sub-5", 5, c.recordRow)

		s.Text("inspecting\npkg/tui/ui.go")
		assert.Equal(t, "sub-5  pkg/tui/ui.go", c.rowText("sub-5"))
	})

	// streaming deltas show only the current in-progress line: completed lines scroll past and each newline starts fresh.
	t.Run("scrolls_per_line", func(t *testing.T) {
		c := newCapture()
		s := newChildSink("sub-9", 9, c.recordRow)

		for _, d := range []string{"first line ", "scrolled\nsecond ", "line grows"} {
			s.Text(d)
		}
		// the first completed line is gone; only the active one remains on screen
		require.Eventually(t, func() bool { return c.rowText("sub-9") == "sub-9  second line grows" }, time.Second, 5*time.Millisecond)

		s.TurnEnd(agent.TurnResult{}) // next turn starts a fresh line, row stays live
		require.Eventually(t, func() bool { return c.rowText("sub-9") == "sub-9  thinking…" }, time.Second, 5*time.Millisecond)
	})

	// a blank or whitespace-only current line never publishes a row, so empty streaming lines don't flash.
	t.Run("ignores_whitespace_lines", func(t *testing.T) {
		c := newCapture()
		s := newChildSink("sub-10", 10, c.recordRow)

		s.Text("\n   \t\n")
		assert.Empty(t, c.rowText("sub-10"))

		s.Text("real content")
		assert.Equal(t, "sub-10  real content", c.rowText("sub-10"))
	})
}

func TestSinkThinkingShowsReasoning(t *testing.T) {
	t.Parallel()
	c := newCapture()
	s := newChildSink("sub-6", 6, c.recordRow)

	s.Thinking("look at pkg/tui\nthen decide")
	assert.Equal(t, "sub-6  then decide", c.rowText("sub-6"))
}

func TestSinkStreamSwitchStartsFresh(t *testing.T) {
	t.Parallel()
	c := newCapture()
	s := newChildSink("sub-11", 11, c.recordRow)

	s.Thinking("reasoning here")
	s.Text("the answer")
	require.Eventually(t, func() bool { return c.rowText("sub-11") == "sub-11  the answer" }, time.Second, 5*time.Millisecond)
}
