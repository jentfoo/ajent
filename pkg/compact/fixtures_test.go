package compact

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixtureCompactModel is small enough that the tool-heavy fixture is worth
// compacting without the test having to fabricate an enormous branch.
func fixtureCompactModel() llm.Model {
	return llm.Model{Provider: "anthropic", ID: "claude-opus-4-5",
		ContextWindow: 8192, MaxOutput: 1024, Caps: llm.Capabilities{Reasoning: true}}
}

// TestFixtureCompact runs the staged reduction over the committed tool-heavy
// branch. The measurement is the point: Before and After are taken through the
// same session.ContextMessages the next request is built from, so a saving that
// measures is a saving the request actually gets.
func TestFixtureCompact(t *testing.T) {
	t.Parallel()

	model := fixtureCompactModel()

	t.Run("structural_stage_finds_work", func(t *testing.T) {
		branch := loadFixtureBranch(t, "tools.jsonl")

		stubs, _, stats := structural(branch, "/w")
		assert.Equal(t, 1, stats.Failed, "the errored bash result")
		assert.Equal(t, 2, stats.Superseded, "the re-read file and the overwritten edit")

		// each rule names itself in the stub, so one breaking cannot hide behind
		// another that happens to keep the count up
		texts := make([]string, len(stubs))
		for i, st := range stubs {
			texts[i] = st.Text
		}
		assert.Contains(t, texts, "[read superseded by a later read of the same file]")
		assert.Contains(t, texts, "[edits superseded by a later write of the same file]")
		assert.True(t, slices.ContainsFunc(texts, func(s string) bool {
			return strings.Contains(s, "exit 1: undefined: Foo")
		}), "the failed stub keeps its first line")
	})

	t.Run("plan_replays_to_the_measured_size", func(t *testing.T) {
		branch := loadFixtureBranch(t, "tools.jsonl")

		opts := Options{Cwd: "/w", Force: true, Retain: llm.RetainAll, Caps: model.Caps}
		res, err := Compact(t.Context(), branch, model, nil, opts)
		require.NoError(t, err)
		require.NotNil(t, res, "the fixture has measurable slack")
		assert.Less(t, res.After, res.Before, "compaction has to shrink the context")

		// the recorded plan, replayed through assembly, must measure what was promised
		cd := session.CompactionData{
			Summary: res.Summary, FirstKeptEntryID: res.FirstKeptEntryID, Reduce: &res.Reduce,
		}
		assert.Equal(t, res.After, tokensFor(branch, cd, model, opts.Retain, 0),
			"a measured saving is the saving the next request gets")
	})

	t.Run("summary_folds_the_older_turns", func(t *testing.T) {
		branch := loadFixtureBranch(t, "tools.jsonl")

		var prompted bool
		run := func(_ context.Context, _ llm.Request) (string, error) {
			prompted = true
			return "## Goal\ntidy the parser", nil
		}
		res, err := Compact(t.Context(), branch, model, run, Options{Cwd: "/w", Force: true})
		require.NoError(t, err)
		require.NotNil(t, res)
		require.True(t, prompted, "stage four ran")

		msgs := mustMsgs(t, branch, session.CompactionData{
			Summary: res.Summary, FirstKeptEntryID: res.FirstKeptEntryID, Reduce: &res.Reduce,
		})
		require.NotEmpty(t, msgs)
		assert.Contains(t, textOf(msgs[0]), "tidy the parser", "the summary leads the rebuilt context")
		assert.Less(t, len(msgs), 33, "the branch assembles to fewer messages than uncompacted")
	})

	t.Run("recompaction_is_cumulative", func(t *testing.T) {
		branch := loadFixtureBranch(t, "tools.jsonl")
		opts := Options{Cwd: "/w", Force: true, Retain: llm.RetainAll, Caps: model.Caps}

		first, err := Compact(t.Context(), branch, model, nil, opts)
		require.NoError(t, err)
		require.NotNil(t, first)

		// record the plan the way the driver does, then compact the branch again
		data, err := json.Marshal(session.CompactionData{
			Summary: first.Summary, FirstKeptEntryID: first.FirstKeptEntryID, Reduce: &first.Reduce,
		})
		require.NoError(t, err)
		withPlan := append(slices.Clone(branch), session.Entry{
			ID: "compaction-1", ParentID: branch[len(branch)-1].ID,
			Type: session.TypeCompaction, Data: data,
		})

		second, err := Compact(t.Context(), withPlan, model, nil, opts)
		require.NoError(t, err)
		require.NotNil(t, second, "the recorded plan still leaves measurable slack")

		// only the newest plan applies, so the second run measures the whole
		// branch again rather than stacking onto the first
		assert.Less(t, second.After, second.Before)
		assert.LessOrEqual(t, second.After, first.After+1)
	})
}
