package compact

import (
	"math"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
)

// tokensFor estimates the request input tokens a branch contributes under cd,
// applying its cut and reductions exactly as assembly would, then the same
// Prepare pass the wire applies (retention, cross-model degradation, repair) so
// thinking a request would drop is never counted as context compaction saved.
// base is the fixed request overhead (system block + tool schemas); it is added
// so Before/After measure full usage, not just messages.
func tokensFor(branch []session.Entry, cd session.CompactionData, model llm.Model, retain llm.RetainPolicy, base int) int {
	msgs, warns := session.ContextMessages(branch, cd, nil)
	if msgs == nil && len(warns) > 0 {
		return math.MaxInt // an unlocatable cut saves nothing
	}
	return base + tokens.EstimateFor(model, retain, msgs)
}

// compactAt reports where an automatic compaction fires for m; 0 when its window
// is unknown.
func compactAt(m llm.Model) int { return tokens.CompactAt(m) }
