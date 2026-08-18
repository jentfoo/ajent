package compact

import (
	"context"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The summariser budget must stay below the span it replaces so even a generous
// summary still shrinks the context.
func TestSummarizeBudget(t *testing.T) {
	t.Parallel()

	base := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000} // reserve 40k
	cases := []struct {
		name  string
		model llm.Model
		span  int
		want  int
	}{
		{name: "small_span_half_of_it", model: base, span: 600, want: 300},
		{name: "huge_span_eighty_percent_reserve", model: base, span: 1000000, want: 32000},
		{name: "trivial_span_floor", model: base, span: 8, want: 256},
		{
			name:  "max_output_caps_budget",
			model: llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 5000},
			span:  1000000,
			want:  5000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, summarizeBudget(tc.model, tc.span))
		})
	}
}

func TestSummariserPromptCarriesInstructions(t *testing.T) {
	t.Parallel()

	branch := []session.Entry{
		userText("u1", "refactor the auth package"),
		assistText("a1", strings.Repeat("working on auth ", 200)),
		userText("u2", "now update the tests"),
		assistText("a2", strings.Repeat("tests updated ", 80)),
	}
	var got llm.Request
	run := func(_ context.Context, req llm.Request) (string, error) {
		got = req
		return "## Goal\nrefactor auth", nil
	}
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 600, MaxOutput: 1000}

	res, err := Compact(t.Context(), branch, model, run, Options{
		Instructions: "focus on the auth refactor; keep the file paths",
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, "## Goal\nrefactor auth", res.Summary)
	assert.NotEmpty(t, res.FirstKeptEntryID)
	assert.Less(t, res.After, res.Before)

	require.Len(t, got.Messages, 1)
	prompt := textOf(got.Messages[0])
	assert.Contains(t, prompt, "<conversation>")
	assert.Contains(t, prompt, "## Goal")  // the six-section spec
	assert.Contains(t, prompt, "synopsis") // produced-content detail rule
	assert.Contains(t, prompt, "Additional focus: focus on the auth refactor")
}

func TestSummariserMergesPreviousSummary(t *testing.T) {
	t.Parallel()

	branch := []session.Entry{
		userText("u1", "first ask"),
		compactEntry("comp", "an earlier summary", "u2"),
		userText("u2", "second ask"),
		assistText("a2", strings.Repeat("more work ", 200)),
		userText("u3", "third ask"),
		assistText("a3", strings.Repeat("tail reply ", 120)),
	}
	var got llm.Request
	run := func(_ context.Context, req llm.Request) (string, error) {
		got = req
		return "merged", nil
	}
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 600, MaxOutput: 1000}

	res, err := Compact(t.Context(), branch, model, run, Options{})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Less(t, res.After, res.Before)

	prompt := textOf(got.Messages[0])
	assert.Contains(t, prompt, "<previous-summary>")
	assert.Contains(t, prompt, "an earlier summary")
	assert.Contains(t, prompt, "PRESERVE all existing information") // incremental rules
}
