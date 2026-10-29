package app

import (
	"context"
	"errors"
	"sync"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/client"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tui"
	tuisink "github.com/jentfoo/ajent/pkg/tui/sink"
)

// attachView renders one remote session onto a local terminal: backfill from
// the transcript file and snapshot, then the live event stream applied through
// the same sink mapping the host's own renderer uses. Events arrive on the
// reader goroutine; nothing here may block on a request.
type attachView struct {
	ui   *tui.UI
	sink *tuisink.Sink
	conn *client.Conn
	root context.Context // base for background requests issued off the reader goroutine
	// path is the transcript file hello named; bulk reads (backfill, recall,
	// redraw) come from it at local speed instead of over the socket
	path string

	mu      sync.Mutex
	live    bool
	buf     []ctl.Event // events seen before backfill completed, applied against the watermark then
	running bool        // remote turn in flight; drives SetIdle so Esc keeps its interrupt role
	head    string      // last known transcript head entry id
	dialogs map[uint64]context.CancelFunc
	// promptsCache is the last good recall list, labeled with the head it was
	// read at. prompts() is the only writer, off the reader goroutine.
	promptsCache []session.Prompt
	promptsHead  string
	// calls tracks one in-flight tool call each: its completion hook and whether
	// this surface already streamed the output deltas. Written and read only on
	// the reader goroutine.
	calls map[string]*attachCall
}

// attachCall is one in-flight tool call's client-side state.
type attachCall struct {
	done     func(agent.ToolResult) // the header hook ui.ToolStart returned
	streamed bool                   // output deltas already rendered locally
}

// newAttachView builds the view over a connected client; root is cancelled at
// teardown, taking any in-flight background redraw with it.
func newAttachView(ui *tui.UI, conn *client.Conn, root context.Context, transcriptPath string) *attachView {
	return &attachView{
		ui: ui, sink: tuisink.New(ui), conn: conn, root: root,
		path:    transcriptPath,
		dialogs: make(map[uint64]context.CancelFunc),
		calls:   make(map[string]*attachCall),
	}
}

func (v *attachView) backfill(ctx context.Context) error {
	snap, branch, seq, err := v.conn.Backfill(ctx)
	if err != nil {
		return err
	}
	v.applyBackfill(snap, branch, seq)
	return nil
}

// applyBackfill renders one fetched backfill and goes live: buffered events at
// or below the watermark are superseded by what was just rendered, everything
// above applies. The window between hello and here is milliseconds wide; a
// delta inside it may lose its first characters.
func (v *attachView) applyBackfill(snap ctl.State, branch []session.Entry, water uint64) {
	v.mu.Lock()
	v.head = snap.Head
	buf := v.buf
	v.live, v.buf = true, nil
	v.mu.Unlock()

	v.renderHistory(branch)
	v.applySnapshot(snap)

	for _, ev := range buf {
		if ev.Seq > water {
			v.apply(ev)
		}
	}
}

// renderHistory commits one branch through the replay sink: user prompts above
// their replies, tool headers carrying their bodies, exactly as a resume shows.
func (v *attachView) renderHistory(branch []session.Entry) {
	if len(branch) == 0 {
		return
	}
	session.Replay(branch, v.sink, session.ReplayOptions{})
}

// installRecall wires plain ↑/↓ and ctrl+r onto the terminal, sourcing both
// from this session's recorded prompts: there is no local store.
func (v *attachView) installRecall() {
	v.ui.SetHistorySearch(func() []tui.SearchItem { return searchItems(dedupePrompts(v.prompts())) })
	v.ui.SetPromptHistory(func() []string { return session.PromptTexts(v.prompts()) })
}

// applySnapshot paints the chrome a snapshot holds truth for.
func (v *attachView) applySnapshot(snap ctl.State) {
	if snap.Model.Key != "" {
		v.ui.SetModel(snap.Model.Key, snap.Model.Short, snap.Model.Window)
	}
	for _, seg := range snap.Status.Segments {
		v.ui.SetStatusSegment(segment(seg.Key, seg.Text, seg.Short))
	}
	v.sink.Context(tokens.ContextState{Used: snap.Context.Used, Window: snap.Context.Window,
		Reserve: snap.Context.Reserve, Compact: snap.Context.Compact, Estimated: snap.Context.Estimated})
	if len(snap.QueuedTexts) > 0 {
		v.ui.SetQueued(snap.QueuedTexts)
	}
	for _, a := range snap.Activity {
		v.ui.SetActivityRanked(a.Key, a.Text, a.Rank)
	}
	if snap.Spinning {
		v.sink.TurnStart(agent.TurnInfo{})
		v.setRunning(true)
	}
}

// handle applies one event, buffering until backfill completes.
func (v *attachView) handle(ev ctl.Event) {
	v.mu.Lock()
	if !v.live {
		v.buf = append(v.buf, ev)
		v.mu.Unlock()
		return
	}
	v.mu.Unlock()
	v.apply(ev)
}

// apply maps one control event onto the terminal surface. Stream events mirror
// the host renderer's own sink calls one to one; session surfaces forward to
// the tui methods that produced them.
func (v *attachView) apply(ev ctl.Event) {
	switch ev.Name {
	// --- stream events, through the shared sink mapping ---

	case ctl.EventTurnStart:
		v.sink.TurnStart(agent.TurnInfo{})
		v.setRunning(true)
	case ctl.EventUserPrompt:
		var d ctl.UserPromptData
		if ev.Decode(&d) == nil {
			v.sink.UserPrompt(d.Text)
		}
	case ctl.EventThinkingDelta:
		var d ctl.TextData
		if ev.Decode(&d) == nil {
			v.sink.Thinking(d.Delta)
		}
	case ctl.EventThinkingEnd:
		v.sink.EndThinking()
	case ctl.EventTextDelta:
		var d ctl.TextData
		if ev.Decode(&d) == nil {
			v.sink.Text(d.Delta)
		}
	case ctl.EventTextEnd:
		v.sink.EndText()
	case ctl.EventToolStart:
		var d ctl.ToolStartData
		if ev.Decode(&d) != nil {
			return
		}
		done := v.sink.ToolStart(agent.ToolCall{ID: d.ID, Name: d.Name}, d.Label, d.Full)
		v.callFor(d.ID).done = done
	case ctl.EventToolOutput:
		var d ctl.ToolOutputData
		if ev.Decode(&d) == nil {
			v.callFor(d.ID).streamed = true // deltas render below; the result path must not repeat them
			v.sink.ToolOutput(d.ID, d.Delta)
		}
	case ctl.EventToolProgress:
		var d ctl.ToolProgressData
		if ev.Decode(&d) != nil {
			return
		}
		v.sink.ToolProgress(agent.ToolProgress{CallID: d.ID, Name: d.Name,
			Path: d.Path, Bytes: d.Bytes, Lines: d.Lines, Done: d.Done})
	case ctl.EventToolResult:
		var d ctl.ToolResultData
		if ev.Decode(&d) != nil {
			return
		}
		call := v.takeCall(d.ID)
		done := call.done
		if done == nil {
			// a start lost to the attach window renders its header here, late but
			// attached to its body
			done = v.sink.ToolStart(agent.ToolCall{ID: d.ID, Name: d.Name}, d.Name, false)
		}
		res := agent.ToolResult{IsError: d.Error}
		if !call.streamed {
			// a non-streaming tool's body reaches history only through the result;
			// streamed deltas already commit at their end, so their full-text copy
			// on the wire (for controllers without the stream) must not render again
			res.Display = d.Output
		}
		done(res)
	case ctl.EventDiff:
		var d ctl.DiffData
		if ev.Decode(&d) == nil {
			v.sink.Diff(d.Path, d.Before, d.After)
		}
	case ctl.EventContext:
		var d ctl.ContextData
		if ev.Decode(&d) == nil {
			v.sink.Context(tokens.ContextState{Used: d.Used, Window: d.Window,
				Reserve: d.Reserve, Compact: d.Compact, Estimated: d.Estimated})
		}
	case ctl.EventTurnEnd:
		v.sink.TurnEnd(agent.TurnResult{})
		v.setRunning(false)
		v.dropCalls() // calls whose result never landed keep nothing behind

	// --- session events, forwarded to the methods that produced them ---

	case ctl.EventEcho:
		var d ctl.EchoData
		if ev.Decode(&d) == nil {
			v.ui.UserEcho(d.Text)
		}
	case ctl.EventEchoImages:
		var d ctl.EchoImagesData
		if ev.Decode(&d) == nil && len(d.Images) > 0 {
			ims := make([]tui.Image, len(d.Images))
			for i, im := range d.Images {
				ims[i] = tui.Image{Data: im.Data, MediaType: im.MediaType,
					Label: im.Label, W: im.W, H: im.H}
			}
			v.ui.UserImages(ims)
		}
	case ctl.EventNotice:
		var d ctl.NoticeData
		if ev.Decode(&d) != nil {
			return
		}
		level := wireLevel(d.Level)
		if d.Key == "" {
			v.ui.Notify(d.Text, level)
		} else {
			v.ui.NotifyKeyed(d.Key, d.Text, level)
		}
	case ctl.EventPrint:
		var d ctl.PrintData
		if ev.Decode(&d) == nil {
			v.ui.Print(d.Markdown)
		}
	case ctl.EventStatus:
		v.applyStatus(ev)
	case ctl.EventActivity:
		var d ctl.ActivityData
		if ev.Decode(&d) == nil {
			v.ui.SetActivityRanked(d.Key, d.Text, d.Rank)
		}
	case ctl.EventQueued:
		var d ctl.QueuedData
		if ev.Decode(&d) == nil {
			v.ui.SetQueued(d.Texts)
		}

	// editor drafts are droppable: two editors typing would fight over the buffer

	case ctl.EventEntryAppend:
		var d ctl.EntryData
		if ev.Decode(&d) == nil { // appends extend the head silently; only a move redraws
			v.setHead(d.Entry.ID)
		}
	case ctl.EventHead:
		var d ctl.HeadData
		if ev.Decode(&d) != nil {
			return
		}
		v.setHead(d.ID)
		// off the reader goroutine: redraw re-reads the transcript file
		go v.redraw()
	}
}

// prompts returns the session's recorded user prompts, newest first: the recall
// source for plain ↑/↓ and ctrl+r search. It reads the transcript file, caching
// until the head moves so gestures never re-read an unchanged branch; a failed
// read keeps the last good list.
func (v *attachView) prompts() []session.Prompt {
	v.mu.Lock()
	cached, labeled := v.promptsCache, v.promptsHead
	head := v.head
	v.mu.Unlock()

	if cached != nil && labeled == head {
		return cached // the branch has not moved since this cache was read
	}
	branch, err := client.ReadBranch(v.path, head)
	if err != nil {
		return cached
	}
	ps := branchPrompts(branch)
	v.mu.Lock()
	v.promptsCache, v.promptsHead = ps, head
	v.mu.Unlock()
	return ps
}

// branchPrompts extracts one transcript branch's recorded user prompts, newest
// first, minus secret-prefixed lines.
func branchPrompts(entries []session.Entry) []session.Prompt {
	ps := session.PromptsOf(entries)
	return bulk.SliceFilter(func(p session.Prompt) bool {
		return !session.HasSecretPrefix(secretPrefix, p.Text)
	}, ps)
}

// dedupePrompts keeps each text's newest occurrence; the list is newest first.
func dedupePrompts(ps []session.Prompt) []session.Prompt {
	seen := make(map[string]struct{}, len(ps))
	return bulk.SliceFilter(func(p session.Prompt) bool {
		if _, dup := seen[p.Text]; dup {
			return false
		}
		seen[p.Text] = struct{}{}
		return true
	}, ps)
}

// applyStatus forwards one status-row change. Spinning is ignored: the turn
// events driving the host's own spinner drive this view's through the sink.
func (v *attachView) applyStatus(ev ctl.Event) {
	var d ctl.StatusData
	if ev.Decode(&d) != nil {
		return
	}
	switch {
	case d.Segment != nil:
		// placement comes from this build's spec table, so equal keys collapse alike
		v.ui.SetStatusSegment(segment(d.Segment.Key, d.Segment.Text, d.Segment.Short))
	case d.Model != nil:
		v.ui.SetModel(d.Model.Key, d.Model.Short, d.Model.Window)
	}
}

// callFor returns one call's tracking entry, creating it when output or a
// result arrives beside (or ahead of) its start event.
func (v *attachView) callFor(id string) *attachCall {
	if id == "" {
		return &attachCall{}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	c, ok := v.calls[id]
	if !ok {
		c = &attachCall{}
		v.calls[id] = c
	}
	return c
}

// takeCall consumes one call's tracking entry for its result.
func (v *attachView) takeCall(id string) *attachCall {
	if id == "" {
		return &attachCall{}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	c := v.calls[id]
	delete(v.calls, id)
	if c == nil {
		c = &attachCall{}
	}
	return c
}

// dropCalls forgets every tracked call; turn end is the natural boundary.
func (v *attachView) dropCalls() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = make(map[string]*attachCall)
}

// redraw re-renders from the transcript file after a rewind or fork moved the
// head, whether that move was local or came from the other terminal.
func (v *attachView) redraw() {
	v.mu.Lock()
	head := v.head
	running := v.running
	v.mu.Unlock()

	branch, err := client.ReadBranch(v.path, head)
	if err != nil {
		v.ui.Notify("refresh after rewind failed: "+err.Error(), tui.LevelWarn)
		return
	}
	v.ui.Reset()
	v.ui.Divider() // mark where the restored branch begins, like a local rewind
	v.renderHistory(branch)
	v.ui.SetIdle(!running)
}

// setRunning tracks the remote turn state so Esc interrupts immediately and
// double-Esc rewinds only while the session is at rest.
func (v *attachView) setRunning(running bool) {
	v.mu.Lock()
	if v.running == running {
		v.mu.Unlock()
		return
	}
	v.running = running
	v.mu.Unlock()
	v.ui.SetIdle(!running)
}

// setHead records the transcript head for the rewind picker's tree read.
func (v *attachView) setHead(id string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if id != "" {
		v.head = id
	}
}

// beginInteraction races a remote dialog against the local human: the first
// answer wins on the session side, and a settled event cancels this racer. The
// racer rides the view root, so teardown takes any still-open one with it.
func (v *attachView) beginInteraction(d ctl.InteractionPendingData) {
	ctx, cancel := context.WithCancel(v.root)
	v.registerDialog(d.ID, cancel)
	go v.raceDialog(ctx, d)
}

// registerDialog keeps one racer's canceller under its remote id.
func (v *attachView) registerDialog(id uint64, cancel context.CancelFunc) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.dialogs[id] = cancel
}

// raceDialog asks the local surface and resolves what it answered. A settled
// event cancels ctx first, so a lost race sends nothing.
func (v *attachView) raceDialog(ctx context.Context, d ctl.InteractionPendingData) {
	ans, ok := answerLocal(ctx, v.ui, d)
	if !ok {
		return // cancelled remotely, or nothing this surface could show
	}
	// WithoutCancel: a settled event cancelling ctx must not stop the answer it
	// lost to from being attempted; the session side refuses legibly.
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), client.CallWait)
	defer rcancel()
	err := v.conn.Resolve(rctx, ans)
	var wire *ctl.Error
	switch {
	case err == nil:
	case errors.As(err, &wire) &&
		(wire.Code == ctl.CodeSettled || wire.Code == ctl.CodeNotFound):
		return // the other side won the race; its settled event already dismissed this dialog
	default:
		v.ui.Notify("answering failed: "+err.Error(), tui.LevelWarn)
	}
}

// settleInteraction dismisses one open local dialog whose remote twin settled.
func (v *attachView) settleInteraction(id uint64) {
	v.mu.Lock()
	cancel := v.dialogs[id]
	delete(v.dialogs, id)
	v.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// closeDialogs cancels every still-open local racer; run at teardown.
func (v *attachView) closeDialogs() {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, cancel := range v.dialogs {
		cancel()
	}
	v.dialogs = make(map[uint64]context.CancelFunc)
}

// answerLocal shows d on the local surface and maps the answer onto its resolve
// shape. It reports false when the racer was cancelled first or nothing could
// be shown, leaving the remote dialog to whoever else can answer it.
func answerLocal(ctx context.Context, dl localDialogs, d ctl.InteractionPendingData) (ctl.ResolveParams, bool) {
	cancel := func() (ctl.ResolveParams, bool) { return ctl.ResolveParams{ID: d.ID, Cancel: true}, ctx.Err() == nil }
	skip := func() (ctl.ResolveParams, bool) { return ctl.ResolveParams{}, false }

	opts := d.Options
	switch d.Kind {
	case "pick":
		i, err := dl.PickContext(ctx, d.Prompt, pickItems(opts), tui.PickOptions{})
		if err != nil {
			return noUIOrCancel(err, cancel, skip)
		}
		return ctl.ResolveParams{ID: d.ID, Index: &i}, true
	case "multipick":
		idx, err := dl.MultiPickContext(ctx, d.Prompt, pickItems(opts), tui.MultiPickOptions{})
		if err != nil {
			return noUIOrCancel(err, cancel, skip)
		}
		return ctl.ResolveParams{ID: d.ID, Indexes: &idx}, true
	case "select":
		i, err := dl.SelectContext(ctx, d.Prompt, tuiOptions(opts))
		if err != nil {
			return noUIOrCancel(err, cancel, skip)
		}
		return ctl.ResolveParams{ID: d.ID, Index: &i}, true
	case "confirm":
		yes, err := dl.ConfirmContext(ctx, d.Prompt)
		if err != nil {
			return noUIOrCancel(err, cancel, skip)
		}
		i := boolToInt(yes) // "No" is the offered pair's second row
		return ctl.ResolveParams{ID: d.ID, Index: &i}, true
	case "input", "secret":
		var text string
		var err error
		if d.Kind == "secret" {
			text, err = dl.InputSecretContext(ctx, d.Prompt, "")
		} else {
			text, err = dl.InputContext(ctx, d.Prompt, "")
		}
		if err != nil {
			return noUIOrCancel(err, cancel, skip)
		}
		return ctl.ResolveParams{ID: d.ID, Text: &text}, true
	case "ask":
		ans, err := dl.Ask(ctx, tui.Question{Text: d.Prompt, Options: tuiOptions(opts)})
		if err != nil {
			return noUIOrCancel(err, cancel, skip)
		}
		switch {
		case ans.Declined:
			return cancel()
		case ans.Chat || (ans.Text != "" && len(opts) == 0):
			return ctl.ResolveParams{ID: d.ID, Text: &ans.Text, Chat: ans.Chat}, true
		default:
			return ctl.ResolveParams{ID: d.ID, Index: &ans.Index}, true
		}
	case "decision":
		dec := dl.OpenDecision(tui.DecisionRequest{Prompt: d.Prompt, Context: d.Subject,
			Options: tuiOptions(opts)})
		r, err := dec.Wait(ctx)
		if err != nil {
			return noUIOrCancel(err, cancel, skip)
		}
		return ctl.ResolveParams{ID: d.ID, Index: &r.Index}, true
	default:
		// an unknown kind has no local shape: leave it for the session's own terminal
		return skip()
	}
}

// noUIOrCancel maps a local dialog failure: nothing shown leaves the remote
// dialog open, an explicit Esc is a real answer of decline.
func noUIOrCancel(err error, cancel func() (ctl.ResolveParams, bool), skip func() (ctl.ResolveParams, bool)) (ctl.ResolveParams, bool) {
	if errors.Is(err, tui.ErrNoUI) {
		return skip()
	}
	return cancel()
}

// pickItems renders option labels as picker rows.
func pickItems(opts []string) []tui.PickItem {
	items := make([]tui.PickItem, len(opts))
	for i, o := range opts {
		items[i] = tui.PickItem{Label: o}
	}
	return items
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
