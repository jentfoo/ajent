package subagent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	osexec "os/exec"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
)

func TestEmptySummary(t *testing.T) {
	t.Parallel()

	// a thinking-only final message is followed by one nudge and then the real summary
	t.Run("nudges_then_summarises", func(t *testing.T) {
		p, _ := scripted([]llm.ScriptedTurn{
			{Events: thinkingOnlyTurn()}, // no text, triggers a nudge
			{Events: summaryTurn("the answer is 42", llm.Usage{})},
		})
		m := New(wired(Options{Provider: p}))
		t.Cleanup(m.Close)

		id := m.start("q", "", "")
		j, ok, _ := m.poll(t.Context(), id)
		require.True(t, ok)
		assert.Equal(t, StatusDone, j.Status)
		assert.Contains(t, j.Summary, "the answer is 42")
	})

	// a bounded retry gives up: short reasoning fails rather than reporting done
	t.Run("short_thinking_fails", func(t *testing.T) {
		p, _ := scripted([]llm.ScriptedTurn{
			{Events: thinkingOnlyTurn()}, // no text, triggers a nudge
			{Events: thinkingOnlyTurn()}, // still nothing usable after the one nudge
		})
		m := New(wired(Options{Provider: p}))
		t.Cleanup(m.Close)

		id := m.start("q", "", "")
		j, ok, _ := m.poll(t.Context(), id)
		require.True(t, ok)
		assert.Equal(t, StatusError, j.Status)
		require.ErrorIs(t, j.Err, errNoSummary)
	})

	// substantial reasoning after the nudge stands in for a missing summary.
	t.Run("long_thinking_is_summary", func(t *testing.T) {
		think := strings.Repeat("reasoning ", 30) // ~300 chars, past minThinkingSummary
		p, _ := scripted([]llm.ScriptedTurn{
			{Events: thinkingOnlyTurn()},  // no text, triggers a nudge
			{Events: thinkingTurn(think)}, // still no text, but sizable reasoning
		})
		m := New(wired(Options{Provider: p}))
		t.Cleanup(m.Close)

		id := m.start("q", "", "")
		j, ok, _ := m.poll(t.Context(), id)
		require.True(t, ok)
		assert.Equal(t, StatusDone, j.Status)
		assert.Contains(t, j.Summary, thinkingPreface)
		assert.Contains(t, j.Summary, think)
	})

	// long mid-investigation reasoning is excluded by the tool-call boundary
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
		m := New(wired(Options{
			Provider: p,
			Tools: &fakeSource{tools: []agent.Tool{&fakeTool{name: "read", result: "ok"}},
				readOnly: map[string]bool{"read": true}},
		}))
		t.Cleanup(m.Close)

		id := m.start("q", "", "")
		j, ok, _ := m.poll(t.Context(), id)
		require.True(t, ok)
		assert.Equal(t, StatusError, j.Status)
		require.ErrorIs(t, j.Err, errNoSummary)
	})
}

func TestTruncatedSummary(t *testing.T) {
	t.Parallel()

	// a partial text cut off by an output cap is nudged once, then reported done
	t.Run("partial_text_nudged_then_done", func(t *testing.T) {
		p, sp := scripted([]llm.ScriptedTurn{
			{Events: cappedTextTurn("found pkg/a.go so far")}, // stop reason max_tokens
			{Events: summaryTurn("the answer is 42", llm.Usage{})},
		})
		m := New(wired(Options{Provider: p}))
		t.Cleanup(m.Close)

		id := m.start("q", "", "")
		j, ok, _ := m.poll(t.Context(), id)
		require.True(t, ok)
		assert.Equal(t, StatusDone, j.Status)
		assert.Contains(t, j.Summary, "the answer is 42")
		assert.True(t, lastRequestEndsWith(sp, truncatedNudge))
	})

	// a retry that is also capped fails instead of reporting partial work done
	t.Run("retry_capped_fails", func(t *testing.T) {
		p, sp := scripted([]llm.ScriptedTurn{
			{Events: cappedTextTurn("partial")},
			{Events: cappedTextTurn("still partial")}, // the wrap-up turn hits the cap too
		})
		m := New(wired(Options{Provider: p}))
		t.Cleanup(m.Close)

		id := m.start("q", "", "")
		j, ok, _ := m.poll(t.Context(), id)
		require.True(t, ok)
		assert.Equal(t, StatusError, j.Status)
		require.ErrorIs(t, j.Err, errTruncated)
		assert.True(t, lastRequestEndsWith(sp, truncatedNudge))
	})

	// a turn ended by the step limit leaves tool calls unanswered in the final
	// assistant message; that counts as cut off and gets the same wrap-up turn
	t.Run("step_limit_tool_calls_nudged", func(t *testing.T) {
		toolTurn := []llm.Event{
			{Type: llm.EventTextStart, Index: 0},
			{Type: llm.EventTextDelta, Index: 0, Text: "checking"},
			{Type: llm.EventTextEnd, Index: 0, Block: llm.TextBlock{Text: "checking"}},
			{Type: llm.EventToolCallStart, Index: 1, ToolCallID: "c1", ToolName: "read"},
			{Type: llm.EventToolCallEnd, Index: 1, Block: llm.ToolCallBlock{
				ID: "c1", Name: "read", Input: json.RawMessage(`{"path":"x.go"}`)}},
			{Type: llm.EventDone, StopReason: llm.StopToolUse},
		}
		p, _ := scripted([]llm.ScriptedTurn{
			{Events: toolTurn},
			{Events: summaryTurn("done investigating", llm.Usage{})},
		})
		m := New(wired(Options{Provider: p, MaxSteps: 1})) // the child's own turn cap
		t.Cleanup(m.Close)

		id := m.start("q", "", "")
		j, ok, _ := m.poll(t.Context(), id)
		require.True(t, ok)
		assert.Equal(t, StatusDone, j.Status)
		assert.Contains(t, j.Summary, "done investigating")
	})
}

// cappedTextTurn frames an assistant text turn cut off by the output cap.
func cappedTextTurn(text string) []llm.Event {
	out := make([]llm.Event, 0, 4) // three text events plus the done event
	out = append(out,
		llm.Event{Type: llm.EventTextStart, Index: 0},
		llm.Event{Type: llm.EventTextDelta, Index: 0, Text: text},
		llm.Event{Type: llm.EventTextEnd, Index: 0, Block: llm.TextBlock{Text: text}})
	return append(out, llm.Event{Type: llm.EventDone, StopReason: llm.StopMaxTokens})
}

// lastRequestEndsWith reports whether the provider's final request carried text as
// its last user message, proving which nudge the run loop sent.
func lastRequestEndsWith(sp *llm.ScriptedProvider, text string) bool {
	reqs := sp.Requests()
	if len(reqs) == 0 {
		return false
	}
	msgs := reqs[len(reqs)-1].Messages
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != llm.RoleUser {
			continue
		}
		var sb strings.Builder
		for _, b := range msgs[i].Content {
			if tb, ok := b.(llm.TextBlock); ok {
				sb.WriteString(tb.Text)
			}
		}
		return sb.String() == text
	}
	return false
}

func TestRunAbortedContextIsNotACompletion(t *testing.T) {
	t.Parallel()

	b := &blockingProvider{}
	m := New(wired(Options{Provider: func(llm.Model) (llm.Provider, error) { return b, nil }}))
	t.Cleanup(m.Close)

	id := m.start("q", "", "")
	require.NoError(t, m.Stop(id))
	j, ok, _ := m.poll(t.Context(), id)
	require.True(t, ok)
	assert.Equal(t, StatusAborted, j.Status)
}

func TestRunInheritsModel(t *testing.T) {
	t.Parallel()

	child := llm.Model{Provider: "test", ID: "child-model", ContextWindow: 8000}
	_, sp := scripted([]llm.ScriptedTurn{{Events: summaryTurn("s", llm.Usage{Input: 100, Output: 10})}})
	p := func(llm.Model) (llm.Provider, error) { return sp, nil }
	m := New(wired(Options{
		Provider: p,
		Model:    func() llm.Model { return child },
		Parent:   func() *tokens.Accounting { return tokens.New(llm.Model{ContextWindow: 200000}) },
	}))
	t.Cleanup(m.Close)

	id := m.start("q", "", "")
	m.poll(t.Context(), id)

	require.Eventually(t, func() bool { return len(sp.Requests()) > 0 }, time.Second, 5*time.Millisecond)
	assert.Equal(t, "child-model", sp.Requests()[0].Model.ID)

	// the ledger is pinned to the parent's model at Child(); run must rebase it so
	// window/reserve follow the child's own model
	lj, ok := m.lookup(id)
	require.True(t, ok)
	c := lj.tokens.Context()
	assert.Equal(t, child.ContextWindow, c.Window)
	assert.Equal(t, child.Reserve(), c.Reserve)
	assert.Equal(t, tokens.CompactAt(child), c.Compact)
}

func TestGitInWorkTree(t *testing.T) {
	t.Parallel()

	t.Run("inside_repo", func(t *testing.T) {
		dir := t.TempDir()
		init := osexec.CommandContext(t.Context(), "git", "init", "-q")
		init.Dir = dir
		if err := init.Run(); err != nil {
			t.Skipf("git unavailable: %v", err)
		}
		assert.True(t, gitInWorkTree(t.Context(), dir))
	})

	t.Run("outside_repo", func(t *testing.T) {
		assert.False(t, gitInWorkTree(t.Context(), t.TempDir()))
	})

	t.Run("empty_cwd", func(t *testing.T) {
		assert.False(t, gitInWorkTree(t.Context(), ""))
	})
}
