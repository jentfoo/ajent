package app

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/tui"
)

var (
	// ErrInteractionUnknown names a resolve against no open dialog: a stale id,
	// or one whose dialog already settled.
	ErrInteractionUnknown = errors.New("no such interaction")
	// ErrInteractionSettled names a resolve that lost the race to a first answer.
	ErrInteractionSettled = errors.New("interaction already settled")
)

// dialogKind names one brokered dialog shape, so a client knows how to answer.
type dialogKind uint8

const (
	dialogPick dialogKind = iota
	dialogMultiPick
	dialogSelect
	dialogConfirm
	dialogInput
	dialogSecret
	dialogAsk
	dialogDecision
)

func (k dialogKind) String() string {
	switch k {
	case dialogPick:
		return "pick"
	case dialogMultiPick:
		return "multipick"
	case dialogSelect:
		return "select"
	case dialogConfirm:
		return "confirm"
	case dialogInput:
		return "input"
	case dialogSecret:
		return "secret"
	case dialogAsk:
		return "ask"
	default:
		return "decision"
	}
}

// answerShapes describes how a dialog may be answered: which shapes resolve
// it and what each shape answers against. New dialogs declare their shapes here.
type answerShapes struct {
	options []string // choice labels an index answer selects from
	multi   bool     // admits an indexes answer over the options
	text    bool     // admits a text answer
	secret  bool     // a text answer, redacted in announcements and outcomes
	chat    bool     // admits marking a text answer as an own-words reply
}

// interactionInfo announces one open dialog: what it asks, the options it
// offers and how it may be answered. Free-text answers never echo here.
type interactionInfo struct {
	ID      uint64
	Kind    dialogKind
	Prompt  string
	Subject string
	Shapes  answerShapes
}

// interactionAnswer is one dialog's answer, from the local surface or a remote
// resolver. Exactly one shape applies, keyed by the dialog kind.
type interactionAnswer struct {
	Index    int   // choice kinds
	Indexes  []int // multi pick
	Text     string
	Chat     bool // ask: replied in own words rather than choosing
	Declined bool // Esc: no answer, not a failure
	Err      error
}

// interactionOutcome is what an End observer learns: the answer shape when one
// landed, else how the dialog closed. Secret text never appears.
type interactionOutcome struct {
	Kind      dialogKind
	Answered  bool
	Cancelled bool
	Index     int
	Indexes   []int
	Text      string
	Chat      bool
}

// outcome derives the observer view of a, redacting secret answers.
func (a interactionAnswer) outcome(kind dialogKind, shapes answerShapes) interactionOutcome {
	o := interactionOutcome{Kind: kind, Index: a.Index, Indexes: a.Indexes,
		Text: a.Text, Chat: a.Chat}
	switch {
	case a.Declined:
		o.Cancelled = true
	case a.Err != nil:
		o.Cancelled = true // abandoned or failed: no answer to report
	default:
		o.Answered = true
	}
	if shapes.secret {
		o.Text = ""
	}
	return o
}

// localDialogs is the interactive surface the broker races: the TUI's own
// dialog implementations. *tui.UI satisfies it; tests substitute a scripted one.
type localDialogs interface {
	Mode() tui.Mode
	PickContext(ctx context.Context, prompt string, items []tui.PickItem, opts tui.PickOptions) (int, error)
	MultiPickContext(ctx context.Context, prompt string, items []tui.PickItem, opts tui.MultiPickOptions) ([]int, error)
	SelectContext(ctx context.Context, prompt string, options []tui.Option) (int, error)
	ConfirmContext(ctx context.Context, prompt string) (bool, error)
	InputContext(ctx context.Context, label, placeholder string) (string, error)
	InputSecretContext(ctx context.Context, label, placeholder string) (string, error)
	Ask(ctx context.Context, q tui.Question) (tui.Answer, error)
	OpenDecision(req tui.DecisionRequest) *tui.Decision
}

// interactionBroker fronts every dialog the run can open: the six Console
// interactions, agent questions and approval decisions. Each dialog is
// announced on open, raced between the local surface and any remote answerer
// (first wins), and always ends with an outcome. ui is the local racer, nil
// when no local surface exists: dialogs then hold for a remote answer.
type interactionBroker struct {
	ui localDialogs

	mu         sync.Mutex
	nextID     uint64
	open       map[uint64]*brokerDialog
	ended      map[uint64]struct{} // settled ids, so a late resolve reports settled
	endedOrder []uint64            // settled ids oldest first, bounding the map
	onBegin    func(interactionInfo)
	onEnd      func(id uint64, o interactionOutcome)
}

// newInteractionBroker builds the broker over the local surface, which may be
// nil. The hooks are optional observer seams; nil drops the announcements.
func newInteractionBroker(ui localDialogs, onBegin func(interactionInfo), onEnd func(uint64, interactionOutcome)) *interactionBroker {
	return &interactionBroker{ui: ui, onBegin: onBegin, onEnd: onEnd}
}

// observe chains announcement hooks ahead of any already installed, so a
// late attach (the control server) sees dialogs without displacing earlier
// observers. Safe before any dialog opens.
func (b *interactionBroker) observe(onBegin func(interactionInfo), onEnd func(uint64, interactionOutcome)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	oldBegin, oldEnd := b.onBegin, b.onEnd
	b.onBegin = func(info interactionInfo) {
		if oldBegin != nil {
			oldBegin(info)
		}
		onBegin(info)
	}
	b.onEnd = func(id uint64, o interactionOutcome) {
		if oldEnd != nil {
			oldEnd(id, o)
		}
		onEnd(id, o)
	}
}

// endedCap bounds the settled-id memory: past it the oldest id drops, which
// only downgrades a late resolve's answer from "settled" to "unknown", both
// legible errors a resolver already handles.
const endedCap = 512

// brokerDialog is one brokered interaction: its announcement, the answer slot
// the local human and any remote answerer race for, and its end. First
// resolution wins, every path settles exactly once.
type brokerDialog struct {
	b       *interactionBroker
	id      uint64
	kind    dialogKind
	prompt  string
	subject string
	shapes  answerShapes

	mu       sync.Mutex
	resolved bool
	answer   interactionAnswer
	done     chan struct{}
	raceCtx  context.Context // the racer's context, cancelled by the first settle
	abandon  func()          // cancels the local racer, run by the first settle
}

// begin announces and registers one dialog. The announced options are cloned
// once, so the dialog and its observers share a slice no caller can reach. The
// abandon cancel is created here under the dialog lock, so a settle racing the
// registration can always reach it and the orphan window cannot exist. The
// pending announcement fires under the same lock, so settled can never
// overtake pending on the wire.
func (b *interactionBroker) begin(parent context.Context, kind dialogKind, prompt, subject string, shapes answerShapes) *brokerDialog {
	shapes.options = slices.Clone(shapes.options)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	raceCtx, cancel := context.WithCancel(parent)
	d := &brokerDialog{
		b: b, id: b.nextID, kind: kind,
		prompt: prompt, subject: subject, shapes: shapes,
		raceCtx: raceCtx, abandon: cancel,
		done: make(chan struct{}),
	}
	if b.open == nil {
		b.open = make(map[uint64]*brokerDialog)
		b.ended = make(map[uint64]struct{})
		b.endedOrder = nil
	}
	b.open[d.id] = d
	if b.onBegin != nil {
		b.onBegin(interactionInfo{ID: d.id, Kind: kind, Prompt: prompt,
			Subject: subject, Shapes: shapes})
	}
	return d
}

// end unregisters d and reports its outcome. The settled announcement fires
// under the broker lock, so a client can never see settled before pending.
func (b *interactionBroker) end(d *brokerDialog, a interactionAnswer) {
	b.mu.Lock()
	delete(b.open, d.id)
	if _, dup := b.ended[d.id]; !dup {
		b.ended[d.id] = struct{}{}
		b.endedOrder = append(b.endedOrder, d.id)
		if len(b.endedOrder) > endedCap { // bound the growth; oldest forgets first
			delete(b.ended, b.endedOrder[0])
			b.endedOrder = b.endedOrder[1:]
		}
	}
	if b.onEnd != nil {
		b.onEnd(d.id, a.outcome(d.kind, d.shapes))
	}
	b.mu.Unlock()
}

// settle records the winning answer, firing End exactly once. It reports
// whether this call won.
func (d *brokerDialog) settle(a interactionAnswer) bool {
	d.mu.Lock()
	if d.resolved {
		d.mu.Unlock()
		return false
	}
	d.resolved = true
	d.answer = a
	abandon := d.abandon
	close(d.done)
	d.mu.Unlock()

	if abandon != nil {
		abandon() // idempotent: harmless when the racer already returned
	}
	d.b.end(d, a)
	return true
}

// winner returns the settled answer. Call only after done closes.
func (d *brokerDialog) winner() interactionAnswer {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.answer
}

// wait blocks until the dialog settles or ctx ends, settling it cancelled. It
// returns the winning answer.
func (d *brokerDialog) wait(ctx context.Context) interactionAnswer {
	select {
	case <-d.done:
	case <-ctx.Done():
		d.settle(interactionAnswer{Err: tui.ErrCancelled})
	}
	return d.winner()
}

// race settles d from whichever arrives first: the local surface, a remote
// resolve, or ctx ending. The abandon cancel was installed at begin from this
// same parent, so a settle racing this call still cancels the local racer. A
// local racer that cannot ask at all (ErrNoUI) leaves the dialog open for a
// remote answer. It returns the winning answer.
func (b *interactionBroker) race(ctx context.Context, d *brokerDialog, local func(context.Context) interactionAnswer) interactionAnswer {
	if local != nil {
		go func() {
			a := local(ctx)
			if errors.Is(a.Err, tui.ErrNoUI) {
				return // nobody local to ask: the dialog stays open for a remote answer
			}
			d.settle(a)
		}()
	}
	// the dialog's own context, not the caller's: the first settle cancels it,
	// so wait's cancel-settle path and the racers share one lifetime
	return d.wait(d.raceCtx) //nolint:contextcheck // rooted at begin, cancelled by the first settle
}

// Resolve answers an open dialog remotely. It reports an unknown id, or
// ErrInteractionSettled when a first answer already won (including after the
// dialog's End unregistered it).
func (b *interactionBroker) Resolve(id uint64, a interactionAnswer) error {
	b.mu.Lock()
	d, ok := b.open[id]
	_, ended := b.ended[id]
	b.mu.Unlock()
	switch {
	case !ok:
		if ended {
			return ErrInteractionSettled
		}
		return ErrInteractionUnknown
	case !d.settle(a):
		return ErrInteractionSettled
	}
	return nil
}

// lookup reports one dialog's announcement shape and state: open when a
// dialog with that id still holds, ended when one settled, so a caller can
// distinguish a stale id from an unknown one.
func (b *interactionBroker) lookup(id uint64) (info interactionInfo, open, ended bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ended = b.ended[id]
	d, ok := b.open[id]
	if !ok {
		return interactionInfo{}, false, ended
	}
	return interactionInfo{ID: d.id, Kind: d.kind, Prompt: d.prompt,
		Subject: d.subject, Shapes: d.shapes}, true, false
}

// holding reports the earliest still-open dialog, the one a local surface
// renders first and therefore the one holding the editor. Callers naming what
// owns the input read its Kind.
func (b *interactionBroker) holding() (interactionInfo, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var best interactionInfo
	for _, d := range b.open {
		if best.ID == 0 || d.id < best.ID {
			best = interactionInfo{ID: d.id, Kind: d.kind, Prompt: d.prompt,
				Subject: d.subject, Shapes: d.shapes}
		}
	}
	return best, best.ID != 0
}

// Pick presents a filterable list, brokered. See tui.PickContext.
func (b *interactionBroker) Pick(ctx context.Context, prompt string, items []tui.PickItem, opts tui.PickOptions) (int, error) {
	var local func(context.Context) (int, error)
	if b.ui != nil {
		local = func(ctx context.Context) (int, error) { return b.ui.PickContext(ctx, prompt, items, opts) }
	}
	d := b.begin(ctx, dialogPick, prompt, "", answerShapes{options: pickLabels(items)})
	return choiceResult(b.race(ctx, d, indexRacer(local)))
}

// MultiPick presents a filterable multi-select list, brokered.
func (b *interactionBroker) MultiPick(ctx context.Context, prompt string, items []tui.PickItem, opts tui.MultiPickOptions) ([]int, error) {
	var local func(context.Context) ([]int, error)
	if b.ui != nil {
		local = func(ctx context.Context) ([]int, error) { return b.ui.MultiPickContext(ctx, prompt, items, opts) }
	}
	d := b.begin(ctx, dialogMultiPick, prompt, "",
		answerShapes{options: pickLabels(items), multi: true})
	a := b.race(ctx, d, multiRacer(local))
	return multiResult(a)
}

// Select presents a fixed option list, brokered.
func (b *interactionBroker) Select(ctx context.Context, prompt string, options []tui.Option) (int, error) {
	var local func(context.Context) (int, error)
	if b.ui != nil {
		local = func(ctx context.Context) (int, error) { return b.ui.SelectContext(ctx, prompt, options) }
	}
	d := b.begin(ctx, dialogSelect, prompt, "", answerShapes{options: optionLabels(options)})
	return choiceResult(b.race(ctx, d, indexRacer(local)))
}

// Confirm asks a yes or no question, brokered.
func (b *interactionBroker) Confirm(ctx context.Context, prompt string) (bool, error) {
	var local func(context.Context) (bool, error)
	if b.ui != nil {
		local = func(ctx context.Context) (bool, error) { return b.ui.ConfirmContext(ctx, prompt) }
	}
	d := b.begin(ctx, dialogConfirm, prompt, "", answerShapes{options: []string{"Yes", "No"}})
	a := b.race(ctx, d, boolRacer(local))
	if a.Err != nil || a.Declined {
		_, err := choiceResult(a)
		return false, err
	}
	return a.Index == 0, nil
}

// Input prompts for one line of text, brokered.
func (b *interactionBroker) Input(ctx context.Context, label, placeholder string) (string, error) {
	return b.text(ctx, dialogInput, label, placeholder, false)
}

// InputSecret is Input for sensitive text, brokered: the answer reaches its
// caller but never an announcement or outcome.
func (b *interactionBroker) InputSecret(ctx context.Context, label, placeholder string) (string, error) {
	return b.text(ctx, dialogSecret, label, placeholder, true)
}

func (b *interactionBroker) text(ctx context.Context, kind dialogKind, label, placeholder string, secret bool) (string, error) {
	var local func(context.Context) (string, error)
	if b.ui != nil {
		if secret {
			local = func(ctx context.Context) (string, error) { return b.ui.InputSecretContext(ctx, label, placeholder) }
		} else {
			local = func(ctx context.Context) (string, error) { return b.ui.InputContext(ctx, label, placeholder) }
		}
	}
	d := b.begin(ctx, kind, label, "", answerShapes{text: true, secret: secret})
	a := b.race(ctx, d, textRacer(local))
	return textResult(a)
}

// Ask puts an agent question, brokered. See tui.Ask.
func (b *interactionBroker) Ask(ctx context.Context, q tui.Question) (tui.Answer, error) {
	var racer func(context.Context) interactionAnswer
	if b.ui != nil {
		racer = func(ctx context.Context) interactionAnswer {
			ans, err := b.ui.Ask(ctx, q)
			return askAnswer(ans, err)
		}
	}
	d := b.begin(ctx, dialogAsk, q.Text, "",
		answerShapes{options: optionLabels(q.Options), text: true, chat: true})
	return askResult(b.race(ctx, d, racer))
}

// OpenDecision opens an approval dialog, brokered. A plain-render UI cannot
// show one, so it reports ErrNoUI with no dialog opened; with no local surface
// at all the dialog opens and holds for a remote answer.
func (b *interactionBroker) OpenDecision(req tui.DecisionRequest) (permit.Dialog, error) {
	if len(req.Options) == 0 {
		return noDecision{}, nil // nothing to choose: exactly the TUI's no-dialog handle
	}
	if b.ui != nil && b.ui.Mode() == tui.ModePlain {
		return nil, tui.ErrNoUI
	}
	// no caller context exists on this seam (permit.Dialog carries none), so the
	// dialog roots its own cancelable one: begin mints the race context under the
	// dialog lock and stores its cancel as the abandon the first settle runs
	d := b.begin(context.Background(), dialogDecision, req.Prompt, req.Context,
		answerShapes{options: optionLabels(req.Options)})
	if b.ui != nil {
		// the abandon cancel exists from begin, so a mode-cycle resolve racing
		// this racer still cancels it
		go func() {
			td := b.ui.OpenDecision(req)
			r, err := td.Wait(d.raceCtx)
			if errors.Is(err, tui.ErrNoUI) {
				return // no dialog could be shown: it stays open for a remote answer
			}
			d.settle(interactionAnswer{Index: r.Index, Err: err})
		}()
	}
	return decisionHandle{d: d}, nil
}

// decisionHandle adapts one brokered decision onto permit.Dialog: Wait blocks
// for the settle, Resolve answers programmatically (the mode-change path),
// Close abandons. Esc maps onto the barrier's explicit denial.
type decisionHandle struct {
	d *brokerDialog
}

func (h decisionHandle) Wait(ctx context.Context) (int, error) {
	a := h.d.wait(ctx)
	if a.Declined || errors.Is(a.Err, tui.ErrCancelled) {
		return 0, permit.ErrDenied // an explicit Esc is a real denial, never a generic failure
	}
	if a.Err != nil {
		return 0, a.Err
	}
	return a.Index, nil
}

func (h decisionHandle) Resolve(index int) { h.d.settle(interactionAnswer{Index: index}) }

func (h decisionHandle) Close() { h.d.settle(interactionAnswer{Err: tui.ErrCancelled}) }

// noDecision is a decision that could never open: Wait reports ErrNoUI exactly
// as the TUI does for a request carrying no options.
type noDecision struct{}

func (noDecision) Wait(context.Context) (int, error) { return 0, tui.ErrNoUI }
func (noDecision) Resolve(int)                       {}
func (noDecision) Close()                            {}

// racer adapters turn one local dialog implementation into an answer producer.

func indexRacer(local func(context.Context) (int, error)) func(context.Context) interactionAnswer {
	if local == nil {
		return nil
	}
	return func(ctx context.Context) interactionAnswer {
		i, err := local(ctx)
		return interactionAnswer{Index: i, Err: err}
	}
}

func multiRacer(local func(context.Context) ([]int, error)) func(context.Context) interactionAnswer {
	if local == nil {
		return nil
	}
	return func(ctx context.Context) interactionAnswer {
		idx, err := local(ctx)
		return interactionAnswer{Indexes: idx, Err: err}
	}
}

func boolRacer(local func(context.Context) (bool, error)) func(context.Context) interactionAnswer {
	if local == nil {
		return nil
	}
	return func(ctx context.Context) interactionAnswer {
		yes, err := local(ctx)
		return interactionAnswer{Index: boolToInt(yes), Err: err}
	}
}

func textRacer(local func(context.Context) (string, error)) func(context.Context) interactionAnswer {
	if local == nil {
		return nil
	}
	return func(ctx context.Context) interactionAnswer {
		s, err := local(ctx)
		return interactionAnswer{Text: s, Err: err}
	}
}

func askAnswer(ans tui.Answer, err error) interactionAnswer {
	if err != nil {
		return interactionAnswer{Err: err}
	}
	switch {
	case ans.Declined:
		return interactionAnswer{Declined: true}
	case ans.Chat:
		return interactionAnswer{Text: ans.Text, Chat: true}
	case ans.Text != "":
		return interactionAnswer{Text: ans.Text} // a question with no options
	default:
		return interactionAnswer{Index: ans.Index}
	}
}

// result adapters map a settled answer onto each dialog signature.

func choiceResult(a interactionAnswer) (int, error) {
	if a.Declined {
		return 0, tui.ErrCancelled
	}
	if a.Err != nil {
		return 0, a.Err
	}
	return a.Index, nil
}

func multiResult(a interactionAnswer) ([]int, error) {
	if a.Declined {
		return nil, tui.ErrCancelled
	}
	if a.Err != nil {
		return nil, a.Err
	}
	return a.Indexes, nil
}

func textResult(a interactionAnswer) (string, error) {
	if a.Declined {
		return "", tui.ErrCancelled
	}
	if a.Err != nil {
		return "", a.Err
	}
	return a.Text, nil
}

func askResult(a interactionAnswer) (tui.Answer, error) {
	switch {
	case a.Declined:
		return tui.Answer{Declined: true}, nil // a declined ask is a normal result
	case a.Err != nil:
		return tui.Answer{}, a.Err
	case a.Chat:
		return tui.Answer{Text: a.Text, Chat: true}, nil
	case a.Text != "":
		return tui.Answer{Text: a.Text}, nil
	default:
		return tui.Answer{Index: a.Index}, nil
	}
}

func boolToInt(b bool) int {
	if b {
		return 0 // "Yes" leads the offered pair
	}
	return 1
}

// optionLabels lists a choice dialog's option labels, for its announcement.
func optionLabels(options []tui.Option) []string {
	out := make([]string, len(options))
	for i, o := range options {
		out[i] = o.Label
	}
	return out
}

// pickLabels lists a picker's row labels, for its announcement.
func pickLabels(items []tui.PickItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Label
	}
	return out
}
