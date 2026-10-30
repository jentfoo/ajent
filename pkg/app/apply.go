package app

import (
	"strings"
	"sync"

	"github.com/go-analyze/bulk"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tui"
	tuisink "github.com/jentfoo/ajent/pkg/tui/sink"
)

// eventSurface is the terminal face control events paint: exactly the setters a
// payload may reach, so what renders on every front end is declared here and
// the local-only faces (editor drafts, dialog arbitration, recall stores,
// theme, scrollback) stay off the shared path. *tui.UI satisfies it.
type eventSurface interface {
	Notify(msg string, level tui.Level)
	NotifyKeyed(key, msg string, level tui.Level)
	Print(markdown string)
	SetStatusSegment(seg tui.Segment)
	SetModel(name, short string, maxTokens int)
	SetActivityRanked(key, text string, rank int)
	SetQueued(rows []string)
	SetIdle(idle bool)
}

// applyHooks carries the per-surface callbacks behind event families whose
// other half is local state. The shared path never touches a draft, answers a
// dialog or resolves a pending row on its own; it hands those to the one front
// end that owns them.
type applyHooks struct {
	// queueRows maps one wire copy of the queue onto this surface's display
	// rows. Attach remembers the copy and merges its own pending submissions;
	// the host shows the real texts.
	queueRows func(rows []ctl.QueueRow) []string
	// dropped reports prompts a removal cleared, so a submitting surface
	// resolves its pending rows by id and names each outcome.
	dropped func(d ctl.QueueDroppedData)
	// running tracks whether a turn is in flight beyond SetIdle: Esc keeps its
	// interrupt role while one runs, and a reconnect knows what to restore.
	running func(live bool)
	// hint routes the hint segment through the surface's own arbitration when
	// set: attach shares one board between the host's wire hints and this
	// terminal's local claims, so the two never overwrite each other. Empty
	// text releases the claim.
	hint func(text, short string)
	// dialogPending/dialogSettled route open dialogs to the surface's own
	// arbitration; nothing shared races a human for an answer.
	dialogPending func(d ctl.InteractionPendingData)
	dialogSettled func(id uint64)
}

// applier maps control-plane payloads onto one terminal surface: the single
// projection every front end renders through. Stream events ride tui/sink's
// mapping, so there is one tool-result fallback; chrome families forward to the
// eventSurface; queue rows and dialogs merge through hooks. A new keyed row
// renders on both surfaces with three touches: a view-model field, its event
// payload, and one case in Apply.
//
// The host feeds mutations at publish time through the typed methods, on the
// same goroutine as before, so chrome keeps its paint order beside the stream
// events the agent sinks deliver directly. Attach applies whole wire events via
// Apply once the watermark admits them.
type applier struct {
	surf  eventSurface
	sink  *tuisink.Sink
	hooks applyHooks

	mu sync.Mutex // guards the keyed tracking below, which concurrent host shim writes race for

	turnLive bool                // a turn is in flight by this surface's accounting
	segs     map[string]struct{} // segments painted here, for snapshot reconcile
	acts     map[string]struct{} // activity rows likewise
	calls    map[string]*apCall  // in-flight tool calls by id
}

// apCall is one in-flight tool call's client-side state: the header hook whose
// result commits the body, and whether deltas already rendered so the wire's
// full-text copy must not repeat them.
type apCall struct {
	done     func(agent.ToolResult)
	streamed bool
}

// newApplier applies control payloads to surf's terminal face; sink renders
// the stream families. Hooks may be partly filled: a nil hook skips its
// family, which is how the host supplies none (its chrome never carries the
// local halves).
func newApplier(surf eventSurface, sink *tuisink.Sink, hooks applyHooks) *applier {
	return &applier{
		surf:  surf,
		sink:  sink,
		hooks: hooks,
		segs:  make(map[string]struct{}),
		acts:  make(map[string]struct{}),
		calls: make(map[string]*apCall),
	}
}

// Apply maps one control event onto the surface. Stream events mirror the host
// renderer's own sink calls one to one; chrome families forward to the setters
// that produced them.
func (a *applier) Apply(ev ctl.Event) {
	switch ev.Name {
	// --- stream events, through the shared sink mapping ---

	case ctl.EventTurnStart:
		a.sink.TurnStart(agent.TurnInfo{})
		a.setRunning(true)
	case ctl.EventThinkingDelta:
		var d ctl.TextData
		if ev.Decode(&d) == nil {
			a.sink.Thinking(d.Delta)
		}
	case ctl.EventThinkingEnd:
		a.sink.EndThinking()
	case ctl.EventTextDelta:
		var d ctl.TextData
		if ev.Decode(&d) == nil {
			a.sink.Text(d.Delta)
		}
	case ctl.EventTextEnd:
		a.sink.EndText()
	case ctl.EventToolStart:
		var d ctl.ToolStartData
		if ev.Decode(&d) != nil {
			return
		}
		done := a.sink.ToolStart(agent.ToolCall{ID: d.ID, Name: d.Name}, d.Label, d.Full)
		a.callFor(d.ID).done = done
	case ctl.EventToolOutput:
		var d ctl.ToolOutputData
		if ev.Decode(&d) == nil {
			a.callFor(d.ID).streamed = true // deltas render below; the result must not repeat them
			a.sink.ToolOutput(d.ID, d.Delta)
		}
	case ctl.EventToolProgress:
		var d ctl.ToolProgressData
		if ev.Decode(&d) != nil {
			return
		}
		a.sink.ToolProgress(agent.ToolProgress{CallID: d.ID, Name: d.Name,
			Path: d.Path, Bytes: d.Bytes, Lines: d.Lines, Done: d.Done})
	case ctl.EventToolResult:
		var d ctl.ToolResultData
		if ev.Decode(&d) == nil {
			a.toolResult(d)
		}
	case ctl.EventDiff:
		var d ctl.DiffData
		if ev.Decode(&d) == nil {
			a.sink.Diff(d.Path, d.Before, d.After)
		}
	case ctl.EventContext:
		var d ctl.ContextData
		if ev.Decode(&d) == nil {
			a.context(wireContext(d))
		}
	case ctl.EventTurnEnd:
		a.sink.TurnEnd(agent.TurnResult{})
		a.setRunning(false)
		a.dropCalls() // calls whose result never landed keep nothing behind

	// --- chrome, through the surface setters ---

	case ctl.EventNotice:
		var d ctl.NoticeData
		if ev.Decode(&d) == nil {
			a.notice(d.Key, d.Text, wireLevel(d.Level))
		}
	case ctl.EventPrint:
		var d ctl.PrintData
		if ev.Decode(&d) == nil {
			a.print(d.Markdown)
		}
	case ctl.EventStatus:
		var d ctl.StatusData
		if ev.Decode(&d) != nil {
			return
		}
		// spinning is ignored live: turn events carry the same fact through the
		// sink; only a snapshot reconciles it, since a drop can lose the end
		switch {
		case d.Segment != nil:
			// placement comes from this build's spec table, so equal keys collapse alike
			a.segment(segment(d.Segment.Key, d.Segment.Text, d.Segment.Short))
		case d.Model != nil:
			a.model(d.Model.Key, d.Model.Short, d.Model.Window)
		}
	case ctl.EventActivity:
		var d ctl.ActivityData
		if ev.Decode(&d) == nil {
			a.activity(d.Key, d.Text, d.Rank)
		}
	case ctl.EventQueued:
		var d ctl.QueuedData
		if ev.Decode(&d) == nil {
			a.queued(d.Rows)
		}
	case ctl.EventQueueDropped:
		var d ctl.QueueDroppedData
		if ev.Decode(&d) == nil && a.hooks.dropped != nil {
			a.hooks.dropped(d)
		}
	case ctl.EventIdle:
		var d ctl.IdleData
		if ev.Decode(&d) == nil {
			a.idle(d.Idle) // the host's at-rest fact, drain window included
		}

	// --- local-only families, routed to the surface's own arbitration ---

	case ctl.EventInteractionPending:
		var d ctl.InteractionPendingData
		if ev.Decode(&d) == nil && a.hooks.dialogPending != nil {
			a.hooks.dialogPending(d)
		}
	case ctl.EventInteractionSettled:
		var d ctl.InteractionSettledData
		if ev.Decode(&d) == nil && a.hooks.dialogSettled != nil {
			a.hooks.dialogSettled(d.ID)
		}

	// session facts the next snapshot carries; nothing paints from these alone.
	case ctl.EventSessionRenamed, ctl.EventPermission, ctl.EventTools:
	}
}

// Snapshot applies one full state, clearing what it no longer lists: segments
// and activity rows that dropped out, a queue replaced wholesale by row id, and
// a spinner whose turn ended while this surface was away. A reconnect
// resnapshots instead of replaying events, so the diff against what is already
// rendered is the whole reconcile rule.
func (a *applier) Snapshot(snap ctl.State) {
	if snap.Model.Key != "" {
		a.model(snap.Model.Key, snap.Model.Short, snap.Model.Window)
	}
	for _, seg := range snap.Status.Segments {
		a.segment(segment(seg.Key, seg.Text, seg.Short))
	}
	listed := bulk.SliceToSetBy(func(s ctl.Segment) string { return s.Key }, snap.Status.Segments)
	for _, key := range a.staleKeys(a.segs, listed) {
		a.segment(segment(key, "", ""))
	}
	for _, row := range snap.Activity {
		a.activity(row.Key, row.Text, row.Rank)
	}
	liveActs := bulk.SliceToSetBy(func(r ctl.ActivityRow) string { return r.Key }, snap.Activity)
	for _, key := range a.staleKeys(a.acts, liveActs) {
		a.activity(key, "", 0)
	}
	a.context(wireContext(snap.Context))
	a.queued(snap.QueuedRows)

	if snap.Spinning {
		a.sink.TurnStart(agent.TurnInfo{})
		a.setRunning(true)
	} else if a.live() {
		// the turn ended while this surface was away and its end event was lost
		// to the drop: force it, or the spinner sticks past reconnect
		a.sink.TurnEnd(agent.TurnResult{})
		a.setRunning(false)
		a.dropCalls()
	}
	a.idle(snap.Idle) // last word: the snapshot is the truth both branches reconcile toward
}

// live reports whether a turn is in flight by this surface's accounting.
func (a *applier) live() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.turnLive
}

// staleKeys lists the tracked keys a snapshot no longer names. Caller does not
// hold mu; the read and the clears it feeds are individually locked, and only
// one goroutine applies snapshots.
func (a *applier) staleKeys(tracked map[string]struct{}, listed map[string]struct{}) []string {
	a.mu.Lock()
	keys := bulk.MapKeysSlice(tracked)
	a.mu.Unlock()
	return bulk.SliceFilter(func(key string) bool {
		_, ok := listed[key]
		return !ok
	}, keys)
}

// notice surfaces one transient line at its severity, keyed notices replacing
// in place.
func (a *applier) notice(key, text string, level tui.Level) {
	if key == "" {
		a.surf.Notify(text, level)
		return
	}
	a.surf.NotifyKeyed(key, text, level)
}

// print renders one markdown block into history.
func (a *applier) print(markdown string) { a.surf.Print(markdown) }

// segment adds, replaces or removes a keyed status segment and tracks it for
// snapshot reconciliation. An empty Text removes. The hint key may route
// through the surface's own arbitration, so wire hints and local claims share
// one board instead of racing for the line.
func (a *applier) segment(seg tui.Segment) {
	a.mu.Lock()
	if seg.Text == "" {
		delete(a.segs, seg.Key)
	} else {
		a.segs[seg.Key] = struct{}{}
	}
	a.mu.Unlock()
	if seg.Key == segHint && a.hooks.hint != nil {
		a.hooks.hint(seg.Text, seg.Short)
		return
	}
	a.surf.SetStatusSegment(seg)
}

// model updates the model labels.
func (a *applier) model(key, short string, maxTokens int) {
	a.surf.SetModel(key, short, maxTokens)
}

// activity adds, replaces or removes a keyed activity row and tracks it for
// snapshot reconciliation. An empty Text removes.
func (a *applier) activity(key, text string, rank int) {
	a.mu.Lock()
	if text == "" {
		delete(a.acts, key)
	} else {
		a.acts[key] = struct{}{}
	}
	a.mu.Unlock()
	a.surf.SetActivityRanked(key, text, rank)
}

// queued replaces the pending-prompt rows wholesale: the wire copy is the
// truth by row id, so an emptied queue clears instead of keeping its old rows.
func (a *applier) queued(rows []ctl.QueueRow) {
	if a.hooks.queueRows != nil {
		a.surf.SetQueued(a.hooks.queueRows(rows))
		return
	}
	a.surf.SetQueued(queueRowTexts(rows))
}

// idle flips the prompt's rest state.
func (a *applier) idle(idle bool) { a.surf.SetIdle(idle) }

// context repaints the context bar through the shared sink mapping.
func (a *applier) context(c tokens.ContextState) { a.sink.Context(c) }

// toolResult applies the wire rule for one finished call: a start lost to the
// attach window renders its header here, late but attached to its body, and a
// non-streaming tool commits its body from the result because streamed deltas
// already commit at their end.
func (a *applier) toolResult(d ctl.ToolResultData) {
	call := a.takeCall(d.ID)
	done := call.done
	if done == nil {
		done = a.sink.ToolStart(agent.ToolCall{ID: d.ID, Name: d.Name}, d.Name, false)
	}
	res := agent.ToolResult{IsError: d.Error}
	if !call.streamed {
		res.Display = d.Output
		if d.Error && strings.TrimSpace(d.Output) != "" {
			// an error's body commits from Content on the shared sink path,
			// never Display, so a denial reason survives the wire round trip
			res.Content = llm.BlockList{llm.TextBlock{Text: d.Output}}
		}
	}
	done(res)
}

// setRunning records a turn boundary in this surface's accounting and reports
// it to the hook. Only real flips reach the hook, so idempotent reconciles do
// not repaint idle.
func (a *applier) setRunning(live bool) {
	a.mu.Lock()
	if a.turnLive == live {
		a.mu.Unlock()
		return
	}
	a.turnLive = live
	a.mu.Unlock()
	if a.hooks.running != nil {
		a.hooks.running(live)
	}
}

// callFor returns one call's tracking entry, creating it when output or a
// result arrives beside (or ahead of) its start event.
func (a *applier) callFor(id string) *apCall {
	if id == "" {
		return &apCall{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.calls[id]
	if !ok {
		c = &apCall{}
		a.calls[id] = c
	}
	return c
}

// takeCall consumes one call's tracking entry for its result.
func (a *applier) takeCall(id string) *apCall {
	if id == "" {
		return &apCall{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.calls[id]
	delete(a.calls, id)
	if c == nil {
		c = &apCall{}
	}
	return c
}

// dropCalls forgets every tracked call; turn end is the natural boundary.
func (a *applier) dropCalls() {
	a.mu.Lock()
	a.calls = make(map[string]*apCall)
	a.mu.Unlock()
}

// wireContext maps a context event payload onto the ledger terms the sink
// renders. The field list matches hubui's contextState in reverse.
func wireContext(d ctl.ContextData) tokens.ContextState {
	return tokens.ContextState{Used: d.Used, Window: d.Window,
		Reserve: d.Reserve, Compact: d.Compact, Estimated: d.Estimated}
}

// wireLevel maps a notice event's level onto the terminal's.
func wireLevel(s string) tui.Level {
	switch s {
	case "warn":
		return tui.LevelWarn
	case "error":
		return tui.LevelError
	default:
		return tui.LevelInfo
	}
}
