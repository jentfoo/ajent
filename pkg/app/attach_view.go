package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/client"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tui"
	tuisink "github.com/jentfoo/ajent/pkg/tui/sink"
)

// preLiveBufMax bounds the pre-backfill event buffer, so a wedged backfill
// cannot grow it without limit while a turn streams.
const preLiveBufMax = 1024

// attachView renders one remote session onto a local terminal: backfill from
// the transcript file and snapshot, then the live event stream applied through
// the shared applier, the same projection the host terminal paints its own
// mutations with.
//
// One goroutine owns the view: every mutation (events, backfill, head moves,
// reconnect transitions) is a message on the actor queue, which makes the
// live-flag ordering, the sink's busy field, and the async-redraw wipe
// impossible rather than fixed. Requests off the actor run on the view root.
type attachView struct {
	ui   *tui.UI
	sink *tuisink.Sink   // committed rendering: replay and the one echo rule
	ap   *applier        // live events: stream mapping plus chrome, over the same sink
	root context.Context // base for background requests issued off the actor
	// path is the transcript file hello named; bulk reads (backfill, recall,
	// redraw) come from it at local speed instead of over the socket
	path string

	mu      sync.Mutex // guards only what the UI goroutine's callbacks read
	head    string     // last known transcript head entry id
	running bool       // remote turn in flight; drives SetIdle so Esc keeps its interrupt role
	hello   ctl.Hello  // greeting of the connection in use

	// msgs is the actor mailbox. Backfill-before-buffered-events holds by queue
	// order, never a flag.
	msgs   chan func()
	closed chan struct{}

	// actor-owned state below; only run() touches it
	conn         *client.Conn
	rendered     map[string]struct{} // committed entry ids this surface already shows
	pending      []pendingRow        // this terminal's submitted prompts awaiting their entry
	wireQueued   []ctl.QueueRow      // the last queue copy, for pending merges
	dialogs      map[uint64]context.CancelFunc
	outcome      *time.Timer
	live         bool        // backfill completed; events apply instead of buffering
	buf          []ctl.Event // events seen before backfill, applied against its watermark
	redrawQueued bool        // a full repaint is already queued; another head move needs none

	// promptsHead and promptsCache are gone: recall reads the same on-disk
	// indexes the host keyboard uses, which cache by size and mtime themselves.
}

// setTranscript names the file bulk reads come from. Called once before the
// first read, under mu for the race detector.
func (v *attachView) setTranscript(path string) {
	v.mu.Lock()
	v.path = path
	v.mu.Unlock()
}

// transcript reads the transcript file path under mu.
func (v *attachView) transcript() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.path
}

// helloNow reads the greeting of the connection in use.
func (v *attachView) helloNow() ctl.Hello {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.hello
}

// setHello records the greeting of the connection in use.
func (v *attachView) setHello(h ctl.Hello) {
	v.mu.Lock()
	v.hello = h
	v.mu.Unlock()
}

// setHeadNow records a head this terminal itself moved over the wire.
func (v *attachView) setHeadNow(id string) {
	v.mu.Lock()
	if id != "" {
		v.head = id
	}
	v.mu.Unlock()
}

// notConnected refuses a submit while reconnecting, restoring the editor text
// so nothing vanishes.
func (v *attachView) notConnected(line string) {
	v.ui.Notify("not connected; the text is kept in the editor", tui.LevelWarn)
	v.ui.PrependInput(line)
}

// pendingRow is one prompt this terminal submitted whose entry has not landed:
// the correlation it sent with the submit and the typed line shown while it
// waits, restored on a failed submit.
type pendingRow struct {
	id   string
	text string
}

// submitFailed resolves one submission's pending row inline with the reply's
// error and restores the text, matching the serve-mode prefill rule.
func (v *attachView) submitFailed(id, line string, err error) {
	v.send(func() {
		v.takePendingID(id)
		v.refreshQueue()
	})
	v.ui.Notify("submit failed: "+err.Error()+"; the text is kept in the editor", tui.LevelWarn)
	v.ui.PrependInput(line)
}

// newAttachView builds the view over a connected client; root is cancelled at
// teardown, taking any in-flight background redraw with it.
func newAttachView(ui *tui.UI, conn *client.Conn, root context.Context, transcriptPath string) *attachView {
	v := &attachView{
		ui: ui, sink: tuisink.New(ui), conn: conn, root: root,
		path:    transcriptPath,
		dialogs: make(map[uint64]context.CancelFunc),
		msgs:    make(chan func(), 256),
		closed:  make(chan struct{}),
	}
	v.ap = newApplier(ui, v.sink, applyHooks{
		queueRows:     v.queueTexts,
		dropped:       v.queueDropped,
		running:       v.setRunning,
		dialogPending: func(d ctl.InteractionPendingData) { v.beginInteraction(root, d) },
		dialogSettled: v.settleInteraction,
	})
	go v.run()
	return v
}

// run is the actor loop: the only goroutine that touches the sink, the
// rendered set, pending rows and dialogs.
func (v *attachView) run() {
	defer close(v.closed)
	for {
		select {
		case fn := <-v.msgs:
			fn()
		case <-v.root.Done():
			return
		}
	}
}

// send queues one mutation for the actor. After teardown it drops, matching
// every other cancelled background write.
func (v *attachView) send(fn func()) {
	select {
	case v.msgs <- fn:
	case <-v.closed:
	case <-v.root.Done():
	}
}

// call runs fn on the actor and waits for it to finish, false once the view
// is torn down.
func (v *attachView) call(fn func()) bool {
	done := make(chan struct{})
	v.send(func() { fn(); close(done) })
	select {
	case <-done:
		return true
	case <-v.closed:
		return false
	case <-v.root.Done():
		return false
	}
}

// waitIdle drains the actor queue, for tests and the backfill handshake.
func (v *attachView) waitIdle() {
	v.call(func() {})
}

// backfill fetches one starting state and hands it to the actor, which renders
// the branch and marks every id before any buffered event can apply.
func (v *attachView) backfill(ctx context.Context) error {
	snap, branch, seq, err := v.conn.Backfill(ctx)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	v.send(func() { done <- v.applyBackfill(snap, branch, seq) })
	select {
	case err := <-done:
		return err
	case <-v.closed:
		return errors.New("attach view stopped")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// applyBackfill renders one fetched backfill and goes live. On a first attach
// the whole branch renders; on a reconnect only the difference does: unseen
// entries on an unchanged head render incrementally, a moved head gets the
// divider and full-branch redraw every rewind shows. The window between hello
// and here is milliseconds wide; a delta inside it may lose its first
// characters.
func (v *attachView) applyBackfill(snap ctl.State, branch []session.Entry, water uint64) error {
	first := v.rendered == nil
	defer func() {
		// buffered events drain last, against the watermark: everything the
		// backfill rendered at or below it is superseded
		buf := v.buf
		v.buf = nil
		v.live = true
		for _, ev := range buf {
			if ev.Seq > water {
				v.apply(ev)
			}
		}
	}()
	v.mu.Lock()
	headMoved := snap.Head != v.head && !first
	v.head = snap.Head
	v.mu.Unlock()
	prev := v.rendered
	v.rendered = bulk.SliceToSetBy(func(e session.Entry) string { return e.ID }, branch)
	switch {
	case first:
		v.renderHistory(branch)
	case headMoved:
		v.repaint(branch)
	default:
		for _, e := range branch {
			if _, ok := prev[e.ID]; !ok {
				v.renderEntry(e)
			}
		}
	}
	v.reconcilePending(branch)
	v.ap.Snapshot(snap) // the reconcile pass clears what the snapshot no longer lists
	v.reconcileDialogs(v.root, snap.Interactions)
	return nil
}

// swapConn installs conn as the actor's live connection.
func (v *attachView) swapConn(conn *client.Conn) {
	v.call(func() { v.conn = conn })
}

// connDown drops the connection and goes quiet: connFor answers nil so
// interrupts, submits and dialog answers report not connected, and events
// buffer until the reconnect's backfill renders the difference.
func (v *attachView) connDown() {
	v.call(func() {
		v.conn = nil
		v.live = false
	})
}

// renderHistory commits one branch through the replay sink: user prompts above
// their replies, tool headers carrying their bodies, exactly as a resume shows.
func (v *attachView) renderHistory(branch []session.Entry) {
	if len(branch) == 0 {
		return
	}
	session.Replay(branch, v.sink, session.ReplayOptions{})
}

// renderEntry paints one committed entry the surface has not shown, through
// the one echo rule Replay also applies (session.EchoEntry): a prompt echoes,
// a replay-marked injected block draws as its own block, a notice replays, and
// everything the stream or Replay's folding owns paints nothing.
func (v *attachView) renderEntry(e session.Entry) {
	session.EchoEntry(e, v.sink)
}

// installRecall wires plain ↑/↓ and ctrl+r onto the terminal over the same
// local stores the host keyboard recalls from (command-design.md): search
// walks the workspace-wide recall store keyed to the served run's workspace,
// arrows scope to this session's own transcript. Read-only participation: the
// served run appends, this terminal only recalls.
func (v *attachView) installRecall() {
	search, arrows := v.recallSources()
	v.ui.SetHistorySearch(search)
	v.ui.SetPromptHistory(arrows)
}

// recallSources builds the ctrl+r and ↑/↓ providers from hello's own facts:
// a nil search means no recall store was readable, so both gestures fall back
// to editor history, and a missing transcript path lets arrows share that
// fallback. Everything reads local files; nothing rides the socket.
func (v *attachView) recallSources() (search func() []tui.SearchItem, arrows func() []string) {
	hello := v.helloNow()
	store, err := session.NewStore()
	if err != nil {
		return nil, nil
	}
	hist, _ := session.NewEditorHistory(store, hello.Workspace, secretPrefix)
	idx := session.NewRecallIndex(store, hello.Workspace, secretPrefix, hist)
	search = func() []tui.SearchItem { return searchItems(idx.Lines()) }
	if hello.TranscriptPath != "" {
		sess := session.NewTranscriptIndex(hello.TranscriptPath, secretPrefix)
		arrows = func() []string { return session.PromptTexts(sess.Prompts()) }
	}
	return search, arrows
}

// handle applies one live event, buffering on the actor until backfill lands:
// queue order renders history first, then the watermark drops what it superseded.
func (v *attachView) handle(ev ctl.Event) {
	v.send(func() {
		if !v.live {
			// bounded: a flood during a slow backfill drops the oldest, which the
			// resnapshot's watermark reconciliation covers anyway
			if len(v.buf) >= preLiveBufMax {
				v.buf = v.buf[1:]
			}
			v.buf = append(v.buf, ev)
			return
		}
		v.apply(ev)
	})
}

// apply applies one live event. Committed rendering and head moves reconcile
// through this view's own books; everything else rides the shared applier.
func (v *attachView) apply(ev ctl.Event) {
	switch ev.Name {
	case ctl.EventEntryAppend:
		var d ctl.EntryData
		if ev.Decode(&d) == nil {
			v.applyAppend(d.Entry)
		}
	case ctl.EventBranchChanged:
		var d ctl.BranchChangedData
		if ev.Decode(&d) != nil {
			return
		}
		v.mu.Lock()
		v.head = d.Head
		v.mu.Unlock()
		if d.Cause == rewindCauseCompact {
			return // the compaction entry itself arrives via entry.append
		}
		v.requestRedraw()
	default:
		v.ap.Apply(ev)
	}
}

// dropOutcome names a queue removal honestly: a recall returns the text to an
// editor, everything else drops it. An unnamed cause is a cleared one.
func dropOutcome(cause string) string {
	switch cause {
	case "recalled":
		return "prompt recalled for editing"
	case "":
		cause = "cleared"
	}
	return "prompt dropped (" + cause + ")"
}

// queueTexts maps one wire copy of the queue onto display rows for the shared
// applier, remembering the copy so later repaints merge this terminal's own
// submissions into it.
func (v *attachView) queueTexts(rows []ctl.QueueRow) []string {
	v.wireQueued = rows
	return v.withPending(rows)
}

// refreshQueue repaints the queue display from the last wire copy and this
// terminal's unresolved submissions, after a pending row resolved inline.
func (v *attachView) refreshQueue() { v.ap.queued(v.wireQueued) }

// queueDropped resolves this terminal's pending rows a removal cleared and
// flashes one outcome per row on the hint line.
func (v *attachView) queueDropped(d ctl.QueueDroppedData) {
	v.resolvePending(d.Rows)
	for range d.Rows {
		v.showOutcome(dropOutcome(d.Cause))
	}
}

// applyAppend applies the wire rule for committed entries: ids already
// rendered mark silently, a pending row resolves when the prompt it submitted
// commits and its committed rendering takes the row's place, and everything
// else renders through the one echo rule. EchoEntry paints nothing for shapes
// the stream owns (assistant content, tool carriers, unmarked injected
// context), so what draws here is exactly what replay would draw for the same
// entry, mid-turn included.
func (v *attachView) applyAppend(e ctl.Entry) {
	if e.ID != "" { // appends extend the head silently; only a branch.changed redraws
		v.mu.Lock()
		v.head = e.ID
		v.mu.Unlock()
	}
	if _, ok := v.rendered[e.ID]; ok {
		return // replayed or already committed here
	}
	v.rendered[e.ID] = struct{}{}
	entry := session.Entry{ID: e.ID, ParentID: e.ParentID,
		Type: session.Type(e.Type), TS: e.TS, Data: e.Data}
	if entry.Type == session.TypeMessage {
		var md session.MessageData
		if entry.Decode(&md) == nil && md.Message.Role == llm.RoleUser &&
			v.takeCommitted(md.QueueIDs) {
			// the row resolved with its entry: repaint without it
			v.refreshQueue()
		}
	}
	v.renderEntry(entry)
}

// reconcilePending drops pending rows whose prompts committed while this
// terminal was away: only live appends announce themselves, so a resnapshot
// must resolve them from the branch it just rendered.
func (v *attachView) reconcilePending(branch []session.Entry) {
	if len(v.pending) == 0 {
		return
	}
	var resolved bool
	for _, e := range branch {
		if e.Type != session.TypeMessage {
			continue
		}
		var md session.MessageData
		if json.Unmarshal(e.Data, &md) == nil && md.Message.Role == llm.RoleUser &&
			v.takeCommitted(md.QueueIDs) {
			resolved = true
		}
	}
	if resolved {
		v.refreshQueue()
	}
}

// withPending merges this terminal's unresolved submissions into the wire's
// queue rows for display: a row the host already shows stays single, a secret
// row redacted on the wire renders from its local copy, and rows this terminal
// did not submit keep their own texts.
func (v *attachView) withPending(rows []ctl.QueueRow) []string {
	out := make([]string, 0, len(rows)+len(v.pending))
	wired := bulk.SliceToSetBy(func(r ctl.QueueRow) string { return r.ID }, rows)
	for _, r := range rows {
		if r.Text != "" {
			out = append(out, r.Text)
			continue
		}
		// redacted on the wire: only this terminal's own submission can name it
		for _, p := range v.pending {
			if p.id == r.ID {
				out = append(out, p.text)
			}
		}
	}
	for _, p := range v.pending { // not yet echoed by a queued event
		if _, ok := wired[p.id]; !ok {
			out = append(out, p.text)
		}
	}
	return out
}

// takePendingID removes one pending submission by its correlation, reporting
// whether it was this terminal's.
func (v *attachView) takePendingID(id string) bool {
	i := slices.IndexFunc(v.pending, func(p pendingRow) bool { return p.id == id })
	if i < 0 {
		return false
	}
	v.pending = slices.Delete(v.pending, i, i+1)
	return true
}

// takeCommitted consumes the pending rows a committed user prompt resolves:
// every row whose correlation rode the entry, joined runs included. Reports
// whether any row resolved.
func (v *attachView) takeCommitted(ids []string) bool {
	if len(ids) == 0 || len(v.pending) == 0 {
		return false
	}
	landed := bulk.SliceToSet(ids)
	kept := bulk.SliceFilter(func(p pendingRow) bool {
		_, done := landed[p.id]
		return !done
	}, v.pending)
	if len(kept) == len(v.pending) {
		return false
	}
	v.pending = kept
	return true
}

// resolvePending clears the pending rows a drop event named.
func (v *attachView) resolvePending(rows []ctl.QueueRow) {
	dropped := bulk.SliceToSetBy(func(r ctl.QueueRow) string { return r.ID }, rows)
	v.pending = bulk.SliceFilter(func(p pendingRow) bool {
		_, gone := dropped[p.id]
		return !gone
	}, v.pending)
}

// submitPending records a submission and paints its pending row at once, like
// the local editor echo: the committed rendering replaces it when the entry
// lands, and a drop event converts it to an outcome. Returns the correlation
// the wire submit carries so rows, drops and commits resolve by id.
func (v *attachView) submitPending(line string) string {
	id := newQueueID()
	v.send(func() {
		v.pending = append(v.pending, pendingRow{id: id, text: line})
		v.refreshQueue()
	})
	return id
}

// showOutcome flashes a one-line outcome on the hint line, removing itself
// after a few seconds.
func (v *attachView) showOutcome(text string) {
	if v.outcome != nil {
		v.outcome.Stop()
	}
	v.ui.SetStatusSegment(segment(segHint, text, ""))
	v.outcome = time.AfterFunc(4*time.Second, func() {
		v.ui.SetStatusSegment(segment(segHint, "", ""))
	})
}

// requestRedraw queues one full repaint from the transcript file. Coalesced:
// a second head move before the first repaint runs needs only the newest one.
func (v *attachView) requestRedraw() {
	if v.redrawQueued {
		return
	}
	v.redrawQueued = true
	v.send(func() {
		v.redrawQueued = false
		v.redraw()
	})
}

// repaint resets the screen and renders one whole branch, the single shape
// every head move shares: a backfill onto a moved head and the redraw a
// branch.changed event triggers.
func (v *attachView) repaint(branch []session.Entry) {
	v.ui.Reset()
	v.ui.Divider() // mark where the restored branch begins, like a local rewind
	v.rendered = bulk.SliceToSetBy(func(e session.Entry) string { return e.ID }, branch)
	v.renderHistory(branch)
}

// redraw re-renders the branch under the current head from the transcript file
// after a rewind or fork moved it, whether that move was local or came from the
// other terminal. It runs on the actor, so appends landing while it reads queue
// behind it and paint onto the fresh screen instead of being wiped by its Reset.
func (v *attachView) redraw() {
	v.mu.Lock()
	head, running := v.head, v.running
	v.mu.Unlock()
	branch, err := client.ReadBranch(v.transcript(), head)
	if err != nil {
		v.ui.Notify("refresh after rewind failed: "+err.Error(), tui.LevelWarn)
		return
	}
	v.repaint(branch)
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

// beginInteraction races a remote dialog against the local human: the first
// answer wins on the session side, and a settled event cancels this racer. The
// racer rides parent, the view root on live paths, so teardown takes any
// still-open one with it rather than whichever request happened to announce
// the dialog. Idempotent by id: a resnapshot re-listing a dialog already
// racing here starts nothing new.
func (v *attachView) beginInteraction(parent context.Context, d ctl.InteractionPendingData) {
	if _, racing := v.dialogs[d.ID]; racing {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	v.dialogs[d.ID] = cancel
	go v.raceDialog(ctx, d)
}

// reconcileDialogs brings this surface's racers to the snapshot's open set:
// dialogs still holding the run that no racer serves open locally, and
// racers whose dialogs settled while this terminal was away cancel. The
// watermark already dropped their events, so the socket's snapshot is the
// only word on what still holds. parent seeds any racer it opens; live
// paths pass the view root, never a request context, so a dialog never
// dies with the resnapshot that re-announced it.
func (v *attachView) reconcileDialogs(parent context.Context, list []ctl.InteractionPendingData) {
	live := bulk.SliceToSetBy(func(d ctl.InteractionPendingData) uint64 { return d.ID }, list)
	for id := range v.dialogs {
		if _, open := live[id]; !open {
			v.settleInteraction(id)
		}
	}
	for _, d := range list {
		v.beginInteraction(parent, d)
	}
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
	conn := v.connFor()
	if conn == nil {
		return // the socket dropped mid-dialog; the reconnect's resnapshot re-races it
	}
	err := conn.Resolve(rctx, ans)
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

// connFor reads the current connection off the actor, so a reconnect swap is
// never raced.
func (v *attachView) connFor() *client.Conn {
	var c *client.Conn
	if !v.call(func() { c = v.conn }) {
		return nil
	}
	return c
}

// settleInteraction dismisses one open local dialog whose remote twin settled.
func (v *attachView) settleInteraction(id uint64) {
	cancel := v.dialogs[id]
	delete(v.dialogs, id)
	if cancel != nil {
		cancel()
	}
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
		case len(opts) == 0 || ans.Chat:
			// a no-options ask is free text and an empty submit is a real (empty)
			// answer, matching the host terminal; only announced options make an
			// index meaningful
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
