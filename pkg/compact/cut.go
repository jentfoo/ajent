package compact

import (
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
)

// branchView caches per-entry decodes and token estimates for one pass over a
// branch, so cut arithmetic and the summariser decode each message entry once.
type branchView struct {
	branch []session.Entry
	state  []msgState
	msgs   []session.MessageData
	toks   []int

	retain        llm.RetainPolicy // band-ceiling retention; zero counts thinking
	keepFrom      int              // branch index of the newest turn-opening user message
	lastAssistant int              // branch index of the newest assistant message
}

type msgState uint8

const (
	msgUncached msgState = iota
	msgOK
	msgBad // not a message entry or undecodable
)

func newBranchView(branch []session.Entry) *branchView {
	v := &branchView{
		branch: branch,
		state:  make([]msgState, len(branch)),
		msgs:   make([]session.MessageData, len(branch)),
		toks:   make([]int, len(branch)),
	}
	v.setRetain(llm.RetainAll)
	return v
}

// setRetain sets the band-ceiling retention policy, resolving the turn bounds
// Prepare's retention uses.
func (v *branchView) setRetain(retain llm.RetainPolicy) {
	v.retain = retain
	v.keepFrom, v.lastAssistant = -1, -1
	for i := range v.branch {
		if isStepStart(v.branch[i]) {
			v.lastAssistant = i
			continue
		}
		if md, ok := v.message(i); ok && md.Message.Role == llm.RoleUser &&
			!llm.OnlyToolResults(md.Message.Content) {
			v.keepFrom = i
		}
	}
}

// message returns the decoded message at i, false when the entry carries none.
func (v *branchView) message(i int) (session.MessageData, bool) {
	if i < 0 || i >= len(v.branch) {
		return session.MessageData{}, false
	}
	switch v.state[i] {
	case msgOK:
		return v.msgs[i], true
	case msgBad:
		return session.MessageData{}, false
	}
	var md session.MessageData
	if v.branch[i].Type != session.TypeMessage || v.branch[i].Decode(&md) != nil {
		v.state[i] = msgBad
		return session.MessageData{}, false
	}
	v.state[i], v.msgs[i] = msgOK, md
	return md, true
}

// tokens returns the cached estimate for entry i, 0 when it carries none.
func (v *branchView) tokens(i int) int {
	if _, ok := v.message(i); !ok {
		return 0
	}
	if n := v.toks[i]; n > 0 {
		return n // a decodable message always estimates above zero
	}
	n := tokens.EstimateMessage(v.msgs[i].Message)
	v.toks[i] = n
	return n
}

// spanTokens sums the estimated message tokens of branch[lo:hi).
func (v *branchView) spanTokens(lo, hi int) int {
	var n int
	for i := max(lo, 0); i < hi && i < len(v.branch); i++ {
		n += v.tokens(i)
	}
	return n
}

// sentTokens estimates entry i as the request carries it: thinking the retain
// policy drops is excluded, matching the Prepare pass on every measure.
func (v *branchView) sentTokens(i int) int {
	n := v.tokens(i)
	md, ok := v.message(i)
	if !ok || retainsThinking(v.retain, i, v.keepFrom, v.lastAssistant) {
		return n
	}
	for _, blk := range md.Message.Content {
		if th, ok := blk.(llm.ThinkingBlock); ok {
			n -= tokens.EstimateBlocks(llm.BlockList{th})
		}
	}
	return n
}

// retainsThinking reports whether entry i's thinking survives retention,
// mirroring llm's per-message rule in branch-index space.
func retainsThinking(policy llm.RetainPolicy, i, keepFrom, lastAssistant int) bool {
	switch policy {
	case llm.RetainLastTurn:
		return i == lastAssistant
	case llm.RetainWholeTurn:
		return i >= keepFrom
	case llm.RetainAll:
		return true
	default:
		return false
	}
}

// countMessages reports how many message entries a span holds, for the notice.
func (v *branchView) countMessages(lo, hi int) int {
	var n int
	for i := max(lo, 0); i < hi && i < len(v.branch); i++ {
		if v.branch[i].Type == session.TypeMessage {
			n++
		}
	}
	return n
}

// isStepStart reports whether an entry opens a step. A step runs from an assistant
// message to just before the next one.
func isStepStart(e session.Entry) bool {
	if e.Type != session.TypeMessage {
		return false
	}
	var md session.MessageData
	return e.Decode(&md) == nil && md.Message.Role == llm.RoleAssistant
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
func (v *branchView) verbatimCut(priorCut, minSteps, maxTokens int) int {
	priorCut, minSteps = max(priorCut, 0), max(minSteps, 1)

	var cut, seen, acc = len(v.branch), 0, 0
	for i := len(v.branch) - 1; i >= priorCut; i-- {
		acc += v.sentTokens(i)
		if !isStepStart(v.branch[i]) {
			continue // an unreadable entry or a non-assistant message opens no step
		}
		seen++
		if seen <= minSteps {
			// the floor is kept whole even over the ceiling: a band that shrank below
			// the live work would leave every turn compacting again immediately
			cut = i
			continue
		}
		if acc > maxTokens { // maxTokens is always > 0, resolveVerbatim floors it
			break
		}
		cut = i
	}
	if cut >= len(v.branch) {
		return cut // no step in the region, there is no band to widen
	}
	return v.withLivePrompt(cut, priorCut)
}

// withLivePrompt extends a band back over the user prompt that opens it, when one
// sits immediately before. Without it a mid-turn compaction folds the question the
// user just asked into the summary while keeping the answer to it verbatim. The
// prompt is half of the live exchange, so whatever it weighs only affects how
// many steps fit under the ceiling, not whether the prompt itself does.
func (v *branchView) withLivePrompt(cut, priorCut int) int {
	for i := cut - 1; i >= priorCut; i-- {
		if v.branch[i].Type != session.TypeMessage {
			continue // notices and setting changes sit between without breaking the pair
		}
		md, ok := v.message(i)
		if ok && !md.Injected && md.Message.Role == llm.RoleUser && !llm.OnlyToolResults(md.Message.Content) {
			return i
		}
		return cut
	}
	return cut
}

// chooseCut returns the index the verbatim band starts at and whether folding
// everything before it into a summary is worth a model call. It never moves
// earlier than priorCut, so a recompaction cannot reopen history a prior one folded.
func (v *branchView) chooseCut(priorCut, minSteps, maxTokens int) (int, bool) {
	cut := v.verbatimCut(priorCut, minSteps, maxTokens)
	if cut <= priorCut || cut >= len(v.branch) {
		return 0, false // the band already reaches the prior cut, or holds no step
	}
	if v.countMessages(priorCut, cut) == 0 {
		return 0, false // an advance over non-message entries would summarise nothing
	}
	return cut, true
}
