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

// selectCut picks the branch index to keep from (FirstKeptEntryID), leaving at
// most tailBudget recent tokens in context. It walks backwards accumulating until
// the budget is met, then snaps to the nearest valid turn boundary so the kept
// tail stays well formed. Returns -1 when nothing needs cutting.
func selectCut(branch []session.Entry, tailBudget int) int {
	if len(branch) == 0 || tailBudget <= 0 {
		return -1
	}
	var acc int
	at := -1 // index where the budget was first met, walking backwards
	for i := len(branch) - 1; i >= 0; i-- {
		if branch[i].Type != session.TypeMessage {
			continue
		}
		acc += entryTokens(branch[i])
		if acc >= tailBudget {
			at = i
			break
		}
	}
	if at == -1 {
		return -1 // the whole branch already fits within tailBudget
	}

	// snap backward to the nearest valid cut at or before `at`: keeping from there
	// retains the full budget (never less) and, when `at` is a tool result, picks up
	// the assistant call that owns it so the pair is never split.
	for j := at; j >= 0; j-- {
		if isValidCut(branch[j]) {
			return j
		}
	}
	// nothing valid behind the line; fall forward to the first valid cut so the
	// tail is still well formed, even if it keeps less than asked.
	for j := at + 1; j < len(branch); j++ {
		if isValidCut(branch[j]) {
			return j
		}
	}
	return -1 // no safe cut exists
}

// isValidCut reports whether cutting at this entry keeps a well-formed tail: a
// turn start (a real user prompt), or an assistant message whose tool calls and
// results all live after it in [i:].
func isValidCut(e session.Entry) bool {
	if e.Type != session.TypeMessage {
		return false
	}
	var md session.MessageData
	if err := e.Decode(&md); err != nil {
		return false
	}
	switch md.Message.Role {
	case llm.RoleUser:
		return !llm.OnlyToolResults(md.Message.Content) // a turn start, never mid-result
	case llm.RoleAssistant:
		return true // keeps its own tool_use and the results that follow in [i:]
	default:
		return false
	}
}

// priorSpanStart returns the index where this compaction's summarisable span
// begins: a previous compaction's FirstKeptEntryID when one exists (so summaries
// merge rather than nest), else 0. It reports false when there is nothing new to
// summarise.
func priorSpanStart(branch []session.Entry) (int, bool) {
	for i := len(branch) - 1; i >= 0; i-- {
		if branch[i].Type != session.TypeCompaction {
			continue
		}
		var cd session.CompactionData
		if err := branch[i].Decode(&cd); err != nil {
			return 0, true
		}
		if cd.FirstKeptEntryID == "" {
			if cd.Summary != "" { // summary-only: only what follows it is new
				return i + 1, i+1 < len(branch)
			}
			return 0, true // reductions only: restart from the root
		}
		for j := range branch {
			if branch[j].ID == cd.FirstKeptEntryID {
				return j, j < len(branch) // empty span (nothing after it) means no-op
			}
		}
		return 0, true
	}
	return 0, false // no prior compaction at all; the whole history is new
}
