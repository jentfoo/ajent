package tokens

import (
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccountingPendingLiveLifecycle(t *testing.T) {
	t.Parallel()

	const key = "p/m1"
	newLedger := func() *Accounting {
		return New(llm.Model{ID: "m1", Provider: "p", ContextWindow: 100000})
	}

	t.Run("fresh_ledger_is_empty_and_exact", func(t *testing.T) {
		a := newLedger()
		assert.Zero(t, a.Context().Used)
		assert.False(t, a.Context().Estimated)
	})

	t.Run("pending_estimate_counts_added_messages", func(t *testing.T) {
		a := newLedger()

		// an appended user message is estimated pending until the provider reports.
		added := 200
		a.Add(added)
		cs := a.Context()
		assert.True(t, cs.Estimated)
		assert.Equal(t, added, cs.Used) // pass-through of the raw estimate at factor 1
	})

	t.Run("partial_snapshot_clears_pending", func(t *testing.T) {
		a := newLedger()

		// anthropic reports input mid-stream: that snaps promptExact and clears pending.
		snapIn := 1000
		a.Partial(llm.Usage{Input: snapIn})
		cs := a.Context()
		assert.False(t, cs.Estimated) // no live yet -> exact
		assert.Equal(t, snapIn, cs.Used)
	})

	t.Run("stream_grows_used_above_snapshot", func(t *testing.T) {
		a := newLedger()

		// text deltas make the bar estimated again and grow Used above the snapshot.
		snapIn := 1000
		a.Partial(llm.Usage{Input: snapIn})
		a.Stream(50)
		cs := a.Context()
		assert.True(t, cs.Estimated)
		assert.Greater(t, cs.Used, snapIn)
	})

	t.Run("response_clears_every_bucket", func(t *testing.T) {
		a := newLedger()

		// build up both estimate buckets first so clearing them is observable.
		a.Add(300)                        // pending: appended messages
		a.Partial(llm.Usage{Input: 1000}) // exact snapshot lands mid-stream
		a.Stream(50)                      // live: response still streaming
		assert.True(t, a.Context().Estimated)

		// a completed response clears every estimate bucket: exact, no longer growing.
		a.Response(key, llm.Usage{Input: 1000, Output: 500}, 200, true)
		cs := a.Context()
		assert.False(t, cs.Estimated)  // no pending or live estimates remain
		assert.Equal(t, 1500, cs.Used) // exact input + output supersedes them
	})
}

func TestAccountingUnreportedTurnIsEstimated(t *testing.T) {
	t.Parallel()

	a := New(llm.Model{ID: "m1", Provider: "p"})
	const key = "p/m1"

	// a provider that reported nothing (llama.cpp) marks the turn estimated.
	a.Response(key, llm.Usage{}, 0, true)
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
	child.Response(key, llm.Usage{Input: in, Output: out}, 1500, true)

	assert.Zero(t, parent.Context().Used) // child's context is its own

	total := parent.Total()
	assert.Equal(t, in+out, total.Input+total.Output) // but spend rolls up

	childTotal := parent.ChildTotal()
	assert.Equal(t, in+out, childTotal.Input+childTotal.Output) // delegated subset is tracked separately
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

func TestAccountingSetBase(t *testing.T) {
	t.Parallel()

	base := 1500

	t.Run("base_before_exact", func(t *testing.T) {
		a := New(llm.Model{ID: "m1", Provider: "p"})
		// a fresh ledger owes the constant overhead (system prompt + tool schemas)
		a.SetBase(base)
		cs := a.Context()
		assert.True(t, cs.Estimated)
		assert.Equal(t, base, cs.Used)

		// an appended message estimate rides on top of the seeded base.
		msg := 300
		a.Add(msg)
		assert.Equal(t, base+msg, a.Context().Used)
	})
	t.Run("exact_supersedes_base", func(t *testing.T) {
		a := New(llm.Model{ID: "m1", Provider: "p"})
		// an exact snapshot already includes system and schemas, so the base drops out
		exact := 4000
		a.SetBase(base)
		a.Partial(llm.Usage{Input: exact}) // snaps promptExact; pending cleared
		assert.Equal(t, exact, a.Context().Used)
	})
	t.Run("set_replaces_not_adds", func(t *testing.T) {
		a := New(llm.Model{ID: "m1", Provider: "p"})
		a.SetBase(300)
		first := a.Context().Used
		a.SetBase(900)
		// replaced rather than accumulated: only the newer value shows.
		assert.Equal(t, first+600, a.Context().Used)
	})
	t.Run("reseed_reapplies_base", func(t *testing.T) {
		a := New(llm.Model{ID: "m1", Provider: "p"})
		exact := 5000
		a.SetBase(900)
		a.Partial(llm.Usage{Input: exact}) // promptExact set; base covered
		assert.Equal(t, exact, a.Context().Used)

		// compaction reseeds to a message-only estimate (promptExact back to zero);
		// the fixed overhead is owed again on top of it.
		resAfter := 2000
		a.Reseed(resAfter)
		assert.Equal(t, resAfter+900, a.Context().Used) // base persists across reseed
	})
}

func TestAccountingSetSubmit(t *testing.T) {
	t.Parallel()

	a := New(llm.Model{ID: "m1", Provider: "p"})

	t.Run("submit_counts_once", func(t *testing.T) {
		sub := 250
		a.SetSubmit(sub)
		// only the submitted bucket carries it; re-setting does not double count.
		assert.Equal(t, sub, a.Context().Used)
		a.SetSubmit(sub)
		assert.Equal(t, sub, a.Context().Used)
	})
	t.Run("cleared_on_delivery", func(t *testing.T) {
		sub := 300
		a.SetSubmit(sub)
		a.Add(sub) // the message lands; pending now owns it
		assert.Equal(t, sub*2, a.Context().Used)
		a.SetSubmit(0) // Delivered clears submit so pending alone counts
		assert.Equal(t, sub, a.Context().Used)
	})
}

func TestAccountingSetStaged(t *testing.T) {
	t.Parallel()

	a := New(llm.Model{ID: "m1", Provider: "p"})

	t.Run("staged_counts_toward_context", func(t *testing.T) {
		a.SetStaged(4000)
		cs := a.Context()
		assert.Equal(t, 4000, cs.Used)
		assert.True(t, cs.Estimated)
	})
	t.Run("replaces_rather_than_accumulates", func(t *testing.T) {
		a.SetStaged(4000)
		a.SetStaged(900) // a second report supersedes the first
		assert.Equal(t, 900, a.Context().Used)
	})
	t.Run("cleared_on_flush", func(t *testing.T) {
		a.SetStaged(900)
		a.SetSubmit(900) // the flush hands the same results to the submission
		a.SetStaged(0)
		assert.Equal(t, 900, a.Context().Used)
	})
}

func TestAccountingPreservesStaged(t *testing.T) {
	t.Parallel()

	m := llm.Model{ID: "m1", Provider: "p", ContextWindow: 32000}
	other := llm.Model{ID: "m2", Provider: "p", ContextWindow: 64000}

	// staged `!` output has not ridden any request yet, so a recount or a model
	// switch has nothing to say about it and must not drop it from the bar
	tests := []struct {
		name string
		op   func(a *Accounting)
		want int
	}{
		{"rebase", func(a *Accounting) { a.Rebase(5000) }, 5700},
		{"reseed", func(a *Accounting) { a.Reseed(5000) }, 5700},
		{"set_model", func(a *Accounting) { a.SetModel(other) }, 700},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := New(m)
			a.Add(2000)
			a.SetStaged(700)
			tc.op(a)
			cs := a.Context()
			assert.Equal(t, tc.want, cs.Used)
			assert.True(t, cs.Estimated)
		})
	}
}

func TestAccountingReseedKeepsSpendResetsContext(t *testing.T) {
	t.Parallel()

	a := New(llm.Model{ID: "m1", Provider: "p", ContextWindow: 100000})
	const key = "p/m1"

	// a reported turn establishes exact context terms and session spend. The
	// prediction matches the report so the calibration factor stays 1 and the
	// reseeded estimate below is unscaled.
	a.Response(key, llm.Usage{Input: 5000, Output: 300}, 5000, true)
	total := a.Total()
	assert.Equal(t, 5300, total.Input+total.Output)

	// compaction reseeds the context to a fresh estimate without touching spend.
	a.Reseed(1200)
	cs := a.Context()
	assert.True(t, cs.Estimated) // the reseeded figure is an estimate
	assert.Equal(t, 1200, cs.Used)
	assert.Equal(t, 5300, a.Total().Input+a.Total().Output)
}

// Spend folds out-of-context usage (a compaction summary call) into session spend
// without touching any context term, so a failed compaction cannot leave the bar
// at the summariser's prompt size.
func TestAccountingSpendCountsTotalOnly(t *testing.T) {
	t.Parallel()

	const key = "p/m1"
	a := New(llm.Model{ID: "m1", Provider: "p", ContextWindow: 100000})
	a.SetBase(400)
	before := a.Context()

	a.Spend(key, llm.Usage{Input: 90000, Output: 500})
	assert.Equal(t, 90500, a.Total().Input+a.Total().Output)

	// context terms untouched: the bar still shows only the seeded base
	assert.Equal(t, before, a.Context())

	a.Spend(key, llm.Usage{}) // zero usage is a no-op, not a zeroed total
	assert.Equal(t, 90500, a.Total().Input+a.Total().Output)
}

func TestAccountingUnreportedResponseKeepsEstimate(t *testing.T) {
	t.Parallel()

	m := llm.Model{ID: "local", Provider: "p", ContextWindow: 32000}
	a := New(m)
	a.SetBase(1200)
	a.Add(5000) // prior history plus the prompt just sent
	a.Stream(400)
	before := a.Context().Used

	// a provider reporting nothing snapped no exact term, so the estimate is still
	// all there is: wiping pending here left the bar at base and never recovered.
	a.Response(m.Key(), llm.Usage{}, 6200, true)
	after := a.Context().Used
	assert.Greater(t, after, 1200)
	assert.Equal(t, before-400, after) // only live is dropped; append re-adds the message
	assert.True(t, a.Context().Estimated)
}

func TestAccountingOutputOnlyReport(t *testing.T) {
	t.Parallel()

	m := llm.Model{ID: "m1", Provider: "p", ContextWindow: 32000}

	t.Run("response_keeps_prompt_and_counts_output", func(t *testing.T) {
		a := New(m)
		a.Response(m.Key(), llm.Usage{Input: 40000, Output: 500}, 40000, true)
		a.Add(3000)
		// a prompt-silent report must not zero the prompt term, and its output must
		// still land: append declines to re-add a message whose usage is non-zero.
		// It joins pending rather than outputExact, which the first turn's 500 holds
		// and which a second assignment would overwrite.
		a.Response(m.Key(), llm.Usage{Output: 120}, 0, true)
		cs := a.Context()
		assert.Equal(t, 40000+500+3000+120, cs.Used)
	})

	t.Run("partial_keeps_prompt_and_pending", func(t *testing.T) {
		a := New(m)
		a.Response(m.Key(), llm.Usage{Input: 40000, Output: 500}, 40000, true)
		a.Add(3000)
		a.Partial(llm.Usage{Output: 120}) // output-only mid-stream snapshot
		assert.Equal(t, 40000+500+3000, a.Context().Used)
	})
}

func TestAccountingReasoningStrippedByRetention(t *testing.T) {
	t.Parallel()

	m := llm.Model{ID: "m1", Provider: "p", ContextWindow: 32000}
	u := llm.Usage{Input: 10000, Output: 4000, Reasoning: 3000}

	kept := New(m)
	kept.Response(m.Key(), u, 10000, true)
	assert.Equal(t, 14000, kept.Context().Used)

	// under RetainNone the billed reasoning never rides in the next request
	stripped := New(m)
	stripped.Response(m.Key(), u, 10000, false)
	assert.Equal(t, 11000, stripped.Context().Used)
}

func TestAccountingPreservesComposing(t *testing.T) {
	t.Parallel()

	m := llm.Model{ID: "m1", Provider: "p", ContextWindow: 32000}
	other := llm.Model{ID: "m2", Provider: "p", ContextWindow: 64000}

	// none of these operations has anything to say about the editor buffer, so a
	// switch, recount or reseed mid-prompt must not drop what is being typed
	tests := []struct {
		name string
		op   func(a *Accounting)
		want int
	}{
		{"rebase", func(a *Accounting) { a.Rebase(5000) }, 5700},      // exact count plus the buffer
		{"reseed", func(a *Accounting) { a.Reseed(5000) }, 5700},      // reseeded estimate plus it
		{"set_model", func(a *Accounting) { a.SetModel(other) }, 700}, // context dropped, buffer kept
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := New(m)
			a.Add(2000)
			a.SetCompose(700)
			tc.op(a)
			cs := a.Context()
			assert.Equal(t, tc.want, cs.Used)
			assert.True(t, cs.Estimated) // the buffer is an estimate, so the bar says so
		})
	}
}

func TestAccountingSetWindowKeepsContext(t *testing.T) {
	t.Parallel()

	small := llm.Model{ID: "m1", Provider: "p", ContextWindow: 32000}
	big := llm.Model{ID: "m2", Provider: "p", ContextWindow: 200000}

	a := New(small)
	a.Add(9000)
	used := a.Context().Used
	require.NotZero(t, used)

	a.SetWindow(big) // reframes without dropping what was already measured
	cs := a.Context()
	assert.Equal(t, used, cs.Used)
	assert.Equal(t, 200000, cs.Window)
}

func TestAccountingRecordSpend(t *testing.T) {
	t.Parallel()

	m := llm.Model{ID: "m1", Provider: "p", ContextWindow: 32000}
	a := New(m)
	a.Add(4000)
	before := a.Context()

	a.RecordSpend(m.Key(), llm.Usage{Input: 50000, Output: 400})
	// every appended message is a recorded entry, so a user echo or tool result
	// arrives here too; counting those as turns made /usage report a 5-step session
	// as 15+ turns once it had been compacted
	a.RecordSpend(m.Key(), llm.Usage{})

	assert.Equal(t, before, a.Context()) // spend never disturbs a context term
	assert.Equal(t, 50400, a.Total().Input+a.Total().Output)
	assert.Equal(t, 1, a.TurnsCount())
	assert.Zero(t, a.EstimatedTurns())
}

func TestAccountingOutputOnlyAcrossTurns(t *testing.T) {
	t.Parallel()

	m := llm.Model{ID: "x", Provider: "p", ContextWindow: 32000}
	a := New(m)
	for range 3 {
		a.Add(100)                                          // the user message
		a.Response(m.Key(), llm.Usage{Output: 50}, 0, true) // provider reports output only
	}
	// three user messages and three responses are all still in context. Parking each
	// response in outputExact instead lost every earlier one as the next overwrote it.
	assert.Equal(t, 450, a.Context().Used)
}
