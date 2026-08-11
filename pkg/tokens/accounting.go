package tokens

import (
	"sync"

	"github.com/jentfoo/ajent/pkg/llm"
)

// maxTurnRecords bounds the retained per-response history so a long session
// cannot grow it without limit; older entries are dropped from the front while
// TurnsCount/EstimatedTurns keep exact running totals.
const maxTurnRecords = 500

// TurnUsage is one response's reported usage. Estimated marks a provider that
// reported nothing (e.g. llama.cpp), so /usage can footnote it rather than
// present a guess as fact.
type TurnUsage struct {
	Model     string // llm.Model.Key()
	Usage     llm.Usage
	Estimated bool
}

// Accounting is a session's token ledger: what each response reported, the
// session total and how full the next request will be.
//
// Used = promptExact + outputExact + factor*(pending+live). The exact terms come
// from provider reports; pending (messages appended since) and live (the current
// streamed response) are raw estimates scaled by the calibration factor at read
// time. After every Response both estimate buckets reset to zero.
type Accounting struct {
	mu sync.Mutex

	model llm.Model
	cal   *Calibrator // shared with child ledgers of this session

	promptExact int     // last reported prompt size, Input + CacheRead + CacheWrite
	outputExact int     // last reported output tokens
	pending     float64 // raw estimate of messages appended since the last report
	live        float64 // raw estimate of the response currently streaming
	composing   float64 // raw estimate of text being typed but not yet sent

	total   llm.Usage            // cumulative billed input/output across the session
	turns   []TurnUsage          // recent per-response usage (bounded; see maxTurnRecords)
	byModel map[string]llm.Usage // spend split by model key, for mid-session switches

	turnCount    int // total responses recorded this session, exact even when turns is bounded
	estTurnCount int // how many of those reported no usage (Estimated=true)

	parent *Accounting // nil except on a child ledger (phase 13 sub-agents)
}

// New returns an empty ledger bound to m's window and reserve.
func New(m llm.Model) *Accounting {
	return &Accounting{model: m, cal: NewCalibrator()}
}

// SetModel rebases the window and reserve onto m for a mid-session /model switch,
// leaving accumulated spend intact. Every context term from before the switch is
// dropped — exact counts were tokenized by the old model just as estimates were —
// so nothing mixes two tokenizers until a fresh report lands under the new one.
func (a *Accounting) SetModel(m llm.Model) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.model = m
	a.promptExact, a.outputExact = 0, 0
	a.pending, a.live, a.composing = 0, 0, 0
}

// SetCompose sets the estimate of text currently being typed but not yet sent.
// It replaces any prior composing value rather than accumulating, so each edit or
// paste reflects only the current buffer. Pass zero to clear it (e.g. on submit).
func (a *Accounting) SetCompose(est int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.composing = float64(est)
}

// Response records one completed response: it snaps the context exact terms to u,
// resets both estimate buckets, folds spend into the totals and per-model split,
// and feeds the calibration factor. predicted is what EstimateRequest reported for
// that request before streaming.
func (a *Accounting) Response(key string, u llm.Usage, predicted int) {
	a.mu.Lock()
	if prompt := u.Input + u.CacheRead + u.CacheWrite; prompt > 0 || u.Output > 0 {
		a.promptExact = prompt
		a.outputExact = u.Output
	}
	a.pending, a.live = 0, 0 // estimates reset after every response
	a.fold(key, u)
	est := Zero(u)
	a.turnCount++
	if est {
		a.estTurnCount++
	}
	a.pushTurn(TurnUsage{Model: key, Usage: u, Estimated: est})
	prompt := u.Input + u.CacheRead + u.CacheWrite
	parent := a.parent
	a.mu.Unlock()

	if parent != nil && !Zero(u) {
		parent.rollUp(key, u)
	}
	a.cal.Feed(key, predicted, prompt)
}

// Partial records a mid-stream usage snapshot (anthropic reports input at
// message_start and output at message_delta). It clears pending — those messages
// are now inside promptExact — without touching live.
func (a *Accounting) Partial(u llm.Usage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if p := u.Input + u.CacheRead + u.CacheWrite; p > 0 || u.Output > 0 {
		a.promptExact = p
	}
	a.pending = 0
}

// Add records the estimated tokens of messages appended since the last report.
func (a *Accounting) Add(est int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending += float64(est)
}

// Stream adds to the live estimate of the response currently streaming.
func (a *Accounting) Stream(est int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.live += float64(est)
}

// EstimatedTurn records one response whose provider reported no usage (e.g. a
// remote-tokenize server that returns nothing), so /usage's turn count and footnote
// reflect what actually ran without disturbing the already-recounted context terms.
func (a *Accounting) EstimatedTurn(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.turnCount++
	a.estTurnCount++
	a.pushTurn(TurnUsage{Model: key, Usage: llm.Usage{}, Estimated: true})
}

// Rebase replaces the context exact terms with an exact count from the provider's
// tokenizer and clears both estimate buckets. used is that count for the next
// request (including everything appended so far).
func (a *Accounting) Rebase(used int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if used > 0 {
		a.promptExact = used
	}
	a.outputExact, a.pending, a.live, a.composing = 0, 0, 0, 0
}

// Context returns how full the next request will be.
func (a *Accounting) Context() ContextState {
	a.mu.Lock()
	defer a.mu.Unlock()
	// every estimate bucket scales by the same per-model factor so composing text
	// is corrected exactly like pending and live are.
	factor := a.cal.Factor(a.model.Key())
	estimated := (float64(a.pending+a.live) + float64(a.composing)) * factor
	usedEst := int(estimated + 0.5)
	return ContextState{
		Used:      a.promptExact + a.outputExact + usedEst,
		Window:    a.model.ContextWindow,
		Reserve:   Reserve(a.model),
		Estimated: estimated > 0,
	}
}

// Total returns the cumulative billed usage across the session.
func (a *Accounting) Total() llm.Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.total
}

// Turns returns the retained per-response usage in order (bounded to maxTurnRecords).
func (a *Accounting) Turns() []TurnUsage {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]TurnUsage, len(a.turns))
	copy(out, a.turns)
	return out
}

// TurnsCount returns the total responses recorded this session, exact even when
// the retained turns slice has been bounded.
func (a *Accounting) TurnsCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.turnCount
}

// EstimatedTurns returns how many recorded responses reported no usage. It is the
// exact count backing /usage's footnote.
func (a *Accounting) EstimatedTurns() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.estTurnCount
}

// ByModel returns spend split by model key. It is empty when the model never
// changed mid-session.
func (a *Accounting) ByModel() map[string]llm.Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]llm.Usage, len(a.byModel))
	for k, v := range a.byModel {
		out[k] = v
	}
	return out
}

// Child returns a nested ledger that folds spend up into the parent (phase 13's
// sub-agents) but keeps its own ContextState.
func (a *Accounting) Child(name string) *Accounting {
	a.mu.Lock()
	defer a.mu.Unlock()
	return &Accounting{model: a.model, cal: a.cal, parent: a}
}

// rollUp adds a descendant's spend to this ledger and propagates further up the
// parent chain. It never touches context fields.
func (a *Accounting) rollUp(key string, u llm.Usage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if Zero(u) {
		return
	}
	a.total.Add(u)
	addByModel(a, key, u)
	if p := a.parent; p != nil {
		p.rollUp(key, u) // nested sub-agent spend reaches the session root too
	}
}

// pushTurn appends one response record, dropping the oldest when the retained
// slice exceeds maxTurnRecords. Caller holds the lock.
func (a *Accounting) pushTurn(tu TurnUsage) {
	a.turns = append(a.turns, tu)
	if n := len(a.turns); n > maxTurnRecords {
		copy(a.turns, a.turns[n-maxTurnRecords:])
		a.turns = a.turns[:maxTurnRecords]
	}
}

// fold adds one response's usage to the totals and per-model split.
func (a *Accounting) fold(key string, u llm.Usage) {
	if Zero(u) {
		return
	}
	a.total.Add(u)
	addByModel(a, key, u)
}

func addByModel(a *Accounting, key string, u llm.Usage) {
	if a.byModel == nil {
		a.byModel = make(map[string]llm.Usage)
	}
	m := a.byModel[key]
	m.Add(u)
	a.byModel[key] = m
}

// Zero reports whether a usage report carries no numbers at all, which is what an
// unreported provider (e.g. llama.cpp) hands back.
func Zero(u llm.Usage) bool {
	return u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheWrite == 0
}
