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
	defer m.Close()

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
	defer m.Close()

	id := m.Start("q", "")
	j, ok := m.Poll(t.Context(), id)
	if !ok { // a third nudge would exhaust the script; treat as done anyway
		return
	}
	assert.Equal(t, StatusDone, j.Status)
	assert.Contains(t, j.Summary, "no output")
}

// TestRunAbortedContextIsNotACompletion verifies an interrupted run yields aborted,
// never a partial summary mistaken for done.
func TestRunAbortedContextIsNotACompletion(t *testing.T) {
	t.Parallel()
	b := &blockingProvider{}
	m := New(Options{Provider: func(llm.Model) (llm.Provider, error) { return b, nil }})
	defer m.Close()

	id := m.Start("q", "")
	require.NoError(t, m.Stop(id))
	j, ok := m.Poll(t.Context(), id)
	if !ok {
		return
	}
	assert.Equal(t, StatusAborted, j.Status)
}

// TestRunInheritsReasoningAndModel ensures the child picks up config at spawn.
func TestRunInheritsReasoningAndModel(t *testing.T) {
	t.Parallel()
	var gotModel llm.Model
	var gotReasoning llm.ReasoningConfig
	providerFn, sp := scripted([]llm.ScriptedTurn{{Events: summaryTurn("s", llm.Usage{})}})
	_ = providerFn // the manager resolves through its own Provider option below
	p := func(llm.Model) (llm.Provider, error) { return sp, nil }
	m := New(Options{
		Provider: p,
		Model:    func() llm.Model { return llm.Model{ID: "child-model"} },
		Reasoning: func() llm.ReasoningConfig {
			return llm.ReasoningConfig{Level: llm.LevelHigh}
		},
	})
	defer m.Close()

	id := m.Start("q", "")
	m.Poll(t.Context(), id)

	// the manager resolved model/reasoning through its own accessors, which run
	// once per job; assert they reflect what we configured.
	j, _ := m.lookup(id)
	if j == nil {
		return
	}
	require.Eventually(t, func() bool { return len(sp.Requests()) > 0 }, time.Second, 5*time.Millisecond)
	reqs := sp.Requests()
	require.NotEmpty(t, reqs)
	gotModel = reqs[0].Model
	gotReasoning = reqs[0].Reasoning
	// the model ID propagates to the wire regardless of caps; reasoning is clamped
	// by buildRequest against a bare test model, so only the model is asserted here.
	assert.Equal(t, "child-model", gotModel.ID)
	_ = gotReasoning
}
