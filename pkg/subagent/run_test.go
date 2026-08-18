package subagent

import (
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEmptySummaryNudgesThenSummarises verifies a thinking-only final message is
// followed by one nudge and then the real summary.
func TestEmptySummaryNudgesThenSummarises(t *testing.T) {
	t.Parallel()
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
}

// TestEmptySummaryAfterTwoNudgesIsPlaceholder verifies the bounded retry gives up
// and returns a placeholder rather than looping.
func TestEmptySummaryAfterTwoNudgesIsPlaceholder(t *testing.T) {
	t.Parallel()
	// three thinking-only turns: initial + two nudges, then nothing useful
	p, _ := scripted([]llm.ScriptedTurn{
		{Events: thinkingOnlyTurn()},
		{Events: thinkingOnlyTurn()},
		{Events: thinkingOnlyTurn()},
	})
	m := New(Options{Provider: p})
	t.Cleanup(m.Close)

	id := m.Start("q", "")
	j, ok := m.Poll(t.Context(), id)
	require.True(t, ok)
	assert.Equal(t, StatusDone, j.Status)
	assert.Contains(t, j.Summary, "no output")
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
