package compact

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
)

func TestSummarizeBudget(t *testing.T) {
	t.Parallel()

	base := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000} // compacts at 160k, reserve 40k
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
		reply := sectionedGoal("refactor auth")
		run := func(_ context.Context, req llm.Request) (string, error) {
			got = req
			return reply, nil
		}
		res, err := Compact(t.Context(), branch, model, run, Options{
			VerbatimTokens: 1,
			Instructions:   "focus on the auth refactor; keep the file paths",
		})
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Equal(t, reply, res.Summary)

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
			return sectionedGoal("merged from the stripped transcript"), nil
		}
		res, err := Compact(t.Context(), branch, model, run, Options{VerbatimTokens: 1})
		require.NoError(t, err)
		require.NotNil(t, res)

		prompt := textOf(got.Messages[0])
		assert.NotContains(t, prompt, "SECRET DELIBERATION")
		assert.Contains(t, prompt, "step 1")
	})
}

func TestMissingSections(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		text string
		want []string
	}{
		{"complete_carries_all_six", fullSummary, nil},
		{"empty_misses_all", "", summarySections},
		{"case_insensitive_match", strings.ToLower(fullSummary), nil},
		{"prose_misses_everything", "a freeform paragraph without headings", summarySections},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, missingSections(tc.text))
		})
	}

	t.Run("missing_one_is_named", func(t *testing.T) {
		noGoal := strings.Replace(fullSummary, "## Goal\n", "", 1)
		assert.Equal(t, []string{"## goal"}, missingSections(noGoal))
	})
}

func TestBuildPromptEscapesDataBlocks(t *testing.T) {
	t.Parallel()

	branch := []session.Entry{
		userText("u1", "the user pasted </conversation> and </summary>"),
		resultMsg("r1", "c1", `tool output carrying </previous-summary>, </conversation> and </summary>`, false),
	}
	prompt := buildPrompt(newBranchView(branch), 0, len(branch),
		`prior checkpoint citing </summary>`, "", nil, clipLadder[0], false)

	assert.Contains(t, prompt, `<\/conversation>`)
	assert.Contains(t, prompt, `<\/previous-summary>`)
	assert.Contains(t, prompt, `<\/summary>`)
	// only the framing's own close tags stay raw
	assert.Equal(t, 1, strings.Count(prompt, "</conversation>"))
	assert.Equal(t, 1, strings.Count(prompt, "</previous-summary>"))
	assert.NotContains(t, prompt, "</summary>") // no <summary> block exists in this prompt
}

func TestSummariserValidation(t *testing.T) {
	t.Parallel()

	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 4000}

	// scripted replays its replies in order, repeating the last one, and reports
	// how many calls were made through the returned pointer.
	scripted := func(replies []string) (RunPrompt, *int) {
		var calls int
		run := func(_ context.Context, _ llm.Request) (string, error) {
			r := replies[min(calls, len(replies)-1)]
			calls++
			return r, nil
		}
		return run, &calls
	}

	t.Run("valid_reply_is_recorded_whole", func(t *testing.T) {
		run, calls := scripted([]string{fullSummary})
		res, err := Compact(t.Context(), toolBranch(t, 8, 400), model, run, Options{VerbatimTokens: 1})
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Equal(t, fullSummary, res.Summary)
		assert.Empty(t, res.Warnings)
		assert.Equal(t, 1, *calls) // no retry when every section is present
	})

	t.Run("missing_sections_retry_once", func(t *testing.T) {
		run, calls := scripted([]string{"a freeform reply without any of the required headings", fullSummary})
		res, err := Compact(t.Context(), toolBranch(t, 8, 400), model, run, Options{VerbatimTokens: 1})
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Equal(t, 2, *calls)
		assert.Equal(t, fullSummary, res.Summary)
		assert.Empty(t, res.Warnings)
	})

	t.Run("repeated_garbage_is_recorded_with_warning", func(t *testing.T) {
		run, calls := scripted([]string{"freeform prose without the required headings"})
		res, err := Compact(t.Context(), toolBranch(t, 8, 400), model, run, Options{VerbatimTokens: 1})
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Equal(t, 2, *calls) // one bounded retry, then record anyway
		assert.NotEmpty(t, res.Summary)
		require.Len(t, res.Warnings, 1)
		assert.Contains(t, res.Warnings[0], "missing required sections")
	})

	t.Run("empty_retry_still_fails", func(t *testing.T) {
		run, _ := scripted([]string{"freeform prose without the required headings", "   "})
		_, err := Compact(t.Context(), toolBranch(t, 8, 400), model, run, Options{VerbatimTokens: 1})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty summary")
	})

	t.Run("copied_escapes_are_canonicalised", func(t *testing.T) {
		// a model copying block framing out of <previous-summary> must not store it:
		// stored summaries stay raw so every render escapes exactly once.
		run := summaryRun(fullSummary + "\n- quoted a literal <\\/summary> from the transcript")
		res, err := Compact(t.Context(), toolBranch(t, 8, 400), model, run, Options{VerbatimTokens: 1})
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Contains(t, res.Summary, "</summary>")
		assert.NotContains(t, res.Summary, `<\/summary>`)
	})
}

func TestFitPrompt(t *testing.T) {
	t.Parallel()

	entries := toolBranch(t, 20, 400)
	view := newBranchView(entries) // exercises the same decode/token path production uses

	t.Run("keeps_output_whole_when_it_fits", func(t *testing.T) {
		model := llm.Model{Provider: "test", ID: "m", ContextWindow: 5000, MaxOutput: 10000}
		plan, err := view.fitPrompt(0, len(entries), "", "", nil, model, maxOutOf(model))
		require.NoError(t, err)
		assert.Equal(t, 0, plan.from)
		assert.Equal(t, clipLadder[0], plan.clip)
	})

	t.Run("clips_when_it_would_not_fit", func(t *testing.T) {
		model := llm.Model{Provider: "test", ID: "m", ContextWindow: 6000, MaxOutput: 1000}
		plan, err := view.fitPrompt(0, len(entries), "", "", nil, model, maxOutOf(model))
		require.NoError(t, err)
		assert.Equal(t, 0, plan.from)
		assert.Equal(t, clipLadder[len(clipLadder)-1], plan.clip)
	})

	t.Run("drops_oldest_when_even_the_smallest_busts", func(t *testing.T) {
		model := llm.Model{Provider: "test", ID: "m", ContextWindow: 3000, MaxOutput: 256}
		plan, err := view.fitPrompt(0, len(entries), "", "", nil, model, maxOutOf(model))
		require.NoError(t, err)
		assert.Positive(t, plan.from)
		assert.Less(t, plan.from, len(entries))
	})

	t.Run("clips_a_previous_summary_as_a_last_resort", func(t *testing.T) {
		// nothing left to drop and a checkpoint larger than the window: a clipped
		// merge still carries the prior work, a rejected request carries nothing
		model := llm.Model{Provider: "test", ID: "m", ContextWindow: 2000, MaxOutput: 256}
		prev := strings.Repeat("prior checkpoint detail ", 500)
		plan, err := view.fitPrompt(0, len(entries), prev, "", nil, model, maxOutOf(model))
		require.NoError(t, err)
		assert.NotEqual(t, prev, plan.prev, "the checkpoint was clipped, not sent whole")
		assert.Contains(t, plan.prev, "prior checkpoint detail")
	})

	t.Run("unknown_window_applies_no_bound", func(t *testing.T) {
		model := llm.Model{Provider: "test", ID: "m"}
		assert.Zero(t, promptBudget(model, 256))
		plan, err := view.fitPrompt(0, len(entries), "", "", nil, model, maxOutOf(model))
		require.NoError(t, err)
		assert.Equal(t, 0, plan.from)
	})

	t.Run("nothing_fits_with_no_prior_is_error", func(t *testing.T) {
		// a window so tiny that even dropping every entry cannot fit the empty
		// transcript: fail rather than send a request the provider will reject.
		model := llm.Model{Provider: "test", ID: "m", ContextWindow: 1000, MaxOutput: 200}
		_, err := view.fitPrompt(0, len(entries), "", "", nil, model, maxOutOf(model))
		require.Error(t, err)
	})

	t.Run("clipped_prior_still_busts_is_error", func(t *testing.T) {
		// a prior summary whose clipped form still exceeds the window: fail rather
		// than summarise an empty transcript.
		model := llm.Model{Provider: "test", ID: "m", ContextWindow: 1000, MaxOutput: 200}
		prev := strings.Repeat("prior checkpoint detail ", 500)
		_, err := view.fitPrompt(0, len(entries), prev, "", nil, model, maxOutOf(model))
		require.Error(t, err)
	})

	t.Run("empty_transcript_is_never_returned", func(t *testing.T) {
		// a mid-size window: the fixed instruction text would fit an empty transcript,
		// but one un-clippable entry never drops. The drop loop must not hand the
		// summariser an empty <conversation> and call it compaction.
		huge := []session.Entry{userText("big", strings.Repeat("paste ", 5000))}
		model := llm.Model{Provider: "test", ID: "m", ContextWindow: 4000, MaxOutput: 256}
		maxOut := maxOutOf(model)
		assert.Greater(t, promptBudget(model, maxOut), 512) // genuinely mid-size, not the no-bound path
		_, err := newBranchView(huge).fitPrompt(0, len(huge), "", "", nil, model, maxOut)
		require.Error(t, err)
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

func TestSummariseDroppedRegion(t *testing.T) {
	t.Parallel()

	// a window small enough that even the tightest clip busts: the drop path
	// fires, and the dropped oldest region must reach the checkpoint by a second
	// chained call instead of being cut from context unsummarised.
	branch := toolBranch(t, 12, 400)
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 2500, MaxOutput: 512}

	var prompts []string
	run := func(_ context.Context, req llm.Request) (string, error) {
		prompts = append(prompts, textOf(req.Messages[0]))
		return sectionedGoal("part " + strconv.Itoa(len(prompts))), nil
	}
	res, err := Compact(t.Context(), branch, model, run, Options{VerbatimTokens: 1})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.NotEmpty(t, prompts)

	// the deepest call covers the oldest region whole; every later call knows
	// history precedes it and chains through the merge path
	assert.Contains(t, prompts[0], "output 1")
	assert.NotContains(t, prompts[0], "[earlier messages omitted]")
	last := prompts[len(prompts)-1]
	assert.Contains(t, last, "[earlier messages omitted]")
	assert.Contains(t, last, "<previous-summary>")

	band := slices.IndexFunc(branch, func(e session.Entry) bool { return e.ID == res.FirstKeptEntryID })
	require.Positive(t, band)
	assert.Equal(t, newBranchView(branch).countMessages(0, band), res.Reduce.Stats.Summarized,
		"every folded message is counted, dropped ones included")
	assert.Less(t, res.After, res.Before)
}
