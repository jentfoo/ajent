package tokens

import (
	"sync"

	"github.com/jentfoo/ajent/pkg/llm"
)

// Accounting is a session's token ledger: per-response usage, running totals,
// and how full the next request will be (Context).
//
// Used = promptExact + outputExact + factor*(pending+live), where pending/live
// are raw estimates scaled by the calibration factor at read time; both reset on
// each Response.
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
	byModel map[string]llm.Usage // spend split by model key, for mid-session switches

	turnCount    int // total responses recorded this session
	estTurnCount int // how many of those reported no usage

	parent *Accounting // nil except on a child ledger for a sub-agent
}

// New returns an empty ledger bound to m's window and reserve.
func New(m llm.Model) *Accounting {
	return &Accounting{model: m, cal: NewCalibrator()}
}

// SetModel rebases window/reserve onto m for a mid-session switch, dropping all
// context terms from before it while keeping accumulated spend intact.
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

// Response records one completed response: snaps exact terms to u, resets both
// estimate buckets, folds spend into totals/per-model split, and feeds the
// calibration factor. predicted is what EstimateRequest reported before streaming.
func (a *Accounting) Response(key string, u llm.Usage, predicted int) {
	a.mu.Lock()
	prompt := u.Input + u.CacheRead + u.CacheWrite
	if prompt > 0 || u.Output > 0 {
		a.promptExact = prompt
		a.outputExact = u.Output
	}
	a.pending, a.live = 0, 0 // estimates reset after every response
	if !Zero(u) {
		a.total.Add(u)
		addByModel(a, key, u)
	}
	est := Zero(u)
	a.turnCount++
	if est {
		a.estTurnCount++
	}
	parent := a.parent
	a.mu.Unlock()

	if parent != nil && !Zero(u) {
		parent.rollUp(key, u)
	}
	a.cal.Feed(key, predicted, prompt)
}

// Partial records a mid-stream usage snapshot, snapping promptExact and clearing
// pending (now covered by it) without touching live.
func (a *Accounting) Partial(u llm.Usage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if p := u.Input + u.CacheRead + u.CacheWrite; p > 0 || u.Output > 0 {
		a.promptExact = p
	}
	a.pending = 0
}

// Add adds an estimate of messages appended since the last report.
func (a *Accounting) Add(est int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending += float64(est)
}

// SeedBase estimates the constant context overhead — the system prompt and tool
// schemas that ride with every request but carry no provider report of their own.
// It adds est to pending only while no exact terms cover it yet, so a fresh epoch
// is seeded once and the next Response snapshot supersedes it. Messages are not
// included; callers account them individually as they append.
func (a *Accounting) SeedBase(est int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if est > 0 && a.promptExact == 0 && a.outputExact == 0 {
		a.pending += float64(est)
	}
}

// Stream adds to the live estimate of the response currently streaming.
func (a *Accounting) Stream(est int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.live += float64(est)
}

// Rebase replaces the exact context term with a count from the provider's
// tokenizer, clearing both estimate buckets; used covers everything appended so far.
func (a *Accounting) Rebase(used int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if used > 0 {
		a.promptExact = used
	}
	a.outputExact, a.pending, a.live, a.composing = 0, 0, 0, 0
}

// Reseed replaces the ledger's context terms with an estimate of the next
// request after compaction rewrites history, leaving session spend intact. The
// estimate sits in pending so Used reports factor*est and stays marked estimated.
func (a *Accounting) Reseed(est int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.promptExact, a.outputExact = 0, 0
	a.live, a.composing = 0, 0
	a.pending = float64(est)
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
		Reserve:   a.model.Reserve(),
		Compact:   CompactAt(a.model),
		Estimated: estimated > 0,
	}
}

// Total returns the cumulative billed usage across the session.
func (a *Accounting) Total() llm.Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.total
}

// TurnsCount returns the total responses recorded this session.
func (a *Accounting) TurnsCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.turnCount
}

// EstimatedTurn records a response whose provider reported no usage, so /usage can
// count it without disturbing the already-exact context terms.
func (a *Accounting) EstimatedTurn(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.turnCount++
	a.estTurnCount++
}

// EstimatedTurns returns how many recorded responses reported no usage, backing
// /usage's footnote.
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

// Child returns a nested ledger sharing this session's calibrator; its spend rolls
// up to the parent while it keeps its own context.
func (a *Accounting) Child() *Accounting {
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
