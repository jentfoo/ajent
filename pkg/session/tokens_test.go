package session

import (
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// usageMessage builds a message entry carrying reported usage, the way a real
// assistant response is recorded.
func usageMessage(id string, text string, u llm.Usage) Entry {
	return Entry{ID: id, Type: TypeMessage, Data: mustJSON(MessageData{
		Message: llm.Text(llm.RoleAssistant, text),
		Stop:    llm.StopEndTurn,
		Usage:   u,
	})}
}

// TestStateRebuildsLedgerFromRecordedUsage asserts a resumed branch folds each
// message's reported usage back into the ledger totals.
func TestStateRebuildsLedgerFromRecordedUsage(t *testing.T) {
	t.Parallel()

	branch := []Entry{
		entry(TypeSession, SessionData{Model: "p/m1"}),
		usageMessage("a1", "first reply", llm.Usage{Input: 1000, Output: 200}),
		msgWithID("u2", llm.Text(llm.RoleUser, "more")), // no usage -> estimated
		usageMessage("a2", "second reply", llm.Usage{Input: 500, Output: 50}),
	}

	st, warns := State(branch, resolveModel)
	assert.Empty(t, warns)

	totals := st.Tokens.Total()
	assert.Equal(t, 1000+200+500+50,
		totals.Input+totals.Output+totals.CacheRead+totals.CacheWrite)

	// a recorded response snaps the context exact terms to its input+output.
	cs := st.Tokens.Context()
	assert.False(t, cs.Estimated)
}

// TestStateLedgerIgnoresSummarizedAwayMessages asserts that after a compaction,
// the rebuilt ledger's context terms reflect only surviving messages — never the
// entries dropped by a cut. A reported turn before the cut must not inflate Used on
// resume/rewind, or threshold auto-compaction would fire immediately even though
// context was just reduced.
func TestStateLedgerIgnoresSummarizedAwayMessages(t *testing.T) {
	t.Parallel()

	branch := make([]Entry, 0, 4)
	branch = append(branch,
		entry(TypeSession, SessionData{Model: "p/m1"}),
		usageMessage("a0", "summarized away reply", llm.Usage{Input: 4000, Output: 1000}), // before the cut
		msgWithID("u1", llm.Text(llm.RoleUser, "kept ask")),                               // no usage -> estimated
	)
	// a compaction whose firstKept is u1 folds everything before it into the summary.
	branch = append(branch,
		Entry{ID: "comp", Type: TypeCompaction, Data: compactData(`{"summary":"s","firstKeptEntryId":"u1"}`)})

	stCompact, warns := State(branch, resolveModel)
	assert.Empty(t, warns)
	require.Len(t, stCompact.Messages, 2) // summary + kept ask

	// the same history without a cut: a0's exact terms stay in context.
	fullBranch := branch[:len(branch)-1]
	stFull, _ := State(fullBranch, resolveModel)

	// skipping summarized-away entries must lower Used below what the full (uncut)
	// ledger reports; without the fix both would carry a0's 4000+1000 exact terms.
	assert.Less(t, stCompact.Tokens.Context().Used, stFull.Tokens.Context().Used,
		"a cut that summarizes away a reported turn must shrink Used")
}

// TestStateRewindRebuildsLedgerForPointOnly asserts rewinding onto a mid-branch
// point yields a ledger covering only the messages before it.
func TestStateRewindRebuildsLedgerForPointOnly(t *testing.T) {
	t.Parallel()

	branch := []Entry{
		entry(TypeSession, SessionData{Model: "p/m1"}),
		usageMessage("a1", "first reply", llm.Usage{Input: 1000, Output: 200}),
		msgWithID("u2", llm.Text(llm.RoleUser, "more")), // no usage -> estimated
		usageMessage("a2", "second reply", llm.Usage{Input: 500, Output: 50}),
	}

	stFull, warns := State(branch, resolveModel)
	assert.Empty(t, warns)

	// rewind before the last assistant message: only a1's spend survives.
	rewound := branch[:3]
	stPart, warns := State(rewound, resolveModel)
	assert.Empty(t, warns)

	totalsPart := stPart.Tokens.Total()
	fullTotals := stFull.Tokens.Total()

	// the rewound ledger holds strictly less spend than the full one.
	sum := func(u llm.Usage) int {
		return u.Input + u.Output + u.CacheRead + u.CacheWrite
	}
	assert.Less(t, sum(totalsPart), sum(fullTotals))
	require.NotZero(t, sum(totalsPart)) // but it kept the earlier reported turn
}
