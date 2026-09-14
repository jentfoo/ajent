package compact

import (
	"math"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
)

// tokensFor estimates the input tokens branch contributes under cd through the
// same ContextMessages and Prepare passes assembly uses, so what measures is what
// the next request gets. resolve stamps each assistant message with its producing
// model; nil leaves them unstamped (foreign). base adds the fixed request overhead.
func tokensFor(branch []session.Entry, cd session.CompactionData, model llm.Model, retain llm.RetainPolicy, base int, resolve func(string) (llm.Model, error)) int {
	msgs, warns := session.ContextMessages(branch, cd, resolve)
	if msgs == nil && len(warns) > 0 {
		return math.MaxInt // an unlocatable cut saves nothing
	}
	return base + tokens.EstimateFor(model, retain, msgs)
}

// compactAt reports where an automatic compaction fires for m; 0 when its window
// is unknown.
func compactAt(m llm.Model) int { return tokens.CompactAt(m) }
