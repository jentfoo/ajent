package compact

import (
	"math"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
)

// tokensFor estimates the request input tokens a branch contributes under cd,
// applying its cut and reductions exactly as assembly would. It is the single
// measure both per-stage early exit and the final report use.
func tokensFor(branch []session.Entry, cd session.CompactionData) int {
	msgs, warns := session.ContextMessages(branch, cd, nil)
	if msgs == nil && len(warns) > 0 {
		return math.MaxInt // an unlocatable cut saves nothing
	}
	return tokens.EstimateMessages(msgs)
}

// tokensReserve reports how many tokens a model holds back for its response.
func tokensReserve(m llm.Model) int { return m.Reserve() }
