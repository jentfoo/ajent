package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/client"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/strutil"
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

	mu       sync.Mutex         // guards only what the UI goroutine's callbacks read
	head     string             // last known transcript head entry id
	running  bool               // remote turn in flight; drives SetIdle so Esc keeps its interrupt role
	hello    ctl.Hello          // greeting of the connection in use
	pickStop context.CancelFunc // armed while the rewind picker is open

	// hints arbitrates the hint segment: the host's wire hints, this terminal's
	// quit arm and queue-drop outcomes all claim one line
	hints *hintBoard

	// msgs is the actor mailbox. Backfill-before-buffered-events holds by queue
	// order, never a flag.
	msgs   chan func()
	closed chan struct{}

	// actor-owned state below; only run() touches it
	conn         *client.Conn
	rendered     map[string]struct{} // committed entry ids this surface already shows
	pending      []pendingRow        // this terminal's submitted prompts awaiting their entry
	wireQueued   []ctl.QueueRow      // the last queue copy, for pending merges
	dialogs      map[uint64]*openDialog
	holder       bool        // this connection holds the control seat; only a holder answers dialogs
	demoted      bool        // the last seat news was read-only: a later regain owes its own notice
	live         bool        // backfill completed; events apply instead of buffering
	buf          []ctl.Event // events seen before backfill, applied against its watermark
	redrawQueued bool        // a full repaint is already queued; another head move needs none
}

// transcript reads the transcript file path from the greeting of the
// connection in use.
func (v *attachView) transcript() string {
	return v.helloNow().TranscriptPath
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

// followInstance adopts a reconnect's greeting. A different serving instance
// drops the pre-live buffer and settles every tracked dialog: that hub
// restarted its seqs and dialog ids at 1, so the previous instance's buffered
// events would pass the new watermark and paint stale deltas, and its dialog
// entries could collide with fresh ids. The same instance keeps both, one seq
// space bridging events between connect and snapshot.
func (v *attachView) followInstance(h ctl.Hello) {
	v.call(func() {
		v.mu.Lock()
		changed := v.hello.Token != h.Token
		v.hello = h
		v.mu.Unlock()
		if changed {
			v.buf = nil
			for _, id := range bulk.MapKeysSlice(v.dialogs) {
				v.settleInteraction(id)
			}
		}
	})
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

// openDialog is one dialog this surface tracks: the announcement a viewer's
// notice renders from, and the racer's cancel while this terminal may answer.
type openDialog struct {
	data   ctl.InteractionPendingData
	cancel context.CancelFunc // nil while a viewer only shows the notice
}

// submitFailed resolves one submission's pending row inline with the reply's
// error and restores the text, matching the serve-mode prefill rule.
func (v *attachView) submitFailed(id, line string, err error) {
	v.send(func() {
		v.retainPending(func(p pendingRow) bool { return p.id != id })
		v.refreshQueue()
	})
	v.ui.Notify("submit failed: "+err.Error()+"; the text is kept in the editor", tui.LevelWarn)
	v.ui.PrependInput(line)
}

// newAttachView builds the view; conn may be nil until swapConn installs one.
// root is cancelled at teardown, taking any in-flight background redraw with it.
func newAttachView(ui *tui.UI, conn *client.Conn, root context.Context) *attachView {
	v := &attachView{
		ui: ui, sink: tuisink.New(ui), conn: conn, root: root,
		dialogs: make(map[uint64]*openDialog),
		msgs:    make(chan func(), 256),
		closed:  make(chan struct{}),
	}
	// one board owns the hint line, so the wire's hint segments and every local
	// claim arbitrate by recency instead of overwriting each other
	v.hints = newHintBoard(func(text, short string) {
		ui.SetStatusSegment(segment(segHint, text, short))
	})
	wireHint := v.hints.Line()
	v.ap = newApplier(ui, v.sink, applyHooks{
		queueRows:     v.queueTexts,
		dropped:       v.queueDropped,
		running:       v.setRunning,
		hint:          wireHint.Set,
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
	v.reconcileStalePending(snap.QueuedRows)
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
		stop := v.pickStop
		v.mu.Unlock()
		if stop != nil {
			// a rewind from the other surface closes the picker it orphaned, so
			// it never answers with a tree the head has left
			stop()
		}
		if d.Cause == rewindCauseCompact {
			return // the compaction entry itself arrives via entry.append
		}
		v.requestRedraw()
	default:
		v.ap.Apply(ev)
	}
}

// dropOutcome names a queue removal honestly: a recall returns the text to an
// editor, everything else drops it. An unnamed cause is a cleared one. Count
// reads naturally, since an interrupt or rewind clears several rows at once.
func dropOutcome(cause string, n int) string {
	switch cause {
	case "recalled":
		if n > 1 {
			return fmt.Sprintf("%d prompts recalled for editing", n)
		}
		return "prompt recalled for editing"
	case "":
		cause = "cleared"
	}
	if n > 1 {
		return fmt.Sprintf("%d prompts dropped (%s)", n, cause)
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
// flashes one outcome naming them on the hint line.
func (v *attachView) queueDropped(d ctl.QueueDroppedData) {
	v.resolvePending(d.Rows)
	v.showOutcome(dropOutcome(d.Cause, len(d.Rows)))
}

// applyAppend applies the wire rule for committed entries: ids already
// rendered mark silently, a pending row resolves when the prompt it submitted
// commits and its committed rendering takes the row's place, and everything
// else renders through the one echo rule. EchoEntry paints nothing for shapes
// the stream owns (assistant content, tool carriers, unmarked injected
// context), so what draws here is exactly what replay would draw for the same
// entry, mid-turn included.
func (v *attachView) applyAppend(e ctl.Entry) {
	// appends extend the head silently; only a branch.changed redraws
	v.setHeadNow(e.ID)
	if _, ok := v.rendered[e.ID]; ok {
		return // replayed or already committed here
	}
	v.rendered[e.ID] = struct{}{}
	entry := session.Entry{ID: e.ID, ParentID: e.ParentID,
		Type: session.Type(e.Type), TS: e.TS, Data: e.Data}
	if v.settleCommit(entry) {
		// the row resolved with its entry: repaint without it
		v.refreshQueue()
	}
	v.renderEntry(entry)
}

// settleCommit reports whether one committed entry is a user message that
// resolves any of this terminal's pending rows.
func (v *attachView) settleCommit(e session.Entry) bool {
	if e.Type != session.TypeMessage {
		return false
	}
	var md session.MessageData
	return e.Decode(&md) == nil && md.Message.Role == llm.RoleUser &&
		v.takeCommitted(md.QueueIDs)
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
		if v.settleCommit(e) {
			resolved = true
		}
	}
	if resolved {
		v.refreshQueue()
	}
}

// reconcileStalePending drops pending rows a disconnect orphaned: neither the
// branch nor the snapshot's queue names them, so the session settled them while
// this terminal was away and their drop event was lost with the connection. A
// row still queued server-side survives; a prompt whose entry lands a moment
// later loses its row early, its committed echo replacing it.
func (v *attachView) reconcileStalePending(rows []ctl.QueueRow) {
	if len(v.pending) == 0 {
		return
	}
	queued := bulk.SliceToSetBy(func(r ctl.QueueRow) string { return r.ID }, rows)
	dropped := v.retainPending(func(p pendingRow) bool {
		_, inflight := queued[p.id]
		return inflight
	})
	if dropped == 0 {
		return
	}
	v.refreshQueue()
	v.showOutcome(strutil.Plural(dropped, "prompt") + " dropped while disconnected")
}

// withPending merges this terminal's unresolved submissions into the wire's
// queue rows for display: a row the host already shows stays single, a secret
// row redacted on the wire renders from its local copy, and rows this terminal
// did not submit keep their own texts.
func (v *attachView) withPending(rows []ctl.QueueRow) []string {
	out := make([]string, 0, len(rows)+len(v.pending))
	wired := bulk.SliceToSetBy(func(r ctl.QueueRow) string { return r.ID }, rows)
	pending := bulk.SliceToIndexBy(func(p pendingRow) string { return p.id }, v.pending)
	for _, r := range rows {
		if r.Text != "" {
			out = append(out, r.Text)
			continue
		}
		// redacted on the wire: only this terminal's own submission can name it
		if p, ok := pending[r.ID]; ok {
			out = append(out, p.text)
		}
	}
	for _, p := range v.pending { // not yet echoed by a queued event
		if _, ok := wired[p.id]; !ok {
			out = append(out, p.text)
		}
	}
	return out
}

// takeCommitted consumes the pending rows a committed user prompt resolves:
// every row whose correlation rode the entry, joined runs included. Reports
// whether any row resolved.
func (v *attachView) takeCommitted(ids []string) bool {
	if len(ids) == 0 || len(v.pending) == 0 {
		return false
	}
	landed := bulk.SliceToSet(ids)
	resolved := v.retainPending(func(p pendingRow) bool {
		_, done := landed[p.id]
		return !done
	})
	return resolved > 0
}

// resolvePending clears the pending rows a drop event named.
func (v *attachView) resolvePending(rows []ctl.QueueRow) {
	dropped := bulk.SliceToSetBy(func(r ctl.QueueRow) string { return r.ID }, rows)
	v.retainPending(func(p pendingRow) bool {
		_, gone := dropped[p.id]
		return !gone
	})
}

// retainPending drops the pending rows keep rejects, reporting how many went.
func (v *attachView) retainPending(keep func(pendingRow) bool) int {
	kept := bulk.SliceFilter(keep, v.pending)
	dropped := len(v.pending) - len(kept)
	v.pending = kept
	return dropped
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

// showOutcome flashes a one-line outcome on the hint line, releasing it after
// the one TTL every control notice shares.
func (v *attachView) showOutcome(text string) {
	v.hints.ShowFor(text, "", hintNoticeTTL)
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

// beginInteraction tracks one open dialog on this surface. A seat holder
// races a local answer against the remote dialog: the first answer wins on the
// session side, and a settled event cancels this racer. The racer rides
// parent, the view root on live paths, so teardown takes any still-open one
// with it rather than whichever request happened to announce the dialog. A
// viewer never races, the server refusing its answers: the dialog records
// itself and surfaces as a notice. Idempotent by id either way, and a dialog
// first noticed as a viewer races once the seat is held.
func (v *attachView) beginInteraction(parent context.Context, d ctl.InteractionPendingData) {
	dlg, tracked := v.dialogs[d.ID]
	if tracked && dlg.cancel != nil {
		return // already racing
	}
	if !v.holder {
		if tracked {
			return // already noticed
		}
		v.dialogs[d.ID] = &openDialog{data: d}
		v.ui.Notify(heldNotice(d), tui.LevelInfo)
		return
	}
	ctx, cancel := context.WithCancel(parent)
	v.dialogs[d.ID] = &openDialog{data: d, cancel: cancel}
	go v.raceDialog(ctx, d)
}

// reconcileDialogs brings this surface's tracked dialogs to the snapshot's
// open set: dialogs still holding the run that nothing tracks here open for
// the seat's role, and tracked dialogs the snapshot no longer lists settle.
// The watermark already dropped their events, so the socket's snapshot is the
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
		// the socket dropped mid-dialog; the yield lets the reconnect's
		// resnapshot re-race the dialog it still lists
		v.yieldDialog(d.ID)
		return
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
		// the dialog may still hold: yield so a resnapshot can re-race it
		v.yieldDialog(d.ID)
	}
}

// armPick installs or clears the open rewind picker's cancel hook; a
// branch.changed trips it, so a remote rewind closes the tree this picker
// still shows.
func (v *attachView) armPick(stop context.CancelFunc) {
	v.mu.Lock()
	v.pickStop = stop
	v.mu.Unlock()
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

// settleInteraction dismisses one tracked dialog whose remote twin settled.
func (v *attachView) settleInteraction(id uint64) {
	d := v.dialogs[id]
	delete(v.dialogs, id)
	if d != nil && d.cancel != nil {
		d.cancel()
	}
}

// yieldDialog demotes one racer's entry back to un-raced, cancelling the
// racer's context so a resnapshot still listing the dialog re-opens it under
// the seat's role. No-op once the dialog settled or re-raced.
func (v *attachView) yieldDialog(id uint64) {
	v.send(func() {
		if d, ok := v.dialogs[id]; ok && d.cancel != nil {
			d.cancel()
			d.cancel = nil
		}
	})
}

// setSeat records whether this connection holds the control seat after a
// claim. Losing the seat closes racers this terminal can no longer answer and
// notices what still holds input, remembering the downgrade so a later regain
// can correct it. The claim's caller queues it ahead of its backfill, so the
// snapshot's dialogs meet the role they arrived under.
func (v *attachView) setSeat(control bool) {
	v.send(func() {
		if !control {
			// every downgrade renders a read-only notice; a reclaim must answer it
			v.demoted = true
		}
		if v.holder == control {
			return
		}
		v.holder = control
		if !control {
			v.watchDialogs()
		}
	})
}

// takeDemoted reports whether this user was last told they watch read-only,
// clearing it so a regained seat announces itself once per loss.
func (v *attachView) takeDemoted() bool {
	var due bool
	v.call(func() {
		due = v.demoted
		v.demoted = false
	})
	return due
}

// seatLost downgrades to a viewer after a forced takeover, naming who drives
// now. Racers close the way setSeat(false) closes them.
func (v *attachView) seatLost(to string) {
	v.setSeat(false)
	v.ui.Notify("control seat taken by "+seatLabel(to)+"; watching read-only", tui.LevelWarn)
}

// watchDialogs converts every open racer into a viewer's notice: the server
// refuses a viewer's answers, so the input UI closes and the dialog stays
// visible as information instead.
func (v *attachView) watchDialogs() {
	ids := bulk.MapKeysSlice(v.dialogs)
	slices.Sort(ids)
	for _, id := range ids {
		d := v.dialogs[id]
		if d.cancel == nil {
			continue // already watching
		}
		d.cancel()
		d.cancel = nil
		v.ui.Notify(heldNotice(d.data), tui.LevelInfo)
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
	case kindPick:
		// silent: the choice's effects announce themselves through wire notices,
		// so a second local summary line would only duplicate them
		i, err := dl.PickContext(ctx, d.Prompt, pickItems(opts), tui.PickOptions{Silent: true})
		if err != nil {
			return noUIOrCancel(err, cancel, skip)
		}
		return ctl.ResolveParams{ID: d.ID, Index: &i}, true
	case kindMultiPick:
		idx, err := dl.MultiPickContext(ctx, d.Prompt, pickItems(opts), tui.MultiPickOptions{})
		if err != nil {
			return noUIOrCancel(err, cancel, skip)
		}
		return ctl.ResolveParams{ID: d.ID, Indexes: &idx}, true
	case kindSelect:
		i, err := dl.SelectContext(ctx, d.Prompt, tuiOptions(opts))
		if err != nil {
			return noUIOrCancel(err, cancel, skip)
		}
		return ctl.ResolveParams{ID: d.ID, Index: &i}, true
	case kindConfirm:
		yes, err := dl.ConfirmContext(ctx, d.Prompt)
		if err != nil {
			return noUIOrCancel(err, cancel, skip)
		}
		i := boolToInt(yes) // "No" is the offered pair's second row
		return ctl.ResolveParams{ID: d.ID, Index: &i}, true
	case kindInput, kindSecret:
		var text string
		var err error
		if d.Kind == kindSecret {
			text, err = dl.InputSecretContext(ctx, d.Prompt, "")
		} else {
			text, err = dl.InputContext(ctx, d.Prompt, "")
		}
		if err != nil {
			return noUIOrCancel(err, cancel, skip)
		}
		return ctl.ResolveParams{ID: d.ID, Text: &text}, true
	case kindAsk:
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
	case kindDecision:
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

// dialogNoun names one dialog kind for a viewer's notice.
func dialogNoun(kind string) string {
	switch kind {
	case kindPick:
		return "picker"
	case kindMultiPick:
		return "multi-pick"
	case kindSelect:
		return "choice"
	case kindConfirm:
		return "confirmation"
	case kindInput:
		return "input"
	case kindSecret:
		return "secret input"
	case kindAsk:
		return "question"
	case kindDecision:
		return "approval"
	default:
		return "dialog"
	}
}

// heldNotice words one open dialog for a viewer: what holds input and that the
// answer belongs to the control seat.
func heldNotice(d ctl.InteractionPendingData) string {
	text := dialogNoun(d.Kind) + " waiting on the control seat: " + d.Prompt
	if subj := strutil.Clip(strutil.FirstLine(d.Subject), 60); subj != "" {
		text += " (" + subj + ")"
	}
	return text
}
