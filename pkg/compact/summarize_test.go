package compact

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The summariser budget must carry forward what a merged checkpoint replaces:
// at least minSummaryTokens, enough to hold any prior summary plus the new span,
// and capped by what the model can emit.
func TestSummarizeBudget(t *testing.T) {
	t.Parallel()

	base := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000} // compacts at 160k; reserve 40k
	cases := []struct {
		name string
		mod  llm.Model
		span int
		prev int
		want int
	}{
		{name: "small_span_no_prior_floors", mod: base, span: 600, prev: 0, want: minSummaryTokens},
		{"huge_span_quarter_of_compact_point", base, 1000000, 3000, 40000},
		{
			name: "max_output_caps_budget",
			mod:  llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 25000},
			span: 1000000,
			prev: 4000,
			want: 25000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, summarizeBudget(tc.mod, tc.span, tc.prev))
		})
	}

	// the amputation repro: a short re-accumulation span over a ~6k-token prior must
	// budget at least minSummaryTokens and hold everything it replaces (prior+span).
	t.Run("short_span_carries_the_prior_summary", func(t *testing.T) {
		got := summarizeBudget(base, 1500, 6000)
		assert.GreaterOrEqual(t, got, minSummaryTokens)
		assert.GreaterOrEqual(t, got, 7500) // prior+span
	})
}

func TestSummariserPrompt(t *testing.T) {
	t.Parallel()

	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 4000}

	t.Run("carries_instructions", func(t *testing.T) {
		branch := toolBranch(t, 8, 400)

		var got llm.Request
		run := func(_ context.Context, req llm.Request) (string, error) {
			got = req
			return "## Goal\nrefactor auth", nil
		}
		res, err := Compact(t.Context(), branch, model, run, Options{
			VerbatimTokens: 1,
			Instructions:   "focus on the auth refactor; keep the file paths",
		})
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Equal(t, "## Goal\nrefactor auth", res.Summary)

		require.Len(t, got.Messages, 1)
		prompt := textOf(got.Messages[0])
		assert.Contains(t, prompt, "<conversation>")
		assert.Contains(t, prompt, "## Goal")       // the six-section spec
		assert.Contains(t, prompt, "synopsis")      // produced-content detail rule
		assert.Contains(t, prompt, "kept verbatim") // the excluded-tail notice
		assert.Contains(t, prompt, "Additional focus: focus on the auth refactor")
	})

	t.Run("merges_previous_summary", func(t *testing.T) {
		steps := toolBranch(t, 8, 400)[1:]
		branch := make([]session.Entry, 0, len(steps)+2)
		branch = append(branch,
			userText("u0", "first ask"),
			compactEntry("comp", "an earlier summary", "a1"))
		branch = append(branch, steps...)

		var got llm.Request
		run := func(_ context.Context, req llm.Request) (string, error) {
			got = req
			return "merged", nil
		}
		res, err := Compact(t.Context(), branch, model, run, Options{VerbatimTokens: 1})
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Equal(t, "merged", res.Summary)

		prompt := textOf(got.Messages[0])
		assert.Contains(t, prompt, "<previous-summary>")
		assert.Contains(t, prompt, "an earlier summary")
		assert.Contains(t, prompt, "INTEGRATE the previous summary")
	})

	t.Run("strips_thinking_from_the_transcript", func(t *testing.T) {
		branch := make([]session.Entry, 0, 9)
		branch = append(branch, userText("u1", "start"))
		for i := 1; i <= 8; i++ {
			s := strconv.Itoa(i)
			branch = append(branch, msg("a"+s, llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{
				llm.ThinkingBlock{Text: "SECRET DELIBERATION " + s},
				llm.TextBlock{Text: "step " + s + " " + strings.Repeat("x ", 400)},
			}}))
		}

		var got llm.Request
		run := func(_ context.Context, req llm.Request) (string, error) {
			got = req
			return "## Goal\nx", nil
		}
		res, err := Compact(t.Context(), branch, model, run, Options{VerbatimTokens: 1})
		require.NoError(t, err)
		require.NotNil(t, res)

		prompt := textOf(got.Messages[0])
		assert.NotContains(t, prompt, "SECRET DELIBERATION")
		assert.Contains(t, prompt, "step 1")
	})
}

func TestFitPrompt(t *testing.T) {
	t.Parallel()

	entries := toolBranch(t, 20, 400)

	t.Run("keeps_output_whole_when_it_fits", func(t *testing.T) {
		model := llm.Model{Provider: "test", ID: "m", ContextWindow: 5000, MaxOutput: 10000}
		prompt, kept, err := fitPrompt(entries, "", "", nil, model, maxOutOf(model))
		require.NoError(t, err)
		assert.Contains(t, prompt, strings.Repeat("x ", 400), "no clip was needed")
		assert.Equal(t, countMessages(entries), kept)
	})

	t.Run("clips_when_it_would_not_fit", func(t *testing.T) {
		model := llm.Model{Provider: "test", ID: "m", ContextWindow: 6000, MaxOutput: 1000}
		prompt, _, err := fitPrompt(entries, "", "", nil, model, maxOutOf(model))
		require.NoError(t, err)
		assert.NotContains(t, prompt, strings.Repeat("x ", 400), "a clip cut the output")
		assert.LessOrEqual(t, tokens.EstimateText(prompt, tokens.KindCode),
			promptBudget(model, maxOutOf(model)))
	})

	t.Run("drops_oldest_when_even_the_smallest_busts", func(t *testing.T) {
		model := llm.Model{Provider: "test", ID: "m", ContextWindow: 3000, MaxOutput: 256}
		prompt, _, err := fitPrompt(entries, "", "", nil, model, maxOutOf(model))
		require.NoError(t, err)
		assert.Contains(t, prompt, "[earlier messages omitted]",
			"the transcript says so inside its own tags")
		assert.LessOrEqual(t, tokens.EstimateText(prompt, tokens.KindCode), promptBudget(model, maxOutOf(model)),
			"dropping entries has to actually reach the budget")
	})

	t.Run("clips_a_previous_summary_as_a_last_resort", func(t *testing.T) {
		// nothing left to drop and a checkpoint larger than the window: a clipped
		// merge still carries the prior work, a rejected request carries nothing
		model := llm.Model{Provider: "test", ID: "m", ContextWindow: 2000, MaxOutput: 256}
		prev := strings.Repeat("prior checkpoint detail ", 500)
		prompt, _, err := fitPrompt(entries, prev, "", nil, model, maxOutOf(model))
		require.NoError(t, err)
		assert.LessOrEqual(t, tokens.EstimateText(prompt, tokens.KindCode), promptBudget(model, maxOutOf(model)))
		assert.NotContains(t, prompt, prev, "the checkpoint was clipped, not sent whole")
		assert.Contains(t, prompt, "prior checkpoint detail", "but what fits still merges")
	})

	t.Run("unknown_window_applies_no_bound", func(t *testing.T) {
		model := llm.Model{Provider: "test", ID: "m"}
		assert.Zero(t, promptBudget(model, 256))
		prompt, _, err := fitPrompt(entries, "", "", nil, model, maxOutOf(model))
		require.NoError(t, err)
		assert.Contains(t, prompt, strings.Repeat("x ", 400))
	})

	t.Run("nothing_fits_with_no_prior_is_error", func(t *testing.T) {
		// a window so tiny that even dropping every entry cannot fit the empty
		// transcript: fail rather than send a request the provider will reject.
		model := llm.Model{Provider: "test", ID: "m", ContextWindow: 1000, MaxOutput: 200}
		_, _, err := fitPrompt(entries, "", "", nil, model, maxOutOf(model))
		require.Error(t, err)
	})

	t.Run("clipped_prior_still_busts_is_error", func(t *testing.T) {
		// a prior summary whose clipped form still exceeds the window: fail rather
		// than summarise an empty transcript.
		model := llm.Model{Provider: "test", ID: "m", ContextWindow: 1000, MaxOutput: 200}
		prev := strings.Repeat("prior checkpoint detail ", 500)
		_, _, err := fitPrompt(entries, prev, "", nil, model, maxOutOf(model))
		require.Error(t, err)
	})

	t.Run("kept_excludes_dropped_entries", func(t *testing.T) {
		// when the drop loop fires, kept counts only what survived into the prompt,
		// so Stats.Summarized stays honest.
		model := llm.Model{Provider: "test", ID: "m", ContextWindow: 3000, MaxOutput: 256}
		_, kept, err := fitPrompt(entries, "", "", nil, model, maxOutOf(model))
		require.NoError(t, err)
		assert.Less(t, kept, countMessages(entries), "the notice must not over-report")
	})

	t.Run("empty_transcript_is_never_returned", func(t *testing.T) {
		// a mid-size window: the fixed instruction text would fit an empty transcript,
		// but one un-clippable entry never drops. The drop loop must not hand the
		// summariser an empty <conversation> and call it compaction.
		huge := []session.Entry{userText("big", strings.Repeat("paste ", 5000))}
		model := llm.Model{Provider: "test", ID: "m", ContextWindow: 4000, MaxOutput: 256}
		maxOut := maxOutOf(model)
		assert.Greater(t, promptBudget(model, maxOut), 512) // genuinely mid-size, not the no-bound path
		_, kept, err := fitPrompt(huge, "", "", nil, model, maxOut)
		require.Error(t, err)
		assert.Zero(t, kept)
	})
}

// maxOutOf returns the reply allowance a caller would pass for model: its
// MaxOutput when set, else the reserve. Mirrors summarizeBudget's emitCap.
func maxOutOf(m llm.Model) int {
	if m.MaxOutput > 0 {
		return m.MaxOutput
	}
	return m.Reserve()
}
