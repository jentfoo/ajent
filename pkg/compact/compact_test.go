package compact

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mustMsgs returns the messages ContextMessages would send for branch under cd.
func mustMsgs(t *testing.T, branch []session.Entry, cd session.CompactionData) []llm.Message {
	t.Helper()
	msgs, warns := session.ContextMessages(branch, cd, nil)
	require.Empty(t, warns)
	return msgs
}

// A manual /compact must fold older turns into a summary even when the whole
// session sits far below half the window, so it never reports "nothing to compact"
// on an ordinary short conversation. It keeps only the most recent user turn.
func TestCompactForceSummarisesSmallSession(t *testing.T) {
	t.Parallel()

	branch := []session.Entry{
		userText("u1", strings.Repeat("first ask ", 40)),
		assistText("a1", strings.Repeat("first answer ", 60)),
		userText("u2", "second question"),
		assistText("a2", strings.Repeat("current reply ", 10)),
	}
	called := false
	run := func(_ context.Context, _ llm.Request) (string, error) {
		called = true
		return "## Goal\nrefactor auth", nil
	}
	// a huge window: without Force the whole branch fits and nothing would happen.
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 1000}

	res, err := Compact(t.Context(), branch, model, run, Options{Force: true})
	require.NoError(t, err)
	require.NotNil(t, res) // must reduce, not report "nothing to compact"
	assert.True(t, called)
	assert.Equal(t, "## Goal\nrefactor auth", res.Summary)

	// the kept tail starts at u2 (the most recent real user prompt): only the final
	// exchange survives verbatim; everything before it is summarised away.
	require.NotEmpty(t, res.FirstKeptEntryID)
	tail := []string{}
	keep := false
	for _, e := range branch {
		if e.ID == res.FirstKeptEntryID {
			keep = true
		}
		if keep && e.Type == session.TypeMessage {
			tail = append(tail, e.ID)
		}
	}
	assert.Equal(t, []string{"u2", "a2"}, tail)
	// the summary plus one exchange must cost less than all four messages.
	cd := session.CompactionData{Summary: res.Summary, FirstKeptEntryID: res.FirstKeptEntryID}
	after := tokens.EstimateMessages(mustMsgs(t, branch, cd))
	before := tokens.EstimateMessages(mustMsgs(t, branch, session.CompactionData{}))
	assert.Less(t, after, before)
}

// A forced compact on a single exchange has no older turn to keep: the whole
// history folds into a summary-only plan. The summariser's reply decides whether
// that is recorded or surfaces as an error.
func TestCompactForceSingleTurnSummaryOnly(t *testing.T) {
	t.Parallel()

	branch := []session.Entry{
		userText("u1", "Read me a short sci-fi story"),
		assistText("a1", strings.Repeat("Mira ran her hand along the spines. ", 100)),
	}
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 1000}

	t.Run("valid_summary_rebuilds_context", func(t *testing.T) {
		run := func(_ context.Context, _ llm.Request) (string, error) {
			return "## Goal\nwanted a story\n## Progress\n### Done\n- [x] told The Last Librarian", nil
		}

		res, err := Compact(t.Context(), branch, model, run, Options{Force: true})
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Empty(t, res.FirstKeptEntryID) // nothing survives verbatim
		assert.NotEmpty(t, res.Summary)
		assert.Less(t, res.After, res.Before)

		// once recorded, the rebuilt context is the summary plus anything newer.
		recorded := append(slices.Clone(branch), compactEntry("c1", res.Summary, ""), userText("u2", "another?"))
		msgs, warns := session.ContextMessages(recorded, session.CompactionData{Summary: res.Summary}, nil)
		require.Empty(t, warns)
		require.Len(t, msgs, 2)
		assert.Contains(t, textOf(msgs[0]), "The Last Librarian")
		assert.Equal(t, "another?", textOf(msgs[1]))
	})

	t.Run("blank_summary_is_error", func(t *testing.T) {
		run := func(_ context.Context, _ llm.Request) (string, error) { return "  \n", nil }

		res, err := Compact(t.Context(), branch, model, run, Options{Force: true})
		require.Error(t, err)
		assert.Nil(t, res)
		assert.Contains(t, err.Error(), "empty summary")
	})
}

// A forced compact whose summary cannot shrink the conversation is refused, never
// recorded as a growth.
func TestCompactForceRefusesWhenSummaryWouldGrow(t *testing.T) {
	t.Parallel()

	branch := []session.Entry{userText("u1", "hi"), assistText("a1", "yo")}
	run := func(_ context.Context, _ llm.Request) (string, error) {
		return "## Goal\n" + strings.Repeat("wordy ", 50), nil
	}
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 1000}

	res, err := Compact(t.Context(), branch, model, run, Options{Force: true})
	require.NoError(t, err)
	assert.Nil(t, res) // a summary that grows context is never recorded
}

// Regression: a cut that keeps nearly everything and adds a summary on top grows
// the context; it must never be recorded as a compaction ("compacted 500 -> 688").
func TestCompactNeverRecordsGrowth(t *testing.T) {
	t.Parallel()

	branch := []session.Entry{
		userText("u1", "Read me a short sci-fi story"),
		assistText("a1", strings.Repeat("Mira ran her hand along the spines. ", 100)),
	}
	run := func(_ context.Context, _ llm.Request) (string, error) {
		return "## Goal\nplaceholder summary body that is not tiny", nil
	}
	// a small window so the automatic path reaches stage 4; the one large reply
	// alone exceeds the target, so the natural cut keeps it and gains nothing.
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 900, MaxOutput: 1000}

	res, err := Compact(t.Context(), branch, model, run, Options{})
	require.NoError(t, err)
	assert.Nil(t, res) // a cut that keeps nearly everything must never be recorded
}

func TestCompactStagesMeetTargetNoSummary(t *testing.T) {
	t.Parallel()

	// a huge failed result far from the current turn is stubbed by stage 1 alone;
	// with a generous target no summariser call is needed.
	big := strings.Repeat("e", 64<<10)
	branch := []session.Entry{
		userText("u1", "q1"),
		callMsg("a1", "c1", "bash", ""),
		resultMsg("r1", "c1", big, true),
		userText("u2", "q2"),
		userText("u3", "q3"),
		userText("u4", "q4"),
	}
	var called bool
	run := func(_ context.Context, _ llm.Request) (string, error) {
		called = true
		return "", nil
	}
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 1000}

	res, err := Compact(t.Context(), branch, model, run, Options{})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, called) // stages 1-3 met the target without a summary
	assert.Empty(t, res.Summary)
	assert.Less(t, res.After, res.Before)
	assert.Equal(t, 1, res.Reduce.Stats.Failed)
}

func TestCompactNoChangeReturnsNil(t *testing.T) {
	t.Parallel()

	branch := []session.Entry{userText("u1", "hi"), assistText("a1", "yo")}
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 1000}
	res, err := Compact(t.Context(), branch, model, nil, Options{})
	require.NoError(t, err)
	assert.Nil(t, res) // nothing to reduce and no summariser wired
}

func TestCompactNoDuplicateStubs(t *testing.T) {
	t.Parallel()

	// a failed result (stage 1) and a large old result (stage 2) must each be
	// stubbed exactly once, never twice.
	big := strings.Repeat("y", 16<<10)
	branch := []session.Entry{
		userText("u1", "q1"),
		callMsg("a1", "c1", "bash", ""),
		resultMsg("r1", "c1", "failed output", true), // stage 1: failed
		callMsg("a2", "c2", "bash", ""),
		resultMsg("r2", "c2", big, false), // stage 2: large old result
		userText("u2", "q2"),
		userText("u3", "q3"),
		userText("u4", "q4"),
	}
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 1000}
	res, err := Compact(t.Context(), branch, model, nil, Options{})
	require.NoError(t, err)
	require.NotNil(t, res)

	seen := make(map[string]int)
	for _, s := range res.Reduce.Stubs {
		seen[s.CallID]++
	}
	for id, n := range seen {
		assert.Equal(t, 1, n, "call id %s stubbed more than once", id) // message carries the offending id
	}
}

// The before/after measure must apply request-build retention: thinking a
// non-RetainAll policy drops from completed turns is not context compaction saves,
// so it never inflates Before.
func TestCompactMeasuresRequestRetention(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("ponder ", 4000)
	branch := []session.Entry{
		userText("u1", "q1"),
		callMsg("a1", "c1", "bash", ""),
		resultMsg("r1", "c1", strings.Repeat("detail ", 100), true), // old failure: stage-1 stub
		msg("t1", llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{llm.ThinkingBlock{Text: big}}}),
		userText("u2", "q2"),
		userText("u3", "q3"), // r1 is older than the last two turns
	}
	model := llm.Model{
		Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 1000,
		Caps: llm.Capabilities{Reasoning: true},
	}

	t.Run("whole_turn_drops_old_thinking_before_measure", func(t *testing.T) {
		res, err := Compact(t.Context(), branch, model, nil, Options{Retain: llm.RetainWholeTurn})
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Less(t, res.Before, 2000) // completed-turn thinking is never counted
	})

	t.Run("retain_all_counts_thinking_before_measure", func(t *testing.T) {
		res, err := Compact(t.Context(), branch, model, nil, Options{Retain: llm.RetainAll})
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Greater(t, res.Before, 2000) // with RetainAll the thinking is real context
	})
}
