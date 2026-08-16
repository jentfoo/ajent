package subagent

import (
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChildSpendRollsIntoParentLedger verifies a child's usage appears as child
// spend on the parent ledger without moving its context.
func TestChildSpendRollsIntoParentLedger(t *testing.T) {
	t.Parallel()
	parent := tokens.New(llm.Model{ID: "parent", ContextWindow: 8000})
	p, _ := scripted([]llm.ScriptedTurn{{Events: summaryTurn("s", llm.Usage{Input: 200, Output: 40})}})
	m := New(Options{
		Provider: p,
		Parent:   func() *tokens.Accounting { return parent },
	})
	defer m.Close()

	id := m.Start("q", "")
	j, ok := m.Poll(t.Context(), id)
	require.True(t, ok)
	assert.Equal(t, StatusDone, j.Status)

	require.Eventually(t, func() bool { return !tokens.Zero(parent.ChildTotal()) }, time.Second, 5*time.Millisecond)
	child := parent.ChildTotal()
	assert.Equal(t, 200, child.Input)
	assert.Equal(t, 40, child.Output)
	// the delegated subset is also part of total
	total := parent.Total()
	assert.GreaterOrEqual(t, total.Input, child.Input)
}

// TestParentContextUnchangedByChild verifies a running child does not move the
// parent's context bar.
func TestParentContextUnchangedByChild(t *testing.T) {
	t.Parallel()
	parent := tokens.New(llm.Model{ID: "parent", ContextWindow: 8000})
	g := &gatedProvider{}
	m := New(Options{
		Provider:      func(llm.Model) (llm.Provider, error) { return g, nil },
		Parent:        func() *tokens.Accounting { return parent },
		MaxConcurrent: 1,
	})
	defer m.Close()

	id := m.Start("q", "")
	require.Eventually(t, func() bool { return g.active.Load() == 1 }, time.Second, 5*time.Millisecond) // child streaming
	before := parent.Context().Used
	g.releaseAll()
	_, ok := m.Poll(t.Context(), id)
	require.True(t, ok)
	assert.Equal(t, before, parent.Context().Used, "a child's run must not move the parent context bar")
}
