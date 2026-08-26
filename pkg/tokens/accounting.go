package tokens

import (
	"sync"

	"github.com/jentfoo/ajent/pkg/llm"
)

// Accounting is a session's token ledger: per-response usage, running totals,
// and how full the next request will be (Context).
//
// Used = promptExact + outputExact + factor*(pending+live+composing+submitted),
// where the estimate terms are raw and scaled by the calibration factor at read
// time; they reset on each Response.
type Accounting struct {
	mu sync.Mutex

	model llm.Model
	cal   *Calibrator // shared with child ledgers of this session

	promptExact int     // last reported prompt size, Input + CacheRead + CacheWrite
	outputExact int     // last reported output tokens
	pending     float64 // raw estimate of messages appended since the last report
	live        float64 // raw estimate of the response currently streaming
	composing   float64 // raw estimate of text being typed but not yet sent
	submitted   float64 // sent text not yet appended to a message; cleared when it lands
	base        float64 // constant request overhead: system prompt + tool schemas

	total      llm.Usage            // cumulative billed input/output across the session
	totalChild llm.Usage            // delegated subset of total: what sub-agents spent
	byModel    map[string]llm.Usage // spend split by model key, for mid-session switches

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
	a.pending, a.live = 0, 0 // composing stays; the editor buffer is not a context term
}

// SetWindow rebases the ledger onto m's window and reserve, keeping every context
// term. Unlike SetModel it does not drop them: it is for a rebuild that resolved
// no model of its own, where the live model is the right frame for context the
// rebuild has already measured.
func (a *Accounting) SetWindow(m llm.Model) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.model = m
}

// SetCompose sets the estimate of text currently being typed but not yet sent.
// It replaces any prior composing value rather than accumulating, so each edit or
// paste reflects only the current buffer. Pass zero to clear it (e.g. on submit).
func (a *Accounting) SetCompose(est int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.composing = float64(est)
}

// SetSubmit sets the estimate of text handed to the agent but not yet appended as
// a message. Pass zero once the message lands and pending covers it.
func (a *Accounting) SetSubmit(est int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.submitted = float64(est)
}

// SetBase replaces the estimate of the constant request overhead: the system
// prompt and tool schemas that ride with every request but carry no provider
// report of their own. Messages are excluded; callers account those as they append.
func (a *Accounting) SetBase(est int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.base = float64(est)
}

// Response records one completed response: snaps exact terms to u, folds spend
// into totals/per-model split, and feeds the calibration factor. predicted is what
// EstimateRequest reported before streaming. keepThink reports whether retention
// replays this response's reasoning into the next request.
func (a *Accounting) Response(key string, u llm.Usage, predicted int, keepThink bool) {
	a.mu.Lock()
	prompt := u.Input + u.CacheRead + u.CacheWrite
	output := u.Output
	if !keepThink {
		// billed reasoning that retention strips never rides in the next request
		output = max(0, output-u.Reasoning)
	}
	if prompt > 0 {
		a.promptExact = prompt
		a.outputExact = output
		a.pending, a.live = 0, 0 // an exact prompt supersedes every estimate before it
	} else {
		// no exact prompt to supersede the estimate, so pending stays and this response
		// joins it. It cannot go to outputExact: that holds one turn only, and append
		// declines to re-add a message whose usage is non-zero, so every earlier
		// response would drop out of the bar as the next one overwrote it.
		a.pending += float64(output)
		a.live = 0 // superseded by the whole-message estimate above
	}
	if !Zero(u) {
		addSpend(a, key, u)
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
	if p := u.Input + u.CacheRead + u.CacheWrite; p > 0 {
		a.promptExact = p
		a.pending = 0 // now covered by the reported prompt
	}
}

// Add adds an estimate of messages appended since the last report.
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

// Rebase replaces the exact context term with a count from the provider's
// tokenizer, clearing both estimate buckets; used covers everything appended so far.
func (a *Accounting) Rebase(used int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if used > 0 {
		a.promptExact = used
	}
	a.outputExact, a.pending, a.live = 0, 0, 0
	// composing and submitted are untouched: neither is in what was counted, and
	// clearing them would drop the editor buffer from the bar until the next keystroke
}

// Reseed replaces the ledger's context terms with an estimate of the next
// request after compaction rewrites history, leaving session spend intact. The
// estimate sits in pending so Used reports factor*est and stays marked estimated.
func (a *Accounting) Reseed(est int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.promptExact, a.outputExact = 0, 0
	a.live = 0 // composing stays: the editor buffer is not part of est
	a.pending = float64(est)
}

// Spend folds usage into the session totals without touching any context term:
// for requests whose prompt is not the session's own context (a compaction
// summary call), so a failed compaction cannot leave the bar at the summariser's
// prompt size.
func (a *Accounting) Spend(key string, u llm.Usage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if Zero(u) {
		return
	}
	addSpend(a, key, u)
}

// RecordSpend folds one recorded response into session spend and turn counts
// without touching any context term. Session rebuild uses it so a branch whose
// history a compaction rewrote still reports the cost it really incurred, while
// its context comes from the assembled messages instead.
func (a *Accounting) RecordSpend(key string, u llm.Usage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if Zero(u) {
		// every appended message is a recorded entry, user echoes and tool results
		// included, so only a report makes one a turn. This matches the replay path,
		// which reaches Response for exactly the same entries.
		return
	}
	a.turnCount++
	addSpend(a, key, u)
}

// Context returns how full the next request will be.
func (a *Accounting) Context() ContextState {
	a.mu.Lock()
	defer a.mu.Unlock()
	// every estimate bucket scales by the same per-model factor so composing text
	// is corrected exactly like pending and live are.
	factor := a.cal.Factor(a.model.Key())
	est := a.pending + a.live + a.composing + a.submitted
	if a.promptExact == 0 {
		est += a.base // an exact prompt report already includes system and schemas
	}
	estimated := est * factor
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

// ChildTotal returns the cumulative usage rolled up from sub-agent ledgers,
// a subset of Total(). Zero until a child reports.
func (a *Accounting) ChildTotal() llm.Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.totalChild
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
	addSpend(a, key, u)
	a.totalChild.Add(u) // the delegated subset, tracked apart from the total
	if p := a.parent; p != nil {
		p.rollUp(key, u) // nested sub-agent spend reaches the session root too
	}
}

// addSpend folds usage into the session totals and the per-model split. The
// caller holds a.mu.
func addSpend(a *Accounting, key string, u llm.Usage) {
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
