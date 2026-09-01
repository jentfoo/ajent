package compact

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
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

// TestFixtureCompact runs the reduction over the committed tool-heavy branch. The
// measurement is the point: Before and After are taken through the same
// session.ContextMessages the next request is built from, so a saving that
// measures is a saving the request actually gets.
func TestFixtureCompact(t *testing.T) {
	t.Parallel()

	model := fixtureCompactModel()

	t.Run("span_stubs_find_the_redundancy", func(t *testing.T) {
		branch := loadFixtureBranch(t, "tools.jsonl")

		stubs := spanStubs(branch, 0, len(branch), "/w")
		texts := stubTexts(stubs)

		// each rule names itself, so one breaking cannot hide behind another that
		// happens to keep the count up
		assert.Contains(t, texts, supersededReadMarker)
		assert.Contains(t, texts, supersededEditMarker)
		assert.True(t, slices.ContainsFunc(texts, func(s string) bool {
			return strings.Contains(s, "exit 1: undefined: Foo")
		}))

		// the fixture closes with seven byte-identical grep results
		var dups int
		for _, txt := range texts {
			if txt == dupMarker {
				dups++
			}
		}
		assert.Equal(t, 6, dups)
	})

	t.Run("plan_replays_to_the_measured_size", func(t *testing.T) {
		branch := loadFixtureBranch(t, "tools.jsonl")

		opts := Options{Cwd: "/w", Retain: llm.RetainAll}
		res, err := Compact(t.Context(), branch, model, summaryRun("## Goal\ntidy the parser"), opts)
		require.NoError(t, err)
		require.NotNil(t, res, "the fixture has measurable slack")
		assert.Less(t, res.After, res.Before)

		// the recorded plan, replayed through assembly, must measure what was promised
		cd := session.CompactionData{
			Summary: res.Summary, FirstKeptEntryID: res.FirstKeptEntryID, Reduce: &res.Reduce,
		}
		assert.Equal(t, res.After, tokensFor(branch, cd, model, opts.Retain, 0))
	})

	t.Run("summary_leads_the_rebuilt_context", func(t *testing.T) {
		branch := loadFixtureBranch(t, "tools.jsonl")

		var prompted bool
		run := func(_ context.Context, _ llm.Request) (string, error) {
			prompted = true
			return "## Goal\ntidy the parser", nil
		}
		res, err := Compact(t.Context(), branch, model, run, Options{Cwd: "/w"})
		require.NoError(t, err)
		require.NotNil(t, res)
		require.True(t, prompted)

		msgs := mustMsgs(t, branch, session.CompactionData{
			Summary: res.Summary, FirstKeptEntryID: res.FirstKeptEntryID, Reduce: &res.Reduce,
		})
		require.NotEmpty(t, msgs)
		assert.Contains(t, textOf(msgs[0]), "tidy the parser")
		assert.Less(t, len(msgs), 33)
	})

	t.Run("recompaction_keeps_the_prior_cut", func(t *testing.T) {
		branch := loadFixtureBranch(t, "tools.jsonl")
		opts := Options{Cwd: "/w", Retain: llm.RetainAll}
		run := summaryRun("## Goal\ntidy the parser")

		first, err := Compact(t.Context(), branch, model, run, opts)
		require.NoError(t, err)
		require.NotNil(t, first)
		require.NotEmpty(t, first.Summary)

		// record the plan the way the driver does, then compact the branch again
		data, err := json.Marshal(session.CompactionData{
			Summary: first.Summary, FirstKeptEntryID: first.FirstKeptEntryID, Reduce: &first.Reduce,
		})
		require.NoError(t, err)
		withPlan := append(slices.Clone(branch), session.Entry{
			ID: "compaction-1", ParentID: branch[len(branch)-1].ID,
			Type: session.TypeCompaction, Data: data,
		})

		// nothing new happened, so a second pass must decline rather than record a
		// plan that would reopen what the first one folded
		second, err := Compact(t.Context(), withPlan, model, run, opts)
		require.NoError(t, err)
		assert.Nil(t, second)
	})
}

// TestFixtureRecompaction compacts a frozen transcript that already carries a cut,
// which is the case that used to silently reopen the folded history.
func TestFixtureRecompaction(t *testing.T) {
	t.Parallel()

	model := fixtureCompactModel()
	branch := loadFixtureBranch(t, "compacted.jsonl")

	prior, _, found := session.NewestCompaction(branch)
	require.True(t, found)
	require.NotEmpty(t, prior.FirstKeptEntryID)
	priorCut := session.CutIndex(branch, prior)
	require.GreaterOrEqual(t, priorCut, 0)

	// new work recorded after the checkpoint, big enough to be worth folding
	for i := 1; i <= 6; i++ {
		n := strconv.Itoa(i)
		branch = append(branch,
			callMsg("na"+n, "nc"+n, "bash", ""),
			resultMsg("nr"+n, "nc"+n, strings.Repeat("fresh output ", 300), false))
	}

	var got llm.Request
	run := func(_ context.Context, req llm.Request) (string, error) {
		got = req
		return "## Goal\nmerged checkpoint", nil
	}
	opts := Options{Cwd: "/w", Retain: llm.RetainAll}

	res, err := Compact(t.Context(), branch, model, run, opts)
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.Equal(t, tokensFor(branch, prior, model, opts.Retain, 0), res.Before)
	assert.Less(t, res.Before, tokensFor(branch, session.CompactionData{}, model, opts.Retain, 0))

	newCut := session.CutIndex(branch, session.CompactionData{
		Summary: res.Summary, FirstKeptEntryID: res.FirstKeptEntryID,
	})
	assert.GreaterOrEqual(t, newCut, priorCut)

	require.Len(t, got.Messages, 1)
	assert.Contains(t, textOf(got.Messages[0]), "<previous-summary>")
	assert.Contains(t, textOf(got.Messages[0]), prior.Summary)

	cd := session.CompactionData{
		Summary: res.Summary, FirstKeptEntryID: res.FirstKeptEntryID, Reduce: &res.Reduce,
	}
	assert.Equal(t, res.After, tokensFor(branch, cd, model, opts.Retain, 0))
	assert.Less(t, res.After, res.Before)
}
