package compact

import (
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// the verbatim band ----------------------------------------------------

// stepBranch builds n steps, each an assistant reply of roughly equal weight, and
// opens with one real user prompt. Steps are "a1".."aN", the prompt "u1".
func stepBranch(t *testing.T, n, filler int) []session.Entry {
	t.Helper()

	branch := []session.Entry{userText("u1", "do the work")}
	for i := 1; i <= n; i++ {
		branch = append(branch, assistText("a"+strconv.Itoa(i), strings.Repeat("x ", filler)))
	}
	return branch
}

func TestVerbatimCut(t *testing.T) {
	t.Parallel()

	t.Run("empty_branch_has_no_band", func(t *testing.T) {
		assert.Equal(t, 0, verbatimCut(nil, 0, 2, 1000))
	})

	t.Run("no_step_returns_the_end", func(t *testing.T) {
		branch := []session.Entry{userText("u1", "only a prompt")}
		assert.Equal(t, len(branch), verbatimCut(branch, 0, 2, 1000))
	})

	t.Run("fewer_steps_than_the_floor", func(t *testing.T) {
		branch := stepBranch(t, 1, 5)
		assert.Equal(t, 0, verbatimCut(branch, 0, 2, 0), "the prompt joins its only step")
	})

	t.Run("floor_keeps_exactly_two_steps", func(t *testing.T) {
		branch := stepBranch(t, 6, 5)
		assert.Equal(t, 5, verbatimCut(branch, 0, 2, 0), "no ceiling extension")
	})

	t.Run("extension_stops_at_the_ceiling", func(t *testing.T) {
		branch := stepBranch(t, 6, 200)
		one := spanTokens(branch, 6, len(branch))
		cut := verbatimCut(branch, 0, 2, one*4+one/2) // room for four steps, not five
		assert.Equal(t, 3, cut)
		assert.LessOrEqual(t, spanTokens(branch, cut, len(branch)), one*4+one/2)
	})

	t.Run("oversized_floor_is_kept_anyway", func(t *testing.T) {
		branch := stepBranch(t, 6, 400)
		cut := verbatimCut(branch, 0, 2, 1) // ceiling below any single step
		assert.Equal(t, 5, cut, "the floor outranks the ceiling")
		assert.Greater(t, spanTokens(branch, cut, len(branch)), 1)
	})

	t.Run("never_reaches_past_the_prior_cut", func(t *testing.T) {
		branch := stepBranch(t, 6, 5)
		assert.Equal(t, 5, verbatimCut(branch, 5, 4, 1<<20))
	})

	t.Run("pulls_in_the_adjacent_prompt", func(t *testing.T) {
		branch := []session.Entry{
			userText("u1", "first ask"),
			assistText("a1", "first answer"),
			userText("u2", "second ask"),
			assistText("a2", "second answer"),
		}
		assert.Equal(t, 2, verbatimCut(branch, 0, 1, 0),
			"the question that opened the step rides with it")
	})

	t.Run("leaves_an_injected_prompt_behind", func(t *testing.T) {
		branch := []session.Entry{
			userText("u1", "first ask"),
			assistText("a1", "first answer"),
			injectedText("i1", "Sub-agent sub-3 completed."),
			assistText("a2", "second answer"),
		}
		assert.Equal(t, 3, verbatimCut(branch, 0, 1, 0),
			"a system notice is not the user's question")
	})
}

func TestChooseCut(t *testing.T) {
	t.Parallel()

	t.Run("advances_past_the_prior_cut", func(t *testing.T) {
		branch := stepBranch(t, 8, 20)
		cut, ok := chooseCut(branch, 0, 2, 0)
		require.True(t, ok)
		assert.Equal(t, 7, cut)
	})

	t.Run("declines_when_the_band_reaches_it", func(t *testing.T) {
		branch := stepBranch(t, 8, 20)
		_, ok := chooseCut(branch, 7, 2, 0)
		assert.False(t, ok)
	})

	t.Run("declines_without_a_step", func(t *testing.T) {
		branch := []session.Entry{userText("u1", "only a prompt")}
		_, ok := chooseCut(branch, 0, 2, 1<<20)
		assert.False(t, ok)
	})

	t.Run("declines_when_the_span_holds_no_message", func(t *testing.T) {
		branch := []session.Entry{
			{ID: "s0", Type: session.TypeSession, Data: []byte(`{"model":"test/m"}`)},
			assistText("a1", "one"),
			assistText("a2", "two"),
		}
		_, ok := chooseCut(branch, 0, 2, 1<<20)
		assert.False(t, ok)
	})
}

// tailWellFormed reports whether every tool call in [cut:] is answered within it.
func tailWellFormed(branch []session.Entry, cut int) bool {
	calls := 0
	for i := cut; i < len(branch); i++ {
		if branch[i].Type != session.TypeMessage {
			continue
		}
		var md session.MessageData
		if err := branch[i].Decode(&md); err != nil {
			continue
		}
		for _, b := range md.Message.Content {
			switch blk := b.(type) {
			case llm.ToolCallBlock:
				calls++
			case llm.ToolResultBlock:
				if blk.CallID == "" || calls <= 0 {
					return false
				}
				calls--
			}
		}
	}
	return calls == 0
}

// countSteps reports how many steps a region holds.
func countSteps(branch []session.Entry) int {
	var n int
	for i := range branch {
		if isStepStart(branch[i]) {
			n++
		}
	}
	return n
}

// TestVerbatimCutFuzz generates branches with random tool interleavings and
// asserts the band is always well formed and always honours its bounds.
func TestVerbatimCutFuzz(t *testing.T) {
	t.Parallel()

	for seed := int64(0); seed < 40; seed++ {
		r := rand.New(rand.NewSource(seed))
		branch := randomBranch(r)
		// budgets cover the boundaries that matter (below one step, at a few steps,
		// far above), not every ~97-token rung: a dense sweep costs thousands of
		// JSON decodes per seed for no added interleaving coverage.
		budgets := []int{30, 100, 300, 600, 1200, 2000, 2900}
		for _, priorCut := range []int{0, len(branch) / 3} {
			for minSteps := 1; minSteps <= 4; minSteps++ {
				for _, budget := range budgets {
					cut := verbatimCut(branch, priorCut, minSteps, budget)
					where := func() string {
						return "seed " + strconv.FormatInt(seed, 10) + " prior " + strconv.Itoa(priorCut) +
							" steps " + strconv.Itoa(minSteps) + " budget " + strconv.Itoa(budget)
					}

					assert.GreaterOrEqual(t, cut, priorCut, "%s reached past the prior cut", where())
					steps := countSteps(branch[priorCut:])
					if steps == 0 {
						assert.Equal(t, len(branch), cut, "%s invented a band", where())
						continue
					}
					require.Less(t, cut, len(branch), "%s dropped an existing band", where())

					// the band opens on a step, or on the prompt that step answers
					assert.True(t, isStepStart(branch[cut]) || isLivePrompt(branch[cut]),
						"%s opened the band mid-result", where())
					assert.True(t, tailWellFormed(branch, cut), "%s orphans a tool call", where())

					kept := countSteps(branch[cut:])
					assert.LessOrEqual(t, kept, steps, "%s kept more steps than exist", where())
					assert.GreaterOrEqual(t, kept, min(minSteps, steps), "%s broke the floor", where())
					if kept > minSteps {
						// past the floor the ceiling binds, over the steps themselves: the
						// prompt withLivePrompt adds in front of them is deliberate
						firstStep := cut
						for firstStep < len(branch) && !isStepStart(branch[firstStep]) {
							firstStep++
						}
						assert.LessOrEqual(t, spanTokens(branch, firstStep, len(branch)), budget,
							"%s busted the ceiling past the floor", where())
					}

					// a second pass over an unchanged branch never reopens folded history
					assert.GreaterOrEqual(t, verbatimCut(branch, cut, minSteps, budget), cut,
						"%s moved backwards on re-entry", where())
				}
			}
		}
	}
}

// randomBranch builds a plausible branch mixing user prompts, injected notices,
// assistant text and tool call/result pairs in random order.
func randomBranch(r *rand.Rand) []session.Entry {
	var branch []session.Entry
	id := 0
	next := func() string { id++; return "e" + strconv.Itoa(id) }
	turns := 2 + r.Intn(5)
	for tn := 0; tn < turns; tn++ {
		branch = append(branch, userText(next(), strings.Repeat("q ", 20+r.Intn(80))))
		if r.Intn(3) == 0 {
			branch = append(branch, injectedText(next(), "Sub-agent completed."))
		}
		steps := r.Intn(3)
		for s := 0; s < steps; s++ {
			calls := 1 + r.Intn(2) // multi-call steps must stay intact
			var blocks llm.BlockList
			for c := 0; c < calls; c++ {
				blocks = append(blocks, llm.ToolCallBlock{
					ID:   "c" + strconv.Itoa(id) + "_" + strconv.Itoa(s) + "_" + strconv.Itoa(c),
					Name: "bash", Input: []byte(`{}`),
				})
			}
			branch = append(branch, msg(next(), llm.Message{Role: llm.RoleAssistant, Content: blocks}))
			for c := 0; c < calls; c++ {
				blk := blocks[c].(llm.ToolCallBlock)
				branch = append(branch, resultMsg(next(), blk.ID,
					strings.Repeat("o ", 10+r.Intn(120)), r.Intn(4) == 0))
			}
		}
		if r.Intn(2) == 0 {
			branch = append(branch, assistText(next(), strings.Repeat("a ", 10+r.Intn(60))))
		}
	}
	return branch
}
