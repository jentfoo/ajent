package compact

import (
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
)

// entryTokens estimates the input tokens one message entry contributes. It uses
// the same estimator as measurement (tokens.EstimateMessage) so cut arithmetic and
// post-cut accounting cannot drift apart, which would let a measured saving differ
// from what the next request actually gets.
func entryTokens(e session.Entry) int {
	var md session.MessageData
	if err := e.Decode(&md); err != nil {
		return 0
	}
	return tokens.EstimateMessage(md.Message)
}

// spanTokens sums the estimated message tokens of branch[lo:hi).
func spanTokens(branch []session.Entry, lo, hi int) int {
	var n int
	for i := max(lo, 0); i < hi && i < len(branch); i++ {
		if branch[i].Type == session.TypeMessage {
			n += entryTokens(branch[i])
		}
	}
	return n
}

// isStepStart reports whether an entry opens a step. A step runs from an assistant
// message to just before the next one, carrying its tool calls and their results,
// which makes it the unit of recent work worth keeping whole.
func isStepStart(e session.Entry) bool {
	if e.Type != session.TypeMessage {
		return false
	}
	var md session.MessageData
	if err := e.Decode(&md); err != nil {
		return false
	}
	return md.Message.Role == llm.RoleAssistant
}

// isLivePrompt reports whether an entry is a real user prompt: typed by the user,
// not a tool result and not system-injected context.
func isLivePrompt(e session.Entry) bool {
	if e.Type != session.TypeMessage {
		return false
	}
	var md session.MessageData
	if err := e.Decode(&md); err != nil || md.Injected {
		return false
	}
	return md.Message.Role == llm.RoleUser && !llm.OnlyToolResults(md.Message.Content)
}

// verbatimCut returns the branch index the verbatim band starts at: the newest
// minSteps steps, kept whole however large, extended backwards with older steps
// while the band stays within maxTokens of message tokens. It never reaches
// earlier than priorCut. len(branch) means the region holds no step at all.
func verbatimCut(branch []session.Entry, priorCut, minSteps, maxTokens int) int {
	priorCut, minSteps = max(priorCut, 0), max(minSteps, 1)

	cut, seen, acc := len(branch), 0, 0
	for i := len(branch) - 1; i >= priorCut; i-- {
		if branch[i].Type != session.TypeMessage {
			continue
		}
		acc += entryTokens(branch[i]) // acc == spanTokens(branch, i, len(branch))
		if !isStepStart(branch[i]) {
			continue
		}
		seen++
		if seen <= minSteps {
			// the floor is kept whole even over the ceiling: a band that shrank below
			// the live work would leave every turn compacting again immediately
			cut = i
			continue
		}
		if acc > maxTokens { // maxTokens is always > 0; resolveVerbatim floors it
			break
		}
		cut = i
	}
	if cut >= len(branch) {
		return cut // no step in the region; there is no band to widen
	}
	return withLivePrompt(branch, cut, priorCut)
}

// withLivePrompt extends a band back over the user prompt that opens it, when one
// sits immediately before. Without it a mid-turn compaction folds the question the
// user just asked into the summary while keeping the answer to it verbatim. The
// prompt is added whatever it weighs — it is half of the live exchange — so the
// ceiling bounds the steps, not the prompt in front of them.
func withLivePrompt(branch []session.Entry, cut, priorCut int) int {
	for i := cut - 1; i >= priorCut; i-- {
		if branch[i].Type != session.TypeMessage {
			continue // notices and setting changes sit between without breaking the pair
		}
		if isLivePrompt(branch[i]) {
			return i
		}
		return cut
	}
	return cut
}

// chooseCut returns the index the verbatim band starts at and whether folding
// everything before it into a summary is worth a model call. It never moves
// earlier than priorCut, so a recompaction cannot reopen history a prior one
// folded.
func chooseCut(branch []session.Entry, priorCut, minSteps, maxTokens int) (int, bool) {
	cut := verbatimCut(branch, priorCut, minSteps, maxTokens)
	if cut <= priorCut || cut >= len(branch) {
		return 0, false // the band already reaches the prior cut, or holds no step
	}
	if countMessages(branch[priorCut:cut]) == 0 {
		return 0, false // an advance over non-message entries would summarise nothing
	}
	return cut, true
}
