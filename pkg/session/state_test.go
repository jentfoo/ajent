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

func TestStateCompactionRebuild(t *testing.T) {
	t.Parallel()

	// a summary plus the kept tail rebuild into messages.
	t.Run("emits_summary_and_kept_tail", func(t *testing.T) {
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
	})

	// firstKeptEntryId can point before the compaction entry in file order.
	t.Run("keeps_from_earlier_message", func(t *testing.T) {
		branch := []Entry{
			msgWithID("m1", llm.Text(llm.RoleUser, "drop me")),
			msgWithID("m2", llm.Text(llm.RoleAssistant, "keep me")),
			{ID: "comp", Type: TypeCompaction, Data: compactData(`{"summary":"s","firstKeptEntryId":"m2"}`)},
		}
		st, warns := State(branch, resolveModel)
		assert.Empty(t, warns)
		require.Len(t, st.Messages, 2)
		assert.Equal(t, "keep me", textOf(st.Messages[1]))
	})
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

func TestStateLedger(t *testing.T) {
	t.Parallel()

	// a resumed branch folds each message's reported usage back into the ledger totals.
	t.Run("rebuilds_from_recorded_usage", func(t *testing.T) {
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
	})

	// after a compaction, the rebuilt ledger's context terms reflect only surviving
	// messages; never the entries dropped by a cut. A reported turn before the cut must not inflate Used on
	// resume/rewind, or threshold auto-compaction would fire immediately even though context was just reduced.
	t.Run("ignores_summarized_away_messages", func(t *testing.T) {
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
	})

	// rewinding onto a mid-branch point yields a ledger covering only the messages before it.
	t.Run("rewind_rebuilds_for_point_only", func(t *testing.T) {
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
	})
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

func TestStateContextPrecision(t *testing.T) {
	t.Parallel()

	// a cut that rewrote the branch re-measures, so the bar wears its ~.
	t.Run("compacted_rebuild_remeasures", func(t *testing.T) {
		// the cut keeps only m4, so the next request is a short summary plus that message.
		// m4's recorded prompt describes the 150k request it was sent with, which no
		// longer exists: replaying it reported the pre-compaction size as exact, and the
		// next turn would immediately compact again.
		branch := []Entry{
			msgUsage("m1", llm.Text(llm.RoleUser, "first"), llm.Usage{}),
			msgUsage("m2", llm.Text(llm.RoleAssistant, "second"), llm.Usage{Input: 50000, Output: 400}),
			msgUsage("m3", llm.Text(llm.RoleUser, "third"), llm.Usage{}),
			msgUsage("m4", llm.Text(llm.RoleAssistant, "fourth"), llm.Usage{Input: 150000, Output: 600}),
			{ID: "comp", Type: TypeCompaction, Data: compactData(`{"summary":"a short summary","firstKeptEntryId":"m4"}`)},
		}
		st, warns := State(branch, resolveModel)
		assert.Empty(t, warns)

		cs := st.Tokens.Context()
		assert.Less(t, cs.Used, 1000)
		assert.True(t, cs.Estimated) // re-measured, so the bar must wear its ~

		// spend still counts the turns the cut removed: those tokens were billed
		total := st.Tokens.Total()
		assert.Equal(t, 201000, total.Input+total.Output)
		assert.Equal(t, 2, st.Tokens.TurnsCount()) // the two that reported, not every entry
	})

	// nothing rewrote this branch, so the recorded prompt plus output is still exactly what the next request carries.
	t.Run("untouched_rebuild_stays_exact", func(t *testing.T) {
		branch := []Entry{
			msgUsage("m1", llm.Text(llm.RoleUser, "first"), llm.Usage{}),
			msgUsage("m2", llm.Text(llm.RoleAssistant, "second"), llm.Usage{Input: 50000, Output: 400}),
		}
		st, warns := State(branch, resolveModel)
		assert.Empty(t, warns)

		cs := st.Tokens.Context()
		assert.Equal(t, 50400, cs.Used)
		assert.False(t, cs.Estimated)
	})

	// a compaction that recorded stats but changed no message must not cost the branch its exact count.
	t.Run("stats_only_reduction_stays_exact", func(t *testing.T) {
		branch := []Entry{
			msgUsage("m1", llm.Text(llm.RoleUser, "first"), llm.Usage{}),
			msgUsage("m2", llm.Text(llm.RoleAssistant, "second"), llm.Usage{Input: 8000, Output: 100}),
			{ID: "comp", Type: TypeCompaction, Data: compactData(`{"reduce":{"stats":{"failed":2}}}`)},
		}
		st, warns := State(branch, resolveModel)
		assert.Empty(t, warns)

		cs := st.Tokens.Context()
		assert.Equal(t, 8100, cs.Used)
		assert.False(t, cs.Estimated)
	})
}

func TestCompactionDataRewritesHistory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data CompactionData
		want bool
	}{
		{"cut_and_summary", CompactionData{Summary: "s", FirstKeptEntryID: "m3"}, true},
		// CutIndex falls back to the compaction entry itself, so this still truncates
		{"summary_only", CompactionData{Summary: "s"}, true},
		{"stubs_only", CompactionData{Reduce: &Reduce{Stubs: []Stub{{CallID: "c1", Limit: 200}}}}, true},
		{"drops_only", CompactionData{Reduce: &Reduce{Drop: []string{"m2"}}}, true},
		{"strip_thinking", CompactionData{Reduce: &Reduce{StripThinking: true}}, true},
		// a plan that only counted what it looked at changed nothing to re-measure
		{"stats_only", CompactionData{Reduce: &Reduce{Stats: Stats{Failed: 2}}}, false},
		{"empty", CompactionData{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.data.rewritesHistory())
		})
	}
}

func msgUsage(id string, m llm.Message, u llm.Usage) Entry {
	return Entry{ID: id, Type: TypeMessage, Data: mustJSON(MessageData{Message: m, Usage: u})}
}

func TestStateTurnCountIgnoresCompaction(t *testing.T) {
	t.Parallel()

	// the recorder persists every appended message, user echoes and tool results
	// included, so a rebuild that counted each entry as a turn reported a two-response
	// session as four the moment it was compacted, two of them as unreported turns.
	msgs := []Entry{
		msgUsage("m1", llm.Text(llm.RoleUser, "do a thing"), llm.Usage{}),
		msgUsage("m2", llm.Text(llm.RoleAssistant, "calling"), llm.Usage{Input: 1000, Output: 50}),
		msgUsage("m3", llm.Text(llm.RoleUser, "tool result"), llm.Usage{}),
		msgUsage("m4", llm.Text(llm.RoleAssistant, "done"), llm.Usage{Input: 2000, Output: 80}),
	}
	compacted := append(append([]Entry{}, msgs...), Entry{ID: "c", Type: TypeCompaction,
		Data: compactData(`{"summary":"s","firstKeptEntryId":"m3"}`)})

	plain, _ := State(msgs, resolveModel)
	rewritten, _ := State(compacted, resolveModel)

	assert.Equal(t, 2, plain.Tokens.TurnsCount())
	assert.Equal(t, plain.Tokens.TurnsCount(), rewritten.Tokens.TurnsCount())
	assert.Zero(t, rewritten.Tokens.EstimatedTurns())
}
