package subagent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEmptySummary covers the nudge-then-summarise retry for thinking-only replies.
func TestEmptySummary(t *testing.T) {
	t.Parallel()

	// a thinking-only final message is followed by one nudge and then the real summary.
	t.Run("nudges_then_summarises", func(t *testing.T) {
		p, _ := scripted([]llm.ScriptedTurn{
			{Events: thinkingOnlyTurn()}, // no text; triggers a nudge
			{Events: summaryTurn("the answer is 42", llm.Usage{})},
		})
		m := New(Options{Provider: p})
		t.Cleanup(m.Close)

		id := m.Start("q", "")
		j, ok := m.Poll(t.Context(), id)
		require.True(t, ok)
		assert.Equal(t, StatusDone, j.Status)
		assert.Contains(t, j.Summary, "the answer is 42")
	})

	// a bounded retry gives up: short reasoning fails rather than reporting done.
	t.Run("short_thinking_fails", func(t *testing.T) {
		p, _ := scripted([]llm.ScriptedTurn{
			{Events: thinkingOnlyTurn()}, // no text; triggers a nudge
			{Events: thinkingOnlyTurn()}, // still nothing usable after the one nudge
		})
		m := New(Options{Provider: p})
		t.Cleanup(m.Close)

		id := m.Start("q", "")
		j, ok := m.Poll(t.Context(), id)
		require.True(t, ok)
		assert.Equal(t, StatusError, j.Status)
		require.ErrorIs(t, j.Err, errNoSummary)
	})

	// substantial reasoning after the nudge stands in for a missing summary.
	t.Run("long_thinking_is_summary", func(t *testing.T) {
		think := strings.Repeat("reasoning ", 30) // ~300 chars, past minThinkingSummary
		p, _ := scripted([]llm.ScriptedTurn{
			{Events: thinkingOnlyTurn()},  // no text; triggers a nudge
			{Events: thinkingTurn(think)}, // still no text, but sizable reasoning
		})
		m := New(Options{Provider: p})
		t.Cleanup(m.Close)

		id := m.Start("q", "")
		j, ok := m.Poll(t.Context(), id)
		require.True(t, ok)
		assert.Equal(t, StatusDone, j.Status)
		assert.Contains(t, j.Summary, thinkingPreface)
		assert.Contains(t, j.Summary, think)
	})

	// long mid-investigation reasoning is excluded by the tool-call boundary.
	t.Run("tool_turn_thinking_ignored", func(t *testing.T) {
		longThink := strings.Repeat("reasoning ", 30) // past minThinkingSummary, but pre-tool
		toolTurn := []llm.Event{
			{Type: llm.EventThinkingStart, Index: 0},
			{Type: llm.EventThinkingDelta, Index: 0, Text: longThink},
			{Type: llm.EventThinkingEnd, Index: 0, Block: llm.ThinkingBlock{Text: longThink}},
			{Type: llm.EventToolCallStart, Index: 1, ToolCallID: "c1", ToolName: "read"},
			{Type: llm.EventToolCallEnd, Index: 1, Block: llm.ToolCallBlock{
				ID: "c1", Name: "read", Input: json.RawMessage(`{"path":"x.go"}`)}},
			{Type: llm.EventDone, StopReason: llm.StopToolUse},
		}
		p, _ := scripted([]llm.ScriptedTurn{
			{Events: toolTurn},           // long reasoning mid-investigation
			{Events: thinkingOnlyTurn()}, // the turn after the tool call is empty
			{Events: thinkingOnlyTurn()}, // nudge response still short and empty
		})
		m := New(Options{
			Provider: p,
			Tools: &fakeSource{tools: []agent.Tool{&fakeTool{name: "read", result: "ok"}},
				readOnly: map[string]bool{"read": true}},
		})
		t.Cleanup(m.Close)

		id := m.Start("q", "")
		j, ok := m.Poll(t.Context(), id)
		require.True(t, ok)
		assert.Equal(t, StatusError, j.Status)
		require.ErrorIs(t, j.Err, errNoSummary)
	})
}

// TestRunAbortedContextIsNotACompletion verifies an interrupted run yields aborted,
// never a partial summary mistaken for done.
func TestRunAbortedContextIsNotACompletion(t *testing.T) {
	t.Parallel()
	b := &blockingProvider{}
	m := New(Options{Provider: func(llm.Model) (llm.Provider, error) { return b, nil }})
	t.Cleanup(m.Close)

	id := m.Start("q", "")
	require.NoError(t, m.Stop(id))
	j, ok := m.Poll(t.Context(), id)
	require.True(t, ok)
	assert.Equal(t, StatusAborted, j.Status)
}

// TestRunInheritsModel verifies the child picks up config at spawn.
func TestRunInheritsModel(t *testing.T) {
	t.Parallel()
	_, sp := scripted([]llm.ScriptedTurn{{Events: summaryTurn("s", llm.Usage{})}})
	p := func(llm.Model) (llm.Provider, error) { return sp, nil }
	m := New(Options{
		Provider: p,
		Model:    func() llm.Model { return llm.Model{ID: "child-model"} },
	})
	t.Cleanup(m.Close)

	id := m.Start("q", "")
	m.Poll(t.Context(), id)

	require.Eventually(t, func() bool { return len(sp.Requests()) > 0 }, time.Second, 5*time.Millisecond)
	assert.Equal(t, "child-model", sp.Requests()[0].Model.ID)
}
