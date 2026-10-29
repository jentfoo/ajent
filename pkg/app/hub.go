package app

import (
	"cmp"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/go-analyze/bulk"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

// draftDebounce coalesces editor drafts: one emission per pause in typing.
const draftDebounce = 100 * time.Millisecond

// activityUnranked mirrors the TUI's rank for rows whose producer keeps no
// order of its own: after every ranked row, ties in arrival order.
const activityUnranked = math.MaxInt

// eventRingSize bounds the retained event history an events.replay request
// reads. Anything older answers "too old" and the client re-snapshots.
const eventRingSize = 512

// hubModel is the model label copy the status line renders, with the
// capabilities a control client polls.
type hubModel struct {
	Key    string
	Short  string
	Window int
	Caps   llm.Capabilities
}

// hubTool is one toggleable /tools row as a client polls it.
type hubTool struct {
	Name     string
	Source   string
	Enabled  bool
	ReadOnly bool
}

// hubUsage is the ledger's numbers, the same terms /usage reports.
type hubUsage struct {
	Turns     int
	Total     llm.Usage
	Child     llm.Usage
	ByModel   map[string]llm.Usage
	Estimated int
}

// hubActivity is one keyed activity row, ranked as its producer declared.
type hubActivity struct {
	Key  string
	Text string
	Rank int
	seq  int // arrival order, tie-breaks equal ranks like the TUI's stable sort
}

// hubSnapshot is one consistent read of everything a control client polls.
// Each field comes from hub-held copies or its source's own lock, never from
// agent.State fields.
type hubSnapshot struct {
	SessionID   string
	SessionName string
	Head        string // transcript entry id appends land after
	Workspace   string
	Cwd         string
	Model       hubModel
	Permission  string // mode name, "" when no barrier exists
	Tools       []hubTool
	Usage       hubUsage
	Context     tokens.ContextState
	Running     bool
	Idle        bool
	Spinning    bool
	Queued      int
	QueuedTexts []string
	Activity    []hubActivity
	Status      tui.Status // the status row the local terminal renders
}

// hubEmitters are the per-surface observer hooks a control client attaches.
// Every field is optional, nil drops that surface, and each fires in the order
// the local surface committed the change.
type hubEmitters struct {
	Echo       func(text string)
	EchoImages func(imgs []tui.Image)
	Notice     func(key, msg string, level tui.Level) // key "" when unkeyed
	Print      func(markdown string)
	Segment    func(seg tui.Segment) // an empty Text reports a removal
	Model      func(key, short string, window int)
	Idle       func(idle bool)
	Spinning   func(on bool)
	Activity   func(key, text string, rank int)
	Queued     func(texts []string)
	Draft      func(text string)
	Head       func(id string) // the session head moved (rewind or fork)
}

// hubDeps is what the driver can supply at hub construction. Objects built
// later (tool registry, agent, barrier, steer queue, stager, /init controller,
// line router) are assigned onto the hub as they appear, always on the driver
// goroutine before the first loop that could race them.
type hubDeps struct {
	ui        *tui.UI
	dialogs   *interactionBroker
	active    llm.Model
	workspace string
	cwd       string
	acct      *tokens.Accounting
}

// sessionHub is the in-process control plane of one running session: every
// observable surface (turn events, TUI mutations, dialogs) and every control
// entry (submit, interrupt, mode cycle) crosses it, so an out-of-process client
// or the local terminal's control loop attaches here instead of reaching into
// the driver's locals.
type sessionHub struct {
	ui *hubUI

	submitLine func(line, origin string) // router entry: routes one line naming its submitter
	ag         *agent.Agent
	q          *steerQueue
	stager     *command.Stager
	barrier    barrierModeCycler // permission gate, a cycler when wired
	initCtl    *initController
	rewindTo   func(head string) (string, error) // rewind/fork onto a named entry, ending a plan workflow first
	tools      *tools.Registry
	workspace  string
	cwd        string

	sessionID   string // stable once the transcript opens
	sessionName string
	headID      string // last head seen, so a rewind event fires only on a real move
	// headFn reads the recorded transcript's live head. Installed at bind,
	// before any loop starts; nil when recording is off.
	headFn func() string

	mu        sync.Mutex
	sinks     []agent.Sink
	acct      *tokens.Accounting // last ledger captured under the agent state lock
	model     hubModel
	segments  map[string]tui.Segment
	queued    []string
	idle      bool
	spinning  bool
	activity  map[string]hubActivity
	actSeq    int // arrival order for activity rows tying on rank
	draftText string
	draftDue  *time.Timer
	emit      hubEmitters

	// control-plane event log: seq assigned once per observable change, the
	// ring feeding events.replay, and one delivery listener per connection
	evSeq  uint64
	evRing []ctl.Event
	evSubs []*evListener
}

// evListener is one registered event delivery func, address-identified so it
// can be dropped by handle.
type evListener struct{ deliver func(ctl.Event) }

// newSessionHub builds the control plane. emit is the driver-time observer set;
// attach before any loop starts.
func newSessionHub(deps hubDeps, emit hubEmitters) *sessionHub {
	h := &sessionHub{
		workspace: deps.workspace,
		cwd:       deps.cwd,
		acct:      deps.acct,
		idle:      true, // the prompt is at rest until a turn starts
		segments:  make(map[string]tui.Segment),
		activity:  make(map[string]hubActivity),
		emit:      emit,
	}
	if deps.active.ID != "" {
		// field set, not setModel: construction is initial state, not a change
		h.model = hubModel{Key: deps.active.Key(), Short: deps.active.ShortName(),
			Window: deps.active.ContextWindow, Caps: deps.active.Caps}
	}
	h.ui = &hubUI{UI: deps.ui, hub: h, dialogs: deps.dialogs}
	return h
}

// bindSession captures the session identity for snapshots: the root id once,
// the recorded name and head from the open-time state. Runs on the driver
// goroutine before any loop starts.
func (h *sessionHub) bindSession(rec *sessRec) {
	if rec == nil {
		return
	}
	h.mu.Lock()
	h.sessionID = sessionHint(rec)
	h.sessionName = session.NameOf(rec.snapshot.entries)
	if rec.w != nil {
		h.headID = rec.w.Head()
		h.headFn = rec.w.Head // live: snapshots never carry a stale head
	}
	h.mu.Unlock()
}

// setSessionName records a rename for snapshots.
func (h *sessionHub) setSessionName(name string) {
	h.mu.Lock()
	h.sessionName = name
	h.mu.Unlock()
}

// setHead records the transcript head after a rewind or fork moved it, emitting
// only on a real move so observers re-read the branch.
func (h *sessionHub) setHead(id string) {
	h.mu.Lock()
	if h.headID == id {
		h.mu.Unlock()
		return
	}
	h.headID = id
	f := h.emit.Head
	h.mu.Unlock()

	if f != nil {
		f(id)
	}
}

// Interactive reports whether a local terminal renders the session. Serve mode
// answers false: the control plane is the only front end.
func (h *sessionHub) Interactive() bool { return h.ui != nil && h.ui.UI != nil }

// sink returns the hub's turn-event observer, for the agent's sink list right
// after the recorder so fanout order matches what the local surface commits.
func (h *sessionHub) sink() agent.Sink { return hubSink{h: h} }

// Register adds a turn-event observer. Events fan out in registration order
// after the recorder; registering while a turn runs is safe.
func (h *sessionHub) Register(s agent.Sink) {
	if s == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sinks = append(h.sinks, s)
}

// observers snapshots the registered sinks. Dispatch runs without the hub lock,
// so an observer calling back into the hub never deadlocks.
func (h *sessionHub) observers() []agent.Sink {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sinks
}

// emitting snapshots the observer hooks, so each fires without the hub lock.
func (h *sessionHub) emitting() hubEmitters {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.emit
}

// setEmitters replaces the observer hooks. Chaining an existing set onto the
// new one is the caller's choice; the driver-time set is empty today.
func (h *sessionHub) setEmitters(emit hubEmitters) {
	h.mu.Lock()
	h.emit = emit
	h.mu.Unlock()
}

// publish records one observable change as a control event: seq assigned
// here, ring appended, then every attached connection's delivery func runs,
// still under the lock, so a connection's lines can never arrive out of seq
// order. Delivery is non-blocking by contract: a stage append or an
// asynchronous drop, so a slow or gone consumer never stalls the caller.
func (h *sessionHub) publish(name string, data any) {
	// encoded before the lock: stream deltas ride this path per sink call and
	// must not serialize behind every other hub surface
	raw := ctl.EncodeData(data)
	h.mu.Lock()
	h.evSeq++
	ev := ctl.Event{Name: name, Seq: h.evSeq, Data: raw}
	h.evRing = append(h.evRing, ev)
	if n := len(h.evRing) - eventRingSize; n > 0 {
		h.evRing = h.evRing[n:] // reslice off the front: no per-publish memmove
	}
	for _, l := range h.evSubs {
		l.deliver(ev)
	}
	h.mu.Unlock()
}

// evWatermark returns the last assigned seq. Request handlers read it before
// gathering their result, so events racing the read stay above the reply.
func (h *sessionHub) evWatermark() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.evSeq
}

// evListen registers one connection's delivery func, reporting the seq it is
// current through: events past it arrive via the listener.
func (h *sessionHub) evListen(f func(ctl.Event)) (*evListener, uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l := &evListener{deliver: f}
	h.evSubs = append(h.evSubs, l)
	return l, h.evSeq
}

// evUnlisten drops a listener by handle, tolerating a double drop.
func (h *sessionHub) evUnlisten(l *evListener) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.evSubs = slices.DeleteFunc(h.evSubs, func(x *evListener) bool { return x == l })
}

// eventsSince returns the retained events past since, false when since predates
// the ring and the client must re-snapshot instead.
func (h *sessionHub) eventsSince(since uint64) ([]ctl.Event, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.evRing) > 0 && since+1 < h.evRing[0].Seq {
		return nil, false
	}
	i, _ := slices.BinarySearchFunc(h.evRing, since+1,
		func(e ctl.Event, seq uint64) int { return cmp.Compare(e.Seq, seq) })
	return slices.Clone(h.evRing[i:]), true
}

// notice re-emits one notice line. key "" marks an unkeyed notice.
func (h *sessionHub) notice(key, msg string, level tui.Level) {
	if f := h.emitting().Notice; f != nil {
		f(key, msg, level)
	}
}

// print re-emits one markdown block, the form /help and /usage use.
func (h *sessionHub) print(markdown string) {
	if f := h.emitting().Print; f != nil {
		f(markdown)
	}
}

// echo re-emits one committed user message.
func (h *sessionHub) echo(text string) {
	if f := h.emitting().Echo; f != nil {
		f(text)
	}
}

// echoImages re-emits the images committed beside a prompt.
func (h *sessionHub) echoImages(imgs []tui.Image) {
	if len(imgs) == 0 {
		return
	}
	if f := h.emitting().EchoImages; f != nil {
		f(imgs)
	}
}

// setSegment remembers one keyed status segment, an empty Text removing it.
func (h *sessionHub) setSegment(seg tui.Segment) {
	h.mu.Lock()
	if seg.Text == "" {
		delete(h.segments, seg.Key)
	} else {
		h.segments[seg.Key] = seg
	}
	h.mu.Unlock()

	if f := h.emitting().Segment; f != nil {
		f(seg)
	}
}

// setModel remembers the full model copy, capabilities included.
func (h *sessionHub) setModel(m llm.Model) {
	h.mu.Lock()
	h.model = hubModel{Key: m.Key(), Short: m.ShortName(),
		Window: m.ContextWindow, Caps: m.Caps}
	h.mu.Unlock()

	if f := h.emitting().Model; f != nil {
		f(m.Key(), m.ShortName(), m.ContextWindow)
	}
}

// setModelLabel remembers a label-only change, the shape a raw SetModel call
// carries: the capabilities copy stays whatever the last full model left.
func (h *sessionHub) setModelLabel(key, short string, window int) {
	h.mu.Lock()
	h.model.Key, h.model.Short, h.model.Window = key, short, window
	h.mu.Unlock()

	if f := h.emitting().Model; f != nil {
		f(key, short, window)
	}
}

// setQueued remembers the pending-prompt rows, oldest first.
func (h *sessionHub) setQueued(texts []string) {
	h.mu.Lock()
	h.queued = slices.Clone(texts)
	h.mu.Unlock()

	if f := h.emitting().Queued; f != nil {
		f(slices.Clone(texts))
	}
}

// setIdle remembers the prompt's rest state.
func (h *sessionHub) setIdle(idle bool) {
	h.mu.Lock()
	h.idle = idle
	h.mu.Unlock()

	if f := h.emitting().Idle; f != nil {
		f(idle)
	}
}

// setActivity remembers one keyed activity row, an empty text removing it.
func (h *sessionHub) setActivity(key, text string, rank int) {
	h.mu.Lock()
	if text == "" {
		delete(h.activity, key)
	} else {
		h.actSeq++
		h.activity[key] = hubActivity{Key: key, Text: text, Rank: rank, seq: h.actSeq}
	}
	h.mu.Unlock()

	if f := h.emitting().Activity; f != nil {
		f(key, text, rank)
	}
}

// draft records the editor's live text, coalesced: rapid edits collapse to
// the latest. A secret-prefixed draft never emits, matching history exclusion.
func (h *sessionHub) draft(text string) {
	h.mu.Lock()
	h.draftText = text
	if h.draftDue == nil {
		h.draftDue = time.AfterFunc(draftDebounce, h.emitDraft)
	} else {
		h.draftDue.Reset(draftDebounce)
	}
	h.mu.Unlock()
}

// emitDraft publishes the coalesced draft once typing pauses.
func (h *sessionHub) emitDraft() {
	h.mu.Lock()
	h.draftDue = nil
	text := h.draftText
	h.mu.Unlock()

	if session.HasSecretPrefix(secretPrefix, text) {
		return
	}
	if f := h.emitting().Draft; f != nil {
		f(text)
	}
}

// prependDraft mirrors the editor's prepend onto the remembered draft, which
// host fills reach without a keystroke.
func (h *sessionHub) prependDraft(text string) {
	h.mu.Lock()
	cur := h.draftText
	h.mu.Unlock()

	if cur == "" {
		h.draft(text)
		return
	}
	h.draft(text + "\n" + cur)
}

// Submit routes one line into the session through the same router the editor
// uses: prompts, /commands, !shell and !! all behave exactly as typed, minus
// editor-coupled side effects. origin names the submitter ("controller") for
// transcript provenance; empty means the local editor. It reports whether the
// run had a router installed yet. Safe from any goroutine.
func (h *sessionHub) Submit(line, origin string) bool {
	h.mu.Lock()
	f := h.submitLine
	h.mu.Unlock()
	if f == nil {
		return false
	}
	f(line, origin)
	return true
}

// setSubmitLine installs the router entry Submit routes through. Runs once on
// the driver goroutine; control requests racing the wiring read the field
// under mu, never a torn write.
func (h *sessionHub) setSubmitLine(f func(line, origin string)) {
	h.mu.Lock()
	h.submitLine = f
	h.mu.Unlock()
}

// rewindEntry returns the installed rewind entry, nil while the run is still
// wiring one.
func (h *sessionHub) rewindEntry() func(head string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rewindTo
}

// setRewindTo installs the rewind entry, which ends an active plan workflow
// before switching branches. Same startup discipline as setSubmitLine.
func (h *sessionHub) setRewindTo(f func(head string) (string, error)) {
	h.mu.Lock()
	h.rewindTo = f
	h.mu.Unlock()
}

// barrierModeCycler is the permission-gate surface the hub reads: the real
// barrier, or a test spy reporting each cycle.
type barrierModeCycler interface {
	Mode() permit.Mode
	Cycle() permit.Mode
	Prev() permit.Mode
}

// interruptStop names what an interrupt stopped, for the hint the keypress
// path shows.
type interruptStop uint8

const (
	stopNothing interruptStop = iota
	stopTurn
	stopSurvey
	stopShell
)

// String names what an interrupt stopped, the control reply's term for it.
func (s interruptStop) String() string {
	switch s {
	case stopTurn:
		return "turn"
	case stopSurvey:
		return "survey"
	case stopShell:
		return "shell"
	default:
		return ""
	}
}

// Interrupt stops active work in priority order: a running turn (its queued
// prompts recover to the editor), an /init survey, then a staged shell
// command. It names what was stopped. Safe from any goroutine.
func (h *sessionHub) Interrupt() interruptStop {
	if h.ag != nil && abortRunning(h.ag, h.q, h.stager) {
		return stopTurn
	}
	if h.initCtl.abort() { // nil-safe: no survey, no stop
		return stopSurvey
	}
	if h.stager != nil && h.stager.Pending() {
		h.stager.Cancel()
		return stopShell
	}
	return stopNothing
}

// ModeCycle steps the permission mode and announces it, exactly like
// Shift+Tab (or its back direction). It returns the mode now in effect, the
// zero value when no gate exists. Safe from any goroutine.
func (h *sessionHub) ModeCycle(back bool) permit.Mode {
	if h.barrier == nil {
		return 0
	}
	var m permit.Mode
	if back {
		m = h.barrier.Prev()
	} else {
		m = h.barrier.Cycle() // re-evaluates any open dialog under the new mode
	}
	showPermissionMode(h.ui, m)
	h.ui.Notify("permissions mode: "+m.String(), tui.LevelInfo)
	return m
}

// Snapshot reads every polled field at one moment. Safe from any goroutine:
// hub-held copies plus each source's own lock.
func (h *sessionHub) Snapshot() hubSnapshot {
	h.mu.Lock()
	snap := hubSnapshot{
		SessionID:   h.sessionID,
		SessionName: h.sessionName,
		Workspace:   h.workspace,
		Cwd:         h.cwd,
		Model:       h.model,
		Idle:        h.idle,
		Spinning:    h.spinning,
		QueuedTexts: slices.Clone(h.queued),
		Activity:    activityRows(h.activity),
	}
	if h.headFn != nil {
		snap.Head = h.headFn() // live writer truth; the copy only gates head events
	} else {
		snap.Head = h.headID
	}
	segs := statusSegments(h.segments)
	acct := h.acct
	h.mu.Unlock()

	// guarded sources read without the hub lock: their owners may call back in
	if h.q != nil {
		snap.Queued = h.q.pending()
	}
	if h.ag != nil {
		snap.Running = h.ag.Running()
	} else {
		snap.Running = snap.Spinning
	}
	if h.barrier != nil {
		snap.Permission = h.barrier.Mode().String()
	}
	if h.tools != nil {
		snap.Tools = toolSnapshot(h.tools)
	}
	if t := h.ledger(acct); t != nil {
		snap.Context = t.Context()
		snap.Usage = hubUsage{
			Turns:     t.TurnsCount(),
			Total:     t.Total(),
			Child:     t.ChildTotal(),
			ByModel:   t.ByModel(),
			Estimated: t.EstimatedTurns(),
		}
	}
	snap.Status = tui.Status{
		Model:      snap.Model.Key,
		ModelShort: snap.Model.Short,
		MaxTokens:  snap.Model.Window,
		Tokens:     snap.Context.Used,
		Reserve:    snap.Context.Reserve,
		Compact:    snap.Context.Compact,
		Estimated:  snap.Context.Estimated,
		Segments:   segs,
	}
	if snap.Spinning {
		snap.Status.Spinner = tui.SpinnerFrame(true)
	}
	return snap
}

// ledger returns the accounting to read numbers from. While the agent is idle
// the pointer is recaptured under the agent state lock; mid-turn the last
// capture serves, since a turn never replaces the ledger, only mutates it
// under its own lock.
func (h *sessionHub) ledger(cached *tokens.Accounting) *tokens.Accounting {
	var t *tokens.Accounting
	if h.ag != nil && h.ag.WithState(func(st *agent.State) { t = st.Tokens }) {
		h.mu.Lock()
		h.acct = t
		h.mu.Unlock()
		return t
	}
	return cached
}

// syncLedger recaptures the ledger after a state switch (rewind, fork,
// compaction) replaced it, so snapshots keep reading the live one.
func (h *sessionHub) syncLedger() {
	if h.ag == nil {
		return
	}
	var t *tokens.Accounting
	if h.ag.WithState(func(st *agent.State) { t = st.Tokens }) && t != nil {
		h.mu.Lock()
		h.acct = t
		h.mu.Unlock()
	}
}

// toolSnapshot reads the registry as /tools does: one row per toggleable unit,
// enabled when every member is on, read-only when every member is marked.
func toolSnapshot(reg *tools.Registry) []hubTool {
	rows := reg.Units(reg.All())
	enabled := bulk.SliceToSet(reg.Names())
	out := make([]hubTool, 0, len(rows))
	for _, row := range rows {
		out = append(out, hubTool{
			Name:     row.Name,
			Source:   row.Source,
			Enabled:  allPresent(row.Names, enabled),
			ReadOnly: allReadOnly(reg, row.Names),
		})
	}
	return out
}

func allPresent(names []string, enabled map[string]struct{}) bool {
	return !slices.ContainsFunc(names, func(n string) bool {
		_, ok := enabled[n]
		return !ok
	})
}

func allReadOnly(reg *tools.Registry, names []string) bool {
	return !slices.ContainsFunc(names, func(n string) bool { return !reg.ReadOnly(n) })
}

// activityRows renders the remembered activity rows in display order.
func activityRows(rows map[string]hubActivity) []hubActivity {
	out := bulk.MapValuesSlice(rows)
	slices.SortStableFunc(out, func(a, b hubActivity) int {
		if c := cmp.Compare(a.Rank, b.Rank); c != 0 {
			return c
		}
		return cmp.Compare(a.seq, b.seq)
	})
	return out
}

// statusSegments renders the remembered segments in display order: Order then
// key, stable so a snapshot never reorders equal-order segments.
func statusSegments(segs map[string]tui.Segment) []tui.Segment {
	out := bulk.MapValuesSlice(segs)
	slices.SortFunc(out, func(a, b tui.Segment) int {
		if c := cmp.Compare(a.Order, b.Order); c != 0 {
			return c
		}
		return cmp.Compare(a.Key, b.Key)
	})
	return out
}

// hubSink is the hub's observer of turn events: it tracks spinner state from
// turn boundaries and fans every event out to registered observers.
type hubSink struct {
	h *sessionHub
}

func (s hubSink) TurnStart(i agent.TurnInfo) {
	s.h.setSpinning(true)
	for _, o := range s.h.observers() {
		o.TurnStart(i)
	}
}

func (s hubSink) TurnEnd(r agent.TurnResult) {
	s.h.setSpinning(false)
	for _, o := range s.h.observers() {
		o.TurnEnd(r)
	}
}

func (s hubSink) UserPrompt(text string) {
	for _, o := range s.h.observers() {
		o.UserPrompt(text)
	}
}

func (s hubSink) Thinking(d string) {
	for _, o := range s.h.observers() {
		o.Thinking(d)
	}
}

func (s hubSink) EndThinking() {
	for _, o := range s.h.observers() {
		o.EndThinking()
	}
}

func (s hubSink) Text(d string) {
	for _, o := range s.h.observers() {
		o.Text(d)
	}
}

func (s hubSink) EndText() {
	for _, o := range s.h.observers() {
		o.EndText()
	}
}

// ToolStart returns a closure calling every observer's done in order.
func (s hubSink) ToolStart(call agent.ToolCall, label string, full bool) func(agent.ToolResult) {
	done := make([]func(agent.ToolResult), 0)
	for _, o := range s.h.observers() {
		if d := o.ToolStart(call, label, full); d != nil {
			done = append(done, d)
		}
	}
	return func(r agent.ToolResult) {
		for _, d := range done {
			d(r)
		}
	}
}

func (s hubSink) ToolOutput(id, d string) {
	for _, o := range s.h.observers() {
		o.ToolOutput(id, d)
	}
}

func (s hubSink) ToolProgress(p agent.ToolProgress) {
	for _, o := range s.h.observers() {
		o.ToolProgress(p)
	}
}

func (s hubSink) Diff(p, b, a string) {
	for _, o := range s.h.observers() {
		o.Diff(p, b, a)
	}
}

func (s hubSink) Usage(u llm.Usage) {
	for _, o := range s.h.observers() {
		o.Usage(u)
	}
}

func (s hubSink) Context(c tokens.ContextState) {
	for _, o := range s.h.observers() {
		o.Context(c)
	}
}

func (s hubSink) Notice(msg string, level agent.Level) {
	for _, o := range s.h.observers() {
		o.Notice(msg, level)
	}
}

// setSpinning tracks the working glyph from turn boundaries and re-emits it.
func (h *sessionHub) setSpinning(on bool) {
	h.mu.Lock()
	changed := h.spinning != on
	h.spinning = on
	h.mu.Unlock()

	if changed {
		if f := h.emitting().Spinning; f != nil {
			f(on)
		}
	}
}
