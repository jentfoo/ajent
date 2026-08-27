package compact

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
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

// toolBranch builds n steps, each an assistant tool call plus its result, opened
// by one real user prompt. Ids are "u1", then "aN"/"rN" per step.
func toolBranch(t *testing.T, n, filler int) []session.Entry {
	t.Helper()

	branch := []session.Entry{userText("u1", "do the work")}
	for i := 1; i <= n; i++ {
		s := strconv.Itoa(i)
		branch = append(branch,
			callMsg("a"+s, "c"+s, "bash", ""),
			resultMsg("r"+s, "c"+s, "output "+s+" "+strings.Repeat("x ", filler), false))
	}
	return branch
}

func summaryRun(text string) RunPrompt {
	return func(_ context.Context, _ llm.Request) (string, error) { return text, nil }
}

func TestCompact(t *testing.T) {
	t.Parallel()

	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 4000}

	t.Run("folds_everything_before_the_band", func(t *testing.T) {
		branch := toolBranch(t, 8, 400)

		res, err := Compact(t.Context(), branch, model, summaryRun("## Goal\nthe work"), Options{VerbatimTokens: 1})
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Equal(t, "a7", res.FirstKeptEntryID, "the last two steps stay verbatim")
		assert.Less(t, res.After, res.Before)

		msgs := mustMsgs(t, branch, session.CompactionData{
			Summary: res.Summary, FirstKeptEntryID: res.FirstKeptEntryID, Reduce: &res.Reduce,
		})
		require.Len(t, msgs, 5) // summary + two call/result pairs
		assert.Contains(t, textOf(msgs[0]), "the work")
	})

	t.Run("no_summariser_returns_nil", func(t *testing.T) {
		branch := toolBranch(t, 8, 400)

		res, err := Compact(t.Context(), branch, model, nil, Options{})
		require.NoError(t, err)
		assert.Nil(t, res)
	})

	t.Run("declines_when_the_band_is_everything", func(t *testing.T) {
		branch := toolBranch(t, 2, 20)

		res, err := Compact(t.Context(), branch, model, summaryRun("## Goal\nx"), Options{})
		require.NoError(t, err)
		assert.Nil(t, res)
	})

	t.Run("never_records_growth", func(t *testing.T) {
		branch := toolBranch(t, 4, 400)
		huge := strings.Repeat("summary prose ", 4000)

		res, err := Compact(t.Context(), branch, model, summaryRun(huge), Options{VerbatimTokens: 1})
		require.NoError(t, err)
		assert.Nil(t, res, "a summary bigger than the span it replaces is not a saving")
	})

	t.Run("blank_summary_is_error", func(t *testing.T) {
		branch := toolBranch(t, 8, 400)

		_, err := Compact(t.Context(), branch, model, summaryRun("   "), Options{VerbatimTokens: 1})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty summary")
	})

	t.Run("plan_is_inert", func(t *testing.T) {
		branch := toolBranch(t, 8, 400)

		res, err := Compact(t.Context(), branch, model, summaryRun("## Goal\nx"), Options{VerbatimTokens: 1})
		require.NoError(t, err)
		require.NotNil(t, res)

		// the cut already removes everything a stub could touch, so recording one
		// would be dead weight that a later rebuild has to reason about
		assert.Empty(t, res.Reduce.Stubs)
		assert.Empty(t, res.Reduce.Drop)
		assert.False(t, res.Reduce.StripThinking)
		assert.Positive(t, res.Reduce.Stats.Summarized)
	})
}

func TestCompactVerbatimBand(t *testing.T) {
	t.Parallel()

	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 4000}
	run := summaryRun("## Goal\nwork")

	t.Run("floor_only_keeps_two_steps", func(t *testing.T) {
		branch := toolBranch(t, 10, 400)

		res, err := Compact(t.Context(), branch, model, run, Options{VerbatimTokens: 1})
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Equal(t, "a9", res.FirstKeptEntryID)
	})

	t.Run("ceiling_extends_the_band", func(t *testing.T) {
		branch := toolBranch(t, 10, 400)
		one := spanTokens(branch, 19, len(branch))

		res, err := Compact(t.Context(), branch, model, run, Options{VerbatimTokens: one * 4})
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Equal(t, "a7", res.FirstKeptEntryID, "four steps fit the ceiling")
	})

	t.Run("oversized_floor_is_kept_verbatim", func(t *testing.T) {
		branch := toolBranch(t, 6, 500)

		res, err := Compact(t.Context(), branch, model, run, Options{VerbatimTokens: 1})
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Equal(t, "a5", res.FirstKeptEntryID, "the floor outranks the ceiling")
	})

	t.Run("options_override_the_defaults", func(t *testing.T) {
		branch := toolBranch(t, 10, 400)

		res, err := Compact(t.Context(), branch, model, run, Options{MinSteps: 1, VerbatimTokens: 1})
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Equal(t, "a10", res.FirstKeptEntryID)
	})

	t.Run("band_results_are_never_reduced", func(t *testing.T) {
		// the same output twice, the duplicate landing inside the band
		body := strings.Repeat("same bytes ", 400)
		branch := []session.Entry{userText("u1", "read it twice")}
		for i := 1; i <= 6; i++ {
			s := strconv.Itoa(i)
			branch = append(branch,
				callMsg("a"+s, "c"+s, "read", "/w/f.go"),
				resultMsg("r"+s, "c"+s, body, false))
		}

		res, err := Compact(t.Context(), branch, model, run, Options{Cwd: "/w", VerbatimTokens: 1})
		require.NoError(t, err)
		require.NotNil(t, res)

		msgs := mustMsgs(t, branch, session.CompactionData{
			Summary: res.Summary, FirstKeptEntryID: res.FirstKeptEntryID, Reduce: &res.Reduce,
		})
		for _, m := range msgs[1:] { // msgs[0] is the summary
			for _, b := range m.Content {
				if tr, ok := b.(llm.ToolResultBlock); ok {
					assert.Equal(t, body, resultText(tr), "a band result was stubbed")
				}
			}
		}
	})
}

func TestCompactMeasuresRequestRetention(t *testing.T) {
	t.Parallel()

	branch := []session.Entry{userText("u1", "start")}
	for i := 1; i <= 6; i++ {
		s := strconv.Itoa(i)
		branch = append(branch, msg("a"+s, llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{
			llm.ThinkingBlock{Text: strings.Repeat("deliberating ", 300)},
			llm.TextBlock{Text: "step " + s},
		}}))
	}
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 4000,
		Caps: llm.Capabilities{Reasoning: true}}
	run := summaryRun("## Goal\nx")

	measure := func(t *testing.T, retain llm.RetainPolicy) int {
		t.Helper()
		res, err := Compact(t.Context(), branch, model, run,
			Options{Retain: retain, VerbatimTokens: 1})
		require.NoError(t, err)
		require.NotNil(t, res)
		return res.Before
	}

	t.Run("retain_none_drops_thinking_before_measure", func(t *testing.T) {
		assert.Less(t, measure(t, llm.RetainNone), 2000)
	})

	t.Run("retain_all_counts_thinking_before_measure", func(t *testing.T) {
		assert.Greater(t, measure(t, llm.RetainAll), 2000)
	})
}

// TestCompactRecompaction covers the second and later compactions of a session.
// Every plan must measure against the context a prior compaction actually left and
// carry its checkpoint forward; a plan that forgot either resurrected the whole
// folded history on the next rebuild.
func TestCompactRecompaction(t *testing.T) {
	t.Parallel()

	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 4000}
	run := summaryRun("## Goal\nfresh")

	// priorBranch is eight folded steps, a compaction keeping from "a7", then ten
	// new steps recorded after it.
	priorBranch := func(t *testing.T) []session.Entry {
		t.Helper()
		branch := toolBranch(t, 8, 400)
		branch = append(branch, compactEntry("comp", "an earlier checkpoint", "a7"))
		for i := 1; i <= 10; i++ {
			s := strconv.Itoa(i)
			branch = append(branch,
				callMsg("na"+s, "nc"+s, "bash", ""),
				resultMsg("nr"+s, "nc"+s, "new output "+strings.Repeat("y ", 400), false))
		}
		return branch
	}

	t.Run("measures_against_effective_context", func(t *testing.T) {
		branch := priorBranch(t)
		prior := session.CompactionData{Summary: "an earlier checkpoint", FirstKeptEntryID: "a7"}

		res, err := Compact(t.Context(), branch, model, run, Options{VerbatimTokens: 1})
		require.NoError(t, err)
		require.NotNil(t, res)

		effective := tokensFor(branch, prior, model, llm.RetainNone, 0)
		raw := tokensFor(branch, session.CompactionData{}, model, llm.RetainNone, 0)
		assert.Equal(t, effective, res.Before)
		assert.Less(t, res.Before, raw, "the raw branch is not what the next request sends")
	})

	t.Run("never_reopens_a_prior_cut", func(t *testing.T) {
		branch := priorBranch(t)
		priorCut := session.CutIndex(branch, session.CompactionData{Summary: "x", FirstKeptEntryID: "a7"})

		res, err := Compact(t.Context(), branch, model, run, Options{VerbatimTokens: 1})
		require.NoError(t, err)
		require.NotNil(t, res)
		cut := session.CutIndex(branch, session.CompactionData{
			Summary: res.Summary, FirstKeptEntryID: res.FirstKeptEntryID,
		})
		assert.GreaterOrEqual(t, cut, priorCut)
		assert.NotEmpty(t, res.Summary)
	})

	t.Run("declines_when_nothing_new_happened", func(t *testing.T) {
		branch := toolBranch(t, 8, 400)
		branch = append(branch, compactEntry("comp", "a checkpoint", "a7"))

		res, err := Compact(t.Context(), branch, model, run, Options{VerbatimTokens: 1})
		require.NoError(t, err)
		assert.Nil(t, res)
	})

	t.Run("budget_carries_the_prior_checkpoint", func(t *testing.T) {
		// a recompaction model with enough headroom that the emit cap does not bind
		big := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 10000}
		branch := priorBranch(t)

		var got llm.Request
		run := func(_ context.Context, req llm.Request) (string, error) {
			got = req
			return "## Goal\nfresh", nil
		}
		res, err := Compact(t.Context(), branch, big, run, Options{VerbatimTokens: 1})
		require.NoError(t, err)
		require.NotNil(t, res)
		// the prior summary must survive recompaction whole, never amputated by a
		// budget sized from the short new span alone.
		assert.GreaterOrEqual(t, got.MaxTokens, minSummaryTokens)
	})

	t.Run("normalises_summary_only_prior", func(t *testing.T) {
		branch := []session.Entry{
			userText("u0", "folded away"),
			compactEntry("comp", "a summary-only checkpoint", ""),
		}
		for i := 1; i <= 6; i++ {
			s := strconv.Itoa(i)
			branch = append(branch,
				callMsg("a"+s, "c"+s, "bash", ""),
				resultMsg("r"+s, "c"+s, "output "+strings.Repeat("z ", 400), false))
		}

		res, err := Compact(t.Context(), branch, model, run, Options{VerbatimTokens: 1})
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Equal(t, "a5", res.FirstKeptEntryID)

		// replay the way the driver does: the new entry becomes the newest, and the
		// messages recorded between the two compactions must survive it
		cd := session.CompactionData{
			Summary: res.Summary, FirstKeptEntryID: res.FirstKeptEntryID, Reduce: &res.Reduce,
		}
		withNew := append(slices.Clone(branch), compactEntry("comp2", cd.Summary, cd.FirstKeptEntryID))
		msgs := mustMsgs(t, withNew, cd)
		require.Len(t, msgs, 5) // summary + the last two call/result pairs
		assert.Contains(t, textOf(msgs[0]), "fresh")
	})
}

func TestResolveVerbatim(t *testing.T) {
	t.Parallel()

	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000} // compacts at 160k

	t.Run("defaults_to_two_steps_and_a_tenth", func(t *testing.T) {
		steps, tok := resolveVerbatim(model, Options{})
		assert.Equal(t, 2, steps)
		assert.Equal(t, 16000, tok)
	})

	t.Run("unknown_window_still_bounds", func(t *testing.T) {
		_, tok := resolveVerbatim(llm.Model{Provider: "test", ID: "m"}, Options{})
		assert.Equal(t, minVerbatimTokens, tok, "an unbounded band could never compact")
	})

	t.Run("options_win", func(t *testing.T) {
		steps, tok := resolveVerbatim(model, Options{MinSteps: 5, VerbatimTokens: 99})
		assert.Equal(t, 5, steps)
		assert.Equal(t, 99, tok)
	})

	t.Run("min_steps_clamped_to_eight", func(t *testing.T) {
		steps, _ := resolveVerbatim(model, Options{MinSteps: 100})
		assert.Equal(t, maxVerbatimSteps, steps, "a hand-edited minSteps cannot swallow the branch")
	})
}
