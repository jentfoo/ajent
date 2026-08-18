package session

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resolveModel(key string) (llm.Model, error) {
	return llm.Model{Provider: "p", ID: key}, nil
}

func TestStateRebuildAppliesInOrder(t *testing.T) {
	t.Parallel()

	branch := []Entry{
		modelChange("one"),
		settingReasoning(`{"level":"medium"}`),
		msgWithID("m1", llm.Text(llm.RoleUser, "hi")),
		msgWithID("m2", llm.Text(llm.RoleAssistant, "yo")),
	}
	st, warns := State(branch, resolveModel)
	assert.Empty(t, warns)
	assert.Equal(t, llm.Model{Provider: "p", ID: "one"}, st.Model)
	testLevel, ok := llm.ParseLevel("medium")
	require.True(t, ok)
	assert.Equal(t, testLevel, st.Reasoning.Level)
	require.Len(t, st.Messages, 2)
	assert.Equal(t, llm.RoleUser, st.Messages[0].Role)
}

func TestStateAppliesToolsSetting(t *testing.T) {
	t.Parallel()

	branch := []Entry{settingChange("tools", `["read","bash"]`)}
	st, warns := State(branch, resolveModel)
	assert.Empty(t, warns)
	assert.Equal(t, []string{"read", "bash"}, st.Tools)
}

func TestStateCompactionEmitsSummaryAndKeptTail(t *testing.T) {
	t.Parallel()

	branch := []Entry{
		msgWithID("m1", llm.Text(llm.RoleUser, "first")),
		msgWithID("m2", llm.Text(llm.RoleAssistant, "second")),
		{ID: "comp", Type: TypeCompaction, Data: compactData(`{"summary":"summed","firstKeptEntryId":"m3"}`)},
		msgWithID("m3", llm.Text(llm.RoleUser, "kept start")),
		msgWithID("m4", llm.Text(llm.RoleAssistant, "kept end")),
	}
	st, warns := State(branch, resolveModel)
	assert.Empty(t, warns)
	require.Len(t, st.Messages, 3) // summary + m3 + m4
	summary := st.Messages[0]
	assert.Equal(t, llm.RoleUser, summary.Role) // a user message reaches every provider
	assert.Contains(t, textOf(summary), "summed")
	assert.Equal(t, "kept start", textOf(st.Messages[1]))
}

func TestStateCompactionKeepsFromEarlierMessage(t *testing.T) {
	t.Parallel()

	// firstKeptEntryId points before the compaction entry in file order.
	branch := []Entry{
		msgWithID("m1", llm.Text(llm.RoleUser, "drop me")),
		msgWithID("m2", llm.Text(llm.RoleAssistant, "keep me")),
		{ID: "comp", Type: TypeCompaction, Data: compactData(`{"summary":"s","firstKeptEntryId":"m2"}`)},
	}
	st, warns := State(branch, resolveModel)
	assert.Empty(t, warns)
	require.Len(t, st.Messages, 2)
	assert.Equal(t, "keep me", textOf(st.Messages[1]))
}

func TestStateUnresolvableModelWarns(t *testing.T) {
	t.Parallel()

	branch := []Entry{
		modelChange("ghost"), // resolve fails on this key
		msgWithID("m1", llm.Text(llm.RoleUser, "hi")),
	}
	resolve := func(key string) (llm.Model, error) {
		return llm.Model{}, fmt.Errorf("no such model %s", key)
	}
	st, warns := State(branch, resolve)
	require.Len(t, st.Messages, 1) // the message still rebuilds
	assert.NotEmpty(t, warns)
}

func TestStateWarnsOnDanglingToolCall(t *testing.T) {
	t.Parallel()

	assistantWithCall := MessageData{Message: llm.Message{Role: llm.RoleAssistant,
		Content: llm.BlockList{llm.ToolCallBlock{ID: "c1", Name: "bash"}}}}
	st, warns := State([]Entry{{ID: "a1", Type: TypeMessage, Data: mustJSON(assistantWithCall)}}, resolveModel)
	require.Len(t, st.Messages, 1) // the tool-call message rebuilds
	assert.NotEmpty(t, warns)
}

func TestStateUnknownSettingIgnored(t *testing.T) {
	t.Parallel()

	branch := []Entry{settingChange("bogus", `1`)}
	st, warns := State(branch, resolveModel)
	assert.Empty(t, warns)
	assert.Nil(t, st.Tools)
}

func TestStateAppliesDottedToolsKey(t *testing.T) {
	t.Parallel()

	branch := []Entry{settingChange("tools.enabled", `["read","bash"]`)}
	st, warns := State(branch, resolveModel)
	assert.Empty(t, warns)
	assert.Equal(t, []string{"read", "bash"}, st.Tools)
}

func TestSettingOverridesLastWriteWins(t *testing.T) {
	t.Parallel()

	branch := []Entry{
		settingChange("reasoning", `{"level":"low"}`),
		msgWithID("m1", llm.Text(llm.RoleUser, "hi")),
		settingChange("reasoning", `{"level":"high"}`),
		settingChange("tools.enabled", `["bash"]`),
	}
	ov := SettingOverrides(branch)
	assert.JSONEq(t, `{"level":"high"}`, string(ov["reasoning"]))
	assert.JSONEq(t, `["bash"]`, string(ov["tools.enabled"]))
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

	branch := append([]Entry(nil),
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

	sum := func(u llm.Usage) int {
		return u.Input + u.Output + u.CacheRead + u.CacheWrite
	}
	// the rewound ledger holds strictly less spend than the full one.
	assert.Less(t, sum(totalsPart), sum(fullTotals))
	require.NotZero(t, sum(totalsPart)) // but it kept the earlier reported turn
}

// helpers ---------------------------------------------------------

func modelChange(key string) Entry { return entry(TypeModelChange, ModelData{Model: key}) }

func settingReasoning(v string) Entry {
	return entry(TypeSettingChange, SettingData{Key: "reasoning", Value: json.RawMessage(v)})
}

func settingChange(key, v string) Entry {
	return entry(TypeSettingChange, SettingData{Key: key, Value: json.RawMessage(v)})
}

func msgWithID(id string, m llm.Message) Entry {
	return Entry{ID: id, Type: TypeMessage, Data: mustJSON(MessageData{Message: m})}
}

func compactData(s string) []byte { return json.RawMessage(s) }

func entry(tp Type, data any) Entry {
	return Entry{Type: tp, Data: mustJSON(data)}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func textOf(m llm.Message) string {
	for _, blk := range m.Content {
		if tb, ok := blk.(llm.TextBlock); ok {
			return tb.Text
		}
	}
	return ""
}

// usageMessage builds a message entry carrying reported usage, the way a real
// assistant response is recorded.
func usageMessage(id string, text string, u llm.Usage) Entry {
	return Entry{ID: id, Type: TypeMessage, Data: mustJSON(MessageData{
		Message: llm.Text(llm.RoleAssistant, text),
		Stop:    llm.StopEndTurn,
		Usage:   u,
	})}
}
