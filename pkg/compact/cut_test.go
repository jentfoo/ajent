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

// cut point ------------------------------------------------------------

func TestSelectCut(t *testing.T) {
	t.Parallel()

	t.Run("whole_branch_fits", func(t *testing.T) {
		branch := []session.Entry{userText("u1", "hi"), assistText("a1", "yo")}
		assert.Equal(t, -1, selectCut(branch, 100000)) // nothing to cut
	})

	t.Run("empty_branch_or_zero_budget", func(t *testing.T) {
		assert.Equal(t, -1, selectCut(nil, 500))
		branch := []session.Entry{userText("u1", "hi")}
		assert.Equal(t, -1, selectCut(branch, 0)) // no budget: nothing to cut
	})

	t.Run("prefers_a_turn_start_when_over_budget", func(t *testing.T) {
		// an over-budget text-only branch still yields a real cut that lands on
		// a turn start or assistant boundary, never mid-turn.
		branch := []session.Entry{
			userText("u1", strings.Repeat("a ", 400)),
			assistText("a1", strings.Repeat("b ", 800)),
			userText("u2", strings.Repeat("c ", 200)),
		}
		cut := selectCut(branch, 500) // keep roughly the last turn
		require.NotEqual(t, -1, cut)
		assert.True(t, isValidCut(branch[cut])) // never a tool-result boundary
	})

	t.Run("never_cuts_mid_turn", func(t *testing.T) {
		// the budget line falls inside a turn; selectCut must snap off the
		// tool-result-only user message so the call/result pair stays intact.
		branch := []session.Entry{
			userText("u1", strings.Repeat("q ", 300)),
			callMsg("a1", "c1", "bash", ""),
			resultMsg("r1", "c1", strings.Repeat("o ", 400), false),
		}
		for budget := 100; budget <= 2000; budget += 50 {
			cut := selectCut(branch, budget)
			if cut == -1 {
				continue
			}
			assert.NotEqual(t, "r1", branch[cut].ID, "budget %d cuts on the tool result", budget)
			assert.True(t, tailWellFormed(branch, cut), "budget %d orphans a tool call", budget)
		}
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

// TestSelectCutFuzzWellFormed generates branches with random tool interleavings
// and asserts every cut keeps a well-formed tail: no orphaned tool_use, and the
// cut never lands on a tool-result-only message.
func TestSelectCutFuzzWellFormed(t *testing.T) {
	t.Parallel()

	for seed := int64(0); seed < 40; seed++ {
		r := rand.New(rand.NewSource(seed))
		branch := randomBranch(r)
		for budget := 30; budget <= 3000; budget += 97 {
			cut := selectCut(branch, budget)
			if cut == -1 {
				continue
			}
			assert.True(t, isValidCut(branch[cut]),
				"seed %d budget %d cut at a non-boundary", seed, budget)
			assert.True(t, tailWellFormed(branch, cut),
				"seed %d budget %d orphans a tool call", seed, budget)
		}
	}
}

// randomBranch builds a plausible branch mixing user prompts, assistant text and
// tool call/result pairs in random order.
func randomBranch(r *rand.Rand) []session.Entry {
	var branch []session.Entry
	id := 0
	next := func() string { id++; return "e" + strconv.Itoa(id) }
	turns := 2 + r.Intn(5)
	for tn := 0; tn < turns; tn++ {
		branch = append(branch, userText(next(), strings.Repeat("q ", 20+r.Intn(80))))
		steps := r.Intn(3)
		for s := 0; s < steps; s++ {
			cid := "c" + strconv.Itoa(id) + "_" + strconv.Itoa(s)
			branch = append(branch, callMsg(next(), cid, "bash", ""))
			branch = append(branch, resultMsg(next(), cid, strings.Repeat("o ", 10+r.Intn(120)), r.Intn(4) == 0))
		}
		if r.Intn(2) == 0 {
			branch = append(branch, assistText(next(), strings.Repeat("a ", 10+r.Intn(60))))
		}
	}
	return branch
}

func TestPriorSpanStart(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		branch []session.Entry
		want   int
		ok     bool
	}{
		{
			name: "no_prior_compaction",
			branch: []session.Entry{
				userText("u1", "q"),
				assistText("a1", "r"),
			},
			want: 0, ok: false,
		},
		{
			name: "summary_only_tail",
			branch: []session.Entry{
				userText("u1", "old ask"),
				assistText("a1", "old answer"),
				compactEntry("comp", "everything folded", ""),
				userText("u2", "new ask"),
			},
			want: 3, ok: true,
		},
		{
			name:   "reductions_only_restarts_root",
			branch: []session.Entry{compactEntry("comp", "", "")}, // no summary, no first kept
			want:   0, ok: true,
		},
		{
			name: "first_kept_found",
			branch: []session.Entry{
				userText("u1", "q"),
				compactEntry("comp", "s", "u2"),
				userText("u2", "r"),
			},
			want: 2, ok: true,
		},
		{
			name:   "first_kept_not_found",
			branch: []session.Entry{compactEntry("comp", "s", "zzz")}, // id absent from branch
			want:   0, ok: true,
		},
		{
			name: "summary_only_empty_tail",
			branch: []session.Entry{
				userText("u1", "q"),
				compactEntry("comp", "folded all", ""), // nothing recorded after it
			},
			want: 2, ok: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, ok := priorSpanStart(tc.branch)
			assert.Equal(t, tc.want, start)
			assert.Equal(t, tc.ok, ok)
		})
	}
}
