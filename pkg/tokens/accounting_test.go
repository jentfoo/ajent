package tokens

import (
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
)

func TestAccountingPendingLiveLifecycle(t *testing.T) {
	t.Parallel()

	a := New(llm.Model{ID: "m1", Provider: "p", ContextWindow: 100000})
	const key = "p/m1"

	assert.Zero(t, a.Context().Used)
	assert.False(t, a.Context().Estimated)

	// an appended user message is estimated pending until the provider reports.
	added := 200
	a.Add(added)
	cs := a.Context()
	assert.True(t, cs.Estimated)
	assert.Equal(t, added, cs.Used) // pass-through of the raw estimate at factor 1

	// anthropic reports input mid-stream: that snaps promptExact and clears pending.
	snapIn := 1000
	a.Partial(llm.Usage{Input: snapIn})
	cs = a.Context()
	assert.False(t, cs.Estimated) // no live yet -> exact
	assert.Equal(t, snapIn, cs.Used)

	// text deltas make the bar estimated again and grow Used above the snapshot.
	a.Stream(50)
	cs = a.Context()
	assert.True(t, cs.Estimated)
	assert.Greater(t, cs.Used, snapIn)

	// a completed response clears every estimate bucket: exact, no longer growing.
	in2 := 1000
	out2 := 500
	a.Response(key, llm.Usage{Input: in2, Output: out2}, 200)
	cs = a.Context()
	assert.False(t, cs.Estimated)
}

func TestAccountingUnreportedTurnIsEstimated(t *testing.T) {
	t.Parallel()

	a := New(llm.Model{ID: "m1", Provider: "p"})
	const key = "p/m1"

	// a provider that reported nothing (llama.cpp) marks the turn estimated.
	a.Response(key, llm.Usage{}, 0)
	assert.Equal(t, 1, a.TurnsCount())
	assert.Equal(t, 1, a.EstimatedTurns())

	// spend totals stay zero for an unreported response; context stays at zero too
	assert.Zero(t, a.Context().Used)
}

func TestAccountingChildRollsUpSpendNotContext(t *testing.T) {
	t.Parallel()

	parent := New(llm.Model{ID: "m1", Provider: "p"})
	const key = "p/m1"

	child := parent.Child()
	in, out := 2000, 300
	// child spends on its own context; the parent must not see that used.
	child.Response(key, llm.Usage{Input: in, Output: out}, 1500)

	assert.Zero(t, parent.Context().Used) // child's context is its own

	total := parent.Total()
	assert.Equal(t, in+out, total.Input+total.Output) // but spend rolls up
}

func TestAccountingSetModelRebasesContext(t *testing.T) {
	t.Parallel()

	a := New(llm.Model{ID: "a", Provider: "p", ContextWindow: 40000})

	// accumulate an estimate, then switch models mid-session.
	a.Add(300)
	assert.True(t, a.Context().Estimated)

	b := llm.Model{ID: "b", Provider: "p", ContextWindow: 200000}
	a.SetModel(b)

	cs := a.Context()
	assert.Equal(t, b.ContextWindow, cs.Window) // window follows the new model
	assert.False(t, cs.Estimated)               // stale estimates dropped on rebase
}

func TestAccountingComposeGrowsAndClears(t *testing.T) {
	t.Parallel()

	a := New(llm.Model{ID: "m1", Provider: "p"})

	// composing text raises Used and marks it estimated, exactly once per buffer.
	paste := strings.Repeat("x", 40000) // ~10k tokens of prose
	a.SetCompose(EstimateText(paste, KindProse))
	cs := a.Context()
	assert.True(t, cs.Estimated)
	big := cs.Used

	// replacing the buffer with far less text shrinks Used rather than accumulating.
	a.SetCompose(1000)
	small := a.Context().Used
	assert.Less(t, small, big)

	// clearing (submit empties the editor -> empty text estimates to zero).
	a.SetCompose(EstimateText("", KindProse))
	cs = a.Context()
	assert.Zero(t, cs.Used)
	assert.False(t, cs.Estimated)
}
