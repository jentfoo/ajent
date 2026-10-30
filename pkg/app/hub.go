package app

import (
	"cmp"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"sync"

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
	tuisink "github.com/jentfoo/ajent/pkg/tui/sink"
)

// submit refusals the wire maps to distinct codes.
var (
	errSubmitStarting  = errors.New("the run is still starting")
	errPromptQueueFull = errors.New("prompt queue full")
)

// activityUnranked mirrors the TUI's rank for rows whose producer keeps no
// order of its own: after every ranked row, ties in arrival order.
const activityUnranked = math.MaxInt

// hubModel is the model label copy the status line renders, with the
// capabilities a control client polls.
type hubModel struct {
	Key    string
	Short  string
	Window int
	Caps   llm.Capabilities
}

// viewModel is the hub's authoritative copy of every ephemeral presentation
// field: the status segments, activity rows, queue rows, idle/turn state, model
// labels, context numbers, permission mode and tool rows. Every mutation
// updates it and fans out its event in one critical section, so
// a snapshot reads one internally consistent state under the same lock that
// guards the event seq. Committed history, streaming buffers, editor internals
// and dialog arbitration stay with their own owners, so only this surface lives
// here. Callers outside the hub mutate it only through the hubUI shim.
type viewModel struct {
	segments   map[string]tui.Segment
	activity   map[string]hubActivity
	actSeq     int // arrival order for activity rows tying on rank
	queued     []ctl.QueueRow
	idle       bool
	spinning   bool
	running    bool // a turn is in flight, tracked from the turn boundary events
	model      hubModel
	context    tokens.ContextState
	permission string // mode name, "" when no barrier exists
	tools      []hubTool
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
// Every field but Head, Usage and Interactions is a view model copy read under
// the hub lock. Head rides the writer's own head func in that same section,
// and Usage and Interactions are the two documented read-after-unlock sources.
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
	QueuedRows  []ctl.QueueRow
	Activity    []hubActivity
	Status      tui.Status // the status row the local terminal renders
	// Interactions lists every still-open dialog oldest first: state that holds
	// the run, so a snapshot must carry it the way queue rows and activity do.
	Interactions []interactionInfo
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

	submitLine func(line, queueID, origin string) error // router entry: routes one line naming its submitter
	ag         *agent.Agent
	q          *steerQueue
	stager     *command.Stager
	barrier    barrierModeCycler // permission gate, a cycler when wired
	initCtl    *initController
	rewindTo   func(head, cause string) (string, error) // rewind/fork to an entry; ends a plan first
	tools      *tools.Registry                          // the registry the view model's tool rows mirror
	workspace  string
	cwd        string

	sessionID   string // stable once the transcript opens
	sessionName string
	headID      string // last head seen, so a rewind event fires only on a real move
	// renameObs observes session renames (the control registry rewrite); nil
	// drops. Installed before any loop starts, like every other seam.
	renameObs func()
	// headFn reads the recorded transcript's live head. Installed at bind,
	// before any loop starts; nil when recording is off.
	headFn func() string

	mu    sync.Mutex
	sinks []agent.Sink
	acct  *tokens.Accounting // last ledger captured under the agent state lock
	vm    viewModel          // the ephemeral view model: mutated and read beside evSeq

	// control-plane event log: one seq space, one delivery listener per
	// connection. Committed content reconciles by entry id, so nothing needs
	// retained events past the live fanout.
	evSeq  uint64
	evSubs []*evListener
}

// evListener is one registered event delivery func, address-identified so it
// can be dropped by handle.
type evListener struct{ deliver func(ctl.Event) }

// newSessionHub builds the control plane. Construction is initial state, not a
// change, so nothing publishes here.
func newSessionHub(deps hubDeps) *sessionHub {
	h := &sessionHub{
		workspace: deps.workspace,
		cwd:       deps.cwd,
		acct:      deps.acct,
	}
	h.vm.idle = true // the prompt is at rest until a turn starts
	h.vm.segments = make(map[string]tui.Segment)
	h.vm.activity = make(map[string]hubActivity)
	if deps.active.ID != "" {
		// field set, not setModel: construction is initial state, not a change
		h.vm.model = hubModel{Key: deps.active.Key(), Short: deps.active.ShortName(),
			Window: deps.active.ContextWindow, Caps: deps.active.Caps}
	}
	h.ui = &hubUI{UI: deps.ui, hub: h, dialogs: deps.dialogs}
	if deps.ui != nil {
		// chrome paints through the shared applier, so the local terminal renders
		// its own mutations with the same mapping an attached terminal applies to
		// the wire events this hub publishes
		h.ui.ap = newApplier(deps.ui, tuisink.New(deps.ui), applyHooks{})
	}
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

// setSessionName records a rename, publishing it so controllers and pickers
// learn renames without polling, and running the rename observer (the control
// registry rewrite) outside the lock.
func (h *sessionHub) setSessionName(name string) {
	h.mu.Lock()
	changed := h.sessionName != name
	h.sessionName = name
	var obs func()
	if changed {
		raw := ctl.EncodeData(ctl.SessionRenamedData{Name: name})
		h.publishLocked(ctl.EventSessionRenamed, raw)
		obs = h.renameObs
	}
	h.mu.Unlock()
	if obs != nil {
		obs()
	}
}

// setHead records the transcript head after a rewind or fork moved it,
// publishing branch.changed with the mutation under one critical section. It
// fires only on a real move: branch.changed plus entry.append together define
// committed state, and no other event may move a head.
func (h *sessionHub) setHead(id, cause string) {
	h.mu.Lock()
	if h.headID == id {
		h.mu.Unlock()
		return
	}
	from := h.headID
	h.headID = id
	raw := ctl.EncodeData(ctl.BranchChangedData{From: from, Head: id, Cause: cause})
	h.publishLocked(ctl.EventBranchChanged, raw)
	h.mu.Unlock()
}

// noteAppend records the head an appended entry moved the branch to, so a later
// rewind publishes branch.changed against the writer's real head instead of the
// last switch's. Appends chain from the head, so the newest entry id is it.
func (h *sessionHub) noteAppend(id string) {
	if id == "" {
		return
	}
	h.mu.Lock()
	h.headID = id
	h.mu.Unlock()
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

// publish records one observable change as a control event. Seq assignment and
// delivery share one critical section, so a connection's lines can never arrive
// out of seq order and commit order equals seq order by construction. Delivery
// is non-blocking by contract: a stage append or an asynchronous drop, so a
// slow or gone consumer never stalls the caller.
func (h *sessionHub) publish(name string, data any) {
	// encoded before the lock: stream deltas ride this path per sink call and
	// must not serialize behind every other hub surface
	raw := ctl.EncodeData(data)
	h.mu.Lock()
	h.publishLocked(name, raw)
	h.mu.Unlock()
}

// publishLocked stamps and delivers one encoded event. Caller holds h.mu; a
// mutation and its publish must share the caller's one critical section so the
// snapshot can never name a state that never existed.
func (h *sessionHub) publishLocked(name string, raw json.RawMessage) {
	h.evSeq++
	ev := ctl.Event{Name: name, Seq: h.evSeq, Data: raw}
	for _, l := range h.evSubs {
		l.deliver(ev)
	}
}

// evWatermark returns the last assigned seq.
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

// notice publishes one transient notice line, key "" marking an unkeyed one.
// Recorded notices ride entry.append instead, so there is no duplicate to
// dedupe; this family is never reconciled against the file.
func (h *sessionHub) notice(key, msg string, level tui.Level) {
	h.publish(ctl.EventNotice, ctl.NoticeData{Text: msg,
		Level: levelName(agent.Level(level)), Key: key})
}

// print publishes one markdown block, the form /help and /usage use.
func (h *sessionHub) print(markdown string) {
	h.publish(ctl.EventPrint, ctl.PrintData{Markdown: markdown})
}

// setSegment remembers one keyed status segment in the view model, an empty
// Text removing it, publishing the change in the same critical section.
func (h *sessionHub) setSegment(seg tui.Segment) {
	h.mu.Lock()
	if seg.Text == "" {
		delete(h.vm.segments, seg.Key)
	} else {
		h.vm.segments[seg.Key] = seg
	}
	h.publishLocked(ctl.EventStatus, ctl.EncodeData(ctl.StatusData{Segment: &ctl.Segment{
		Key: seg.Key, Text: seg.Text, Short: seg.Short, Order: seg.Order,
	}}))
	h.mu.Unlock()
}

// setModel remembers the full model copy, capabilities included, and publishes
// the label change in the same critical section.
func (h *sessionHub) setModel(m llm.Model) {
	h.mu.Lock()
	h.vm.model = hubModel{Key: m.Key(), Short: m.ShortName(),
		Window: m.ContextWindow, Caps: m.Caps}
	h.publishModelLocked(h.vm.model)
	h.mu.Unlock()
}

// setModelLabel remembers a label-only change, the shape a raw SetModel call
// carries: the capabilities copy stays whatever the last full model left.
func (h *sessionHub) setModelLabel(key, short string, window int) {
	h.mu.Lock()
	h.vm.model.Key, h.vm.model.Short, h.vm.model.Window = key, short, window
	h.publishModelLocked(h.vm.model)
	h.mu.Unlock()
}

// publishModelLocked stamps one model change. The event carries the full state
// a snapshot's Model field carries, caps included, so a capability change never
// needs a resnapshot to reach a controller. Caller holds h.mu.
func (h *sessionHub) publishModelLocked(m hubModel) {
	h.publishLocked(ctl.EventStatus, ctl.EncodeData(ctl.StatusData{Model: &ctl.ModelState{
		Key: m.Key, Short: m.Short, Window: m.Window,
		Caps: ctl.Caps{Reasoning: m.Caps.Reasoning, Images: m.Caps.Images,
			ParallelToolCalls: m.Caps.ParallelTools},
	}}))
}

// setQueued remembers the pending-prompt rows, oldest first, the view model's
// queued count being their length. Secret-prefixed rows lose their text on the
// wire but keep their id, matching the draft rule; the local surface already
// rendered the real texts before this ran.
func (h *sessionHub) setQueued(rows []ctl.QueueRow) {
	h.mu.Lock()
	h.vm.queued = slices.Clone(rows)
	h.publishLocked(ctl.EventQueued, ctl.EncodeData(ctl.QueuedData{
		Rows: redactQueueRows(rows),
	}))
	h.mu.Unlock()
}

// queueDroppedCause announces queued prompts a removal cleared, so their
// pending rows resolve instead of vanishing. The cause names what removed them:
// an interrupt recovery, a rewind clear, or a recall back into an editor.
func (h *sessionHub) queueDroppedCause(cause string, rows []ctl.QueueRow) {
	h.publish(ctl.EventQueueDropped,
		ctl.QueueDroppedData{Rows: redactQueueRows(rows), Cause: cause})
}

// setIdle remembers the prompt's rest state and publishes the flip, so
// controllers learn the at-rest fact (what local keys mean) without polling.
func (h *sessionHub) setIdle(idle bool) {
	h.mu.Lock()
	if h.vm.idle != idle {
		h.vm.idle = idle
		h.publishLocked(ctl.EventIdle, ctl.EncodeData(ctl.IdleData{Idle: idle}))
	}
	h.mu.Unlock()
}

// setActivity remembers one keyed activity row in the view model, an empty text
// removing it, publishing in the same critical section.
func (h *sessionHub) setActivity(key, text string, rank int) {
	h.mu.Lock()
	if text == "" {
		delete(h.vm.activity, key)
	} else {
		h.vm.actSeq++
		h.vm.activity[key] = hubActivity{Key: key, Text: text, Rank: rank, seq: h.vm.actSeq}
	}
	h.publishLocked(ctl.EventActivity, ctl.EncodeData(ctl.ActivityData{Key: key, Text: text, Rank: rank}))
	h.mu.Unlock()
}

// setContext remembers the context bar's numbers and publishes them in the same
// critical section: the hub is the event's publisher, the stream sink only
// fans the call out to observers.
func (h *sessionHub) setContext(c tokens.ContextState) {
	h.mu.Lock()
	h.vm.context = c
	h.publishLocked(ctl.EventContext, ctl.EncodeData(ctl.ContextData{
		Used: c.Used, Window: c.Window, Reserve: c.Reserve, Compact: c.Compact, Estimated: c.Estimated,
	}))
	h.mu.Unlock()
}

// setPermissionMode remembers the barrier's mode name and publishes it in the
// same critical section. Only reached with a live barrier, so the name is
// never empty here. No barrier at all leaves the zero value for snapshots.
func (h *sessionHub) setPermissionMode(m permit.Mode) {
	h.mu.Lock()
	h.vm.permission = m.String()
	h.publishLocked(ctl.EventPermission, ctl.EncodeData(ctl.PermissionData{Mode: m.String()}))
	h.mu.Unlock()
}

// setToolsRegistry installs the registry the tool rows mirror. Its change hook
// republishes the rows on every reshape, so toggles, MCP connects and plan
// scopes all reach the view model from their own mutation sites.
func (h *sessionHub) setToolsRegistry(reg *tools.Registry) {
	h.mu.Lock()
	h.tools = reg
	h.mu.Unlock()
	if reg == nil {
		return
	}
	reg.OnChanged(h.refreshTools)
	h.refreshTools()
}

// refreshTools re-reads the registry into the view model and publishes the
// rows. Runs on the registry's change hook, so never under the registry lock.
func (h *sessionHub) refreshTools() {
	h.mu.Lock()
	reg := h.tools
	h.mu.Unlock()
	if reg == nil {
		return
	}
	rows := toolSnapshot(reg)
	h.mu.Lock()
	h.vm.tools = rows
	h.publishLocked(ctl.EventTools, ctl.EncodeData(ctl.ToolsData{Rows: ctlTools(rows)}))
	h.mu.Unlock()
}

// Submit routes one line into the session through the same router the editor
// uses: prompts, /commands, !shell and !! all behave exactly as typed, minus
// editor-coupled side effects. queueID carries a submitting surface's pending
// correlation onto the prompt's queue row and committed entry; empty mints one.
// origin names the submitter ("controller") for transcript provenance; empty
// means the local editor. It reports errSubmitStarting before the router is
// wired and the router's own error otherwise, which is errPromptQueueFull when
// the pump queue is full: a remote submit never blocks the caller. Safe from
// any goroutine.
func (h *sessionHub) Submit(line, queueID, origin string) error {
	h.mu.Lock()
	f := h.submitLine
	h.mu.Unlock()
	if f == nil {
		return errSubmitStarting
	}
	return f(line, queueID, origin)
}

// setSubmitLine installs the router entry Submit routes through. Runs once on
// the driver goroutine; control requests racing the wiring read the field
// under mu, never a torn write.
func (h *sessionHub) setSubmitLine(f func(line, queueID, origin string) error) {
	h.mu.Lock()
	h.submitLine = f
	h.mu.Unlock()
}

// rewindEntry returns the installed rewind entry, nil while the run is still
// wiring one.
func (h *sessionHub) rewindEntry() func(head, cause string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rewindTo
}

// setRewindTo installs the rewind entry, which ends an active plan workflow
// before switching branches. Same startup discipline as setSubmitLine.
func (h *sessionHub) setRewindTo(f func(head, cause string) (string, error)) {
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
	setPermissionMode(h.ui, m)
	h.ui.Notify("permissions mode: "+m.String(), tui.LevelInfo)
	return m
}

// Snapshot reads every polled field at one moment. Safe from any goroutine:
// hub-held copies plus each source's own lock.
func (h *sessionHub) Snapshot() hubSnapshot {
	snap, _ := h.SnapshotAt()
	return snap
}

// SnapshotAt is Snapshot also returning the event watermark the read was taken
// at, captured under the same critical section as the head and every view
// model field, so the reply is one internally consistent state. The reply's
// watermark is that seq, which is what closes the watermark-versus-head
// window: an entry.append naming this head can never sit below the seq a
// client already reconciled.
func (h *sessionHub) SnapshotAt() (hubSnapshot, uint64) {
	h.mu.Lock()
	seq := h.evSeq
	vm := h.vm
	snap := hubSnapshot{
		SessionID:   h.sessionID,
		SessionName: h.sessionName,
		Workspace:   h.workspace,
		Cwd:         h.cwd,
		Model:       vm.model,
		Permission:  vm.permission,
		Tools:       vm.tools,
		Context:     vm.context,
		Running:     vm.running,
		Idle:        vm.idle,
		Spinning:    vm.spinning,
		Queued:      len(vm.queued),
		QueuedRows:  redactQueueRows(vm.queued),
		Activity:    activityRows(vm.activity),
	}
	if h.headFn != nil {
		snap.Head = h.headFn() // live writer truth; the copy only gates head events
	} else {
		snap.Head = h.headID
	}
	segs := statusSegments(vm.segments)
	acct := h.acct
	h.mu.Unlock()

	// the two sources that cannot join the hub lock read after it: the ledger
	// rides the agent's state lock (WithState), the broker's begin holds its own
	// lock across its publish. Both may name a moment slightly newer than seq,
	// which the watermark rule already supersedes.
	if b := h.ui.dialogs; b != nil {
		snap.Interactions = b.opened()
	}
	if t := h.ledger(acct); t != nil {
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
	return snap, seq
}

// ledger returns the accounting to read numbers from. While the agent is idle
// the pointer is recaptured under the agent state lock; mid-turn (or without an
// agent) the last capture serves, since a turn never replaces the ledger, only
// mutates it under its own lock.
func (h *sessionHub) ledger(cached *tokens.Accounting) *tokens.Accounting {
	var t *tokens.Accounting
	if h.ag != nil && h.ag.WithState(func(st *agent.State) { t = st.Tokens }) && t != nil {
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
	_ = h.ledger(nil)
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
			Enabled:  tools.AllPresent(row.Names, enabled),
			ReadOnly: allReadOnly(reg, row.Names),
		})
	}
	return out
}

// allReadOnly reports whether every name is marked read-only.
func allReadOnly(reg *tools.Registry, names []string) bool {
	return !slices.ContainsFunc(names, func(n string) bool { return !reg.ReadOnly(n) })
}

// redactQueueRows strips the text of secret-prefixed rows on a wire copy,
// matching the draft rule: the value never leaves the process. The id stays,
// so the submitting surface still resolves its pending row on a drop or commit.
func redactQueueRows(rows []ctl.QueueRow) []ctl.QueueRow {
	out := make([]ctl.QueueRow, len(rows))
	for i, r := range rows {
		if session.HasSecretPrefix(secretPrefix, r.Text) {
			r.Text = "" // the id keeps resolving the row
		}
		out[i] = r
	}
	return out
}

// queueRowTexts renders wire rows for a local surface, which shows the real
// texts including secrets.
func queueRowTexts(rows []ctl.QueueRow) []string {
	texts := make([]string, len(rows))
	for i, r := range rows {
		texts[i] = r.Text
	}
	return texts
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
	s.h.setTurnLive(true)
	for _, o := range s.h.observers() {
		o.TurnStart(i)
	}
}

func (s hubSink) TurnEnd(r agent.TurnResult) {
	s.h.setTurnLive(false)
	for _, o := range s.h.observers() {
		o.TurnEnd(r)
	}
}

func (s hubSink) UserPrompt(text string) {
	for _, o := range s.h.observers() {
		o.UserPrompt(text)
	}
}

// InjectContext fans the steering echo out to observers. The wire sink drops
// it: committed entries are the only injected-content form on the socket, and
// every attached surface paints each one exactly once through the echo rule.
func (s hubSink) InjectContext(text string) {
	for _, o := range s.h.observers() {
		o.InjectContext(text)
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

// Context records the bar's numbers on the view model, then fans the call out;
// the hub owns the event, so observers republish nothing.
func (s hubSink) Context(c tokens.ContextState) {
	s.h.setContext(c)
	for _, o := range s.h.observers() {
		o.Context(c)
	}
}

func (s hubSink) Notice(msg string, level agent.Level) {
	for _, o := range s.h.observers() {
		o.Notice(msg, level)
	}
}

// setTurnLive tracks a turn's boundary: whether one is in flight and whether
// the working glyph spins, publishing the spinner's flip in the same critical
// section. The running fact itself rides the turn events, not a dedicated one.
func (h *sessionHub) setTurnLive(live bool) {
	h.mu.Lock()
	h.vm.running = live
	if h.vm.spinning != live {
		h.vm.spinning = live
		h.publishLocked(ctl.EventStatus, ctl.EncodeData(ctl.StatusData{Spinning: &live}))
	}
	h.mu.Unlock()
}
