package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/go-analyze/bulk"

	"github.com/jentfoo/ajent/pkg/client"
	"github.com/jentfoo/ajent/pkg/clipboard"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tui"
)

// submitBuffer bounds the editor-to-wire handoff; a wedged host delays
// further submissions rather than dropping them.
const submitBuffer = 16

// Reconnect backoff while the socket is down: first retry delay and its cap.
const (
	reconnectMax = 5 * time.Second

	// reconnectHintWait is the offline stretch before the attach-again hint
	// surfaces once. Retries continue at the backoff cap regardless.
	reconnectHintWait = 30 * time.Second
)

// reconnectFirst is the first retry delay, a var so tests shrink it.
var reconnectFirst = 250 * time.Millisecond

// reconnectGiveUp is the offline stretch after which attach stops retrying and
// exits with a distinct code, so a script can tell a lost session from a clean
// end. A var so tests can shrink it.
var reconnectGiveUp = 5 * time.Minute

// attachStableAfter splits a stable attach's fresh retry stretch from a young
// drop extending the running one. A var so tests can shrink it.
var attachStableAfter = 30 * time.Second

// reconnectPace is retry pacing shared across a run's attach cycles, so fast
// drops accumulate toward the give-up instead of restarting per cycle.
type reconnectPace struct {
	started time.Time     // when this offline stretch began; zero until set
	backoff time.Duration // the next attempt's wait before dialing
	hinted  bool          // the still-offline hint has fired this stretch
}

// attachOutcome is one attach session's end, deciding what the outer loop does.
type attachOutcome int

const (
	attachEnded    attachOutcome = iota // the remote run exited: session.exit arrived
	attachDetached                      // this terminal quit: the session keeps running
	attachDropped                       // the socket died without an exit: reconnect
	attachBackfill                      // connected but the transcript would not read: retry slower
)

// RunAttach mirrors the session behind o.Socket on this terminal: backfill,
// then the live event stream, with every input riding the control methods an
// out-of-process controller has. It opens no transcript and runs no agent loop;
// the remote run owns model, tools and permission mode. The exit code follows
// the same table as a local run: OK for a clean end or a detach, the
// failed-turn code for an end with a reason and for a lost session. A dropped
// socket reconnects rather than exiting bare, rediscovery and banners included.
func RunAttach(o AttachOptions) int {
	matches, live, err := client.Resolve(o.Socket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ajent: %v\n", err)
		explainNoControl(o.Socket, live)
		return ExitUsage
	}
	// a bare attach with nothing live explains itself like a targeted miss
	if o.Socket == "" && len(matches) == 0 {
		explainNoControl("", nil)
		return ExitUsage
	}

	ui, err := tui.New(tui.Options{Mode: o.Mode, Color: o.Color,
		Palette: o.Palette, Images: o.Images, Keymap: o.Keymap})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ajent: %v\n", err)
		return ExitUsage
	}
	defer ui.Close()
	for _, w := range o.Warnings {
		ui.Notify(w, tui.LevelWarn)
	}

	// the view's lifetime ties to this run: background requests die with it
	root, stopRoot := context.WithCancel(context.Background())
	defer stopRoot()

	// one instance dials direct; several open the picker, never a silent pick
	picked := matches
	if len(picked) != 1 {
		picked, err = pickInstances(root, ui, picked, o.Socket == "")
		if err != nil {
			ui.Close() // restore the terminal before explaining
			if errors.Is(err, tui.ErrCancelled) {
				return ExitOK
			}
			fmt.Fprintf(os.Stderr, "ajent: %v\n", err)
			return ExitUsage
		}
	}

	var hello ctl.Hello
	var conn *client.Conn
	view := newAttachView(ui, nil, root)
	holderName := fmt.Sprintf("ajent attach (pid %d)", os.Getpid())

	// one pump and one control reader serve every reconnect cycle: per-cycle
	// copies would leak the old goroutine and race for the terminal's channels
	quit := make(chan struct{})
	var quitOnce sync.Once
	stopLocal := func() { quitOnce.Do(func() { close(quit) }) }
	defer stopLocal()
	go attachControls(ui, view, root.Done(), stopLocal)
	submissions := make(chan string, submitBuffer)
	defer close(submissions)
	go pumpSubmissions(view, submissions)

	// run attaches once, then reconnects follow the state machine until the
	// session ends or this terminal detaches. One pace spans all its cycles.
	var backfillFails int
	var pace reconnectPace
	for {
		if conn == nil {
			conn, hello, err = client.Dial(picked[0].Socket)
			if err != nil {
				ui.Close() // restore the terminal before explaining
				fmt.Fprintf(os.Stderr, "ajent: %v\n", err)
				return ExitUsage
			}
			view.setHello(hello)
		}

		attachedAt := time.Now()
		outcome, exitReason := attachOnce(view, conn, hello, holderName, o.Force, submissions, quit)
		conn.Close()

		switch outcome {
		case attachEnded:
			return attachEnd(ui, exitReason, hello)
		case attachDetached:
			return ExitOK
		case attachDropped:
			backfillFails = 0
			view.connDown() // helpers report not connected until a reconnect swaps one in
		case attachBackfill:
			// the instance serves but its transcript does not read (deleted under
			// it, moved): back off before retrying, or the dial-success path spins
			backfillFails++
			view.connDown()
			ui.Notify("attach failed: the transcript could not be read; retrying", tui.LevelWarn)
			wait := min(reconnectFirst*time.Duration(1<<min(backfillFails, 4)), reconnectMax)
			select {
			case <-quit:
				return ExitOK
			case <-root.Done():
				return ExitOK
			case <-time.After(wait):
			}
		}

		// reconnect state machine: keep scrollback and the local draft (drafts
		// are local state by design), retry with backoff, re-discover by
		// session id, then resnapshot and render only the difference. Drops
		// past attachStableAfter open a fresh stretch, younger ones extend it.
		if time.Since(attachedAt) >= attachStableAfter {
			pace = reconnectPace{}
		}
		ready, gaveUp := view.reconnectLoop(ui, hello, root, &pace, quit)
		if gaveUp {
			ui.Notify("gave up reconnecting; attach again with: ajent --attach "+resumeLabel(hello), tui.LevelWarn)
			return ExitTurn
		}
		if !ready {
			return ExitOK // the user detached while reconnecting
		}
		conn, hello = view.connFor(), view.helloNow() // swapped in by the loop
	}
}

// resumeLabel names a hello's session the way --resume takes it back.
func resumeLabel(h ctl.Hello) string {
	if h.Session.Name != "" {
		return h.Session.Name
	}
	return h.Session.ID
}

// attachEnd paints the end of a run the remote session exited and returns the
// process exit code: OK clean, a reason a failed turn. Only the clean end
// carries the resume hint.
func attachEnd(ui *tui.UI, reason string, hello ctl.Hello) int {
	ui.Notify("the attached session ended", tui.LevelInfo)
	if reason == "" {
		// a clean end is definitive: the transcript stays, so the hint is
		// resume, never attach
		ui.Notify("resume it later with: ajent --resume "+resumeLabel(hello), tui.LevelInfo)
		return ExitOK
	}
	// the reason is the notice, then the failed-turn code
	ui.Notify(reason, tui.LevelWarn)
	return ExitTurn
}

// attachOnce runs one attach session to an outcome: backfill, seat claim, the
// event pump and the input loop. The reason is set only when the session
// itself exited. Every claim of the run rides force, so a dropped connection
// reclaims the seat it asked for at launch rather than returning an observer.
func attachOnce(view *attachView, conn *client.Conn, hello ctl.Hello, holderName string, force bool,
	submissions chan<- string, quit <-chan struct{}) (attachOutcome, string) {
	var exitReason string
	onExit := make(chan struct{}) // closed once per connection on session.exit
	var exitOnce sync.Once
	view.swapConn(conn)

	// the reader starts before anything requests: replies route through it, so
	// a call issued first would wait forever. Lifecycle surfaces (exit, seat)
	// apply immediately; everything else rides the actor.
	go conn.Serve(func(ev ctl.Event) {
		switch ev.Name {
		case ctl.EventSessionExit:
			var d ctl.SessionExitData
			_ = ev.Decode(&d)
			exitOnce.Do(func() {
				exitReason = d.Reason // reader-only until the exit closes, which orders the read
				close(onExit)
			})
			return
		case ctl.EventSeatLost:
			var d ctl.SeatLostData
			if ev.Decode(&d) == nil && d.From == holderName {
				// force-displaced: downgraded to observer without any write probing it
				view.seatLost(d.To)
			}
			return
		}
		view.handle(ev)
	})

	// the seat claim carries the standard request budget: a wedged-but-alive
	// host fails into the reconnect loop instead of hanging before any banner
	cctx, ccancel := context.WithTimeout(context.Background(), client.CallWait)
	control, err := conn.ClaimSeat(cctx, holderName, force)
	ccancel()
	var wire *ctl.Error
	switch {
	case err == nil:
	case errors.As(err, &wire):
		view.ui.Notify(wire.Message, tui.LevelWarn) // seat-held names the holder
	default:
		return attachDropped, ""
	}
	// a claim that hands back a refused or taken seat says so: silence would
	// make this terminal the controller while its last notice claimed read-only
	regained := control && view.takeDemoted()
	// queued ahead of the backfill below, so its dialogs reconcile under the
	// role the claim established
	view.setSeat(control)

	bctx, bcancel := context.WithTimeout(context.Background(), client.CallWait)
	berr := view.backfill(bctx)
	bcancel()
	if berr != nil {
		return attachBackfill, ""
	}

	ui := view.ui
	ui.Notify(fmt.Sprintf("attached to session %s (host pid %d)", resumeLabel(hello), hello.PID), tui.LevelInfo)
	if !control {
		ui.Notify("watching read-only: writes need the control seat", tui.LevelWarn)
	} else if regained {
		ui.Notify("you hold the control seat", tui.LevelInfo)
	}

	view.waitIdle() // the backfill snapshot owns idle, running chrome included
	view.installRecall()
	ui.SetOnRewind(func() { view.rewindPicker() })

	for {
		select {
		case line, ok := <-ui.Messages():
			if !ok {
				return attachDetached, "" // stdin EOF detaches, like a ctrl+c quit
			}
			select {
			case submissions <- line:
			case <-quit:
				return attachDetached, ""
			case <-onExit:
				return attachEnded, exitReason
			case <-conn.Done():
				// exit and EOF can land together. The exit is the truth when both
				// are ready, and the line stays in the editor through the reconnect
				select {
				case <-onExit:
					return attachEnded, exitReason
				default:
				}
				view.notConnected(line)
				return attachDropped, ""
			}
		case <-quit:
			return attachDetached, "" // the user detached; the session keeps running
		case <-onExit:
			return attachEnded, exitReason
		case <-conn.Done():
			// same preference: a raced exit never reads as a loss
			select {
			case <-onExit:
				return attachEnded, exitReason
			default:
			}
			return attachDropped, ""
		}
	}
}

// pumpSubmissions serializes one terminal's submits onto the wire in channel
// order, which is the queue order the remote pump applies. Only prompts paint
// a pending row: slash and shell lines commit no message entry, they surface
// through their effects. Each prompt carries its own correlation, so drops and
// commits resolve the row by id without any text matching.
func pumpSubmissions(view *attachView, submissions <-chan string) {
	for line := range submissions {
		conn := view.connFor()
		if conn == nil {
			view.notConnected(line)
			continue
		}
		params := ctl.SubmitParams{Line: line}
		if command.ParseLine(line).Kind == command.KindPrompt {
			params.PendingID = view.submitPending(line)
		}
		sctx, scancel := context.WithTimeout(context.Background(), client.CallWait)
		serr := conn.Submit(sctx, params)
		scancel()
		if serr != nil {
			view.submitFailed(params.PendingID, line, serr)
		}
	}
}

// reconnectLoop keeps trying until an instance of the session serves again,
// preferring the one it was on, following a sole survivor with a banner and
// opening the picker among several. Lines typed while offline are refused
// with their text kept for the editor. st carries pacing across attach
// cycles, so fast drops share one backoff and give-up window. It reports
// ready once connected, false when the user detached, and gaveUp once this
// stretch outlasts reconnectGiveUp, which callers exit as a failed turn.
func (v *attachView) reconnectLoop(ui *tui.UI, hello ctl.Hello, root context.Context,
	st *reconnectPace, quit <-chan struct{}) (ready, gaveUp bool) {
	ui.Notify("connection lost; reconnecting", tui.LevelWarn)
	if st.started.IsZero() {
		st.started = time.Now()
	}
	if st.backoff <= 0 {
		st.backoff = reconnectFirst
	}

	for {
		if !st.hinted && time.Since(st.started) >= reconnectHintWait {
			// the owed offline hint: attach again later, never resume
			ui.Notify("still offline; the session may have exited, attach again later with: ajent --attach "+
				resumeLabel(hello), tui.LevelWarn)
			st.hinted = true
		}
		if time.Since(st.started) >= reconnectGiveUp {
			return false, true
		}

		// every attempt waits first: typed lines refuse and re-wait, never
		// trigger an immediate dial
		select {
		case line, ok := <-ui.Messages():
			if !ok {
				return false, false // stdin EOF detaches, like a ctrl+c quit
			}
			v.notConnected(line)
			continue
		case <-quit:
			return false, false // the double-ctrl+c detach works mid-reconnect too
		case <-root.Done():
			return false, false
		case <-time.After(st.backoff):
		}
		st.backoff = min(st.backoff*2, reconnectMax)

		// live records only: record-only liveness already judged the dead ones,
		// whose sockets would waste the dial below
		live, err := client.DiscoverLive()
		if err != nil {
			continue // discovery failed, wait out the next round
		}
		follow := client.Followable(live, hello.Session.ID, hello.Token)
		if len(follow) == 0 {
			// Followable answers nil for several candidates of the session
			// as for none; refilter so the picker sees the several
			follow = bulk.SliceFilter(func(in client.Instance) bool {
				return in.SessionID == hello.Session.ID
			}, live)
		}
		switch len(follow) {
		case 1:
			next := follow[0]
			if next.Token != hello.Token && !next.Started().IsZero() {
				ui.Notify(fmt.Sprintf("following new instance of %s (pid %d, started %s)",
					next.Label(), next.PID, next.Started().Local().Format("15:04:05")), tui.LevelInfo)
			}
			conn, nh, derr := client.Dial(next.Socket)
			if derr == nil {
				// the resnapshot rides attachOnce's backfill, which renders
				// only the difference against the rendered set
				v.swapConn(conn)
				v.followInstance(nh)
				return true, false
			}
		case 0:
			// none serving yet
		default:
			picked, perr := pickInstancesFn(root, ui, follow, false)
			if perr != nil {
				if errors.Is(perr, tui.ErrCancelled) {
					return false, false
				}
				break // picker failed, retry next round
			}
			hello.Token = picked[0].Token // the user chose: follow it like a first attach
		}
	}
}

// pickInstancesFn is the reconnect picker seam, a var so tests fake the dialog.
var pickInstancesFn = pickInstances

// pickInstances runs the instance picker, mirroring the resume picker's mental
// model: workspace, started time, model and seat state per row.
func pickInstances(ctx context.Context, ui *tui.UI, live []client.Instance, bare bool) ([]client.Instance, error) {
	if len(live) == 0 {
		return nil, errors.New("no live control sessions")
	}
	items := make([]tui.PickItem, len(live))
	for i, in := range live {
		detail := in.Workspace
		if !in.Started().IsZero() {
			detail += " · started " + in.Started().Local().Format("2006-01-02 15:04")
		}
		if in.Model != "" {
			detail += " · " + in.Model
		}
		seat := "seat open"
		if in.SeatHeld {
			seat = "seat held"
		}
		detail += " · " + seat
		items[i] = tui.PickItem{Label: in.Label(), Detail: detail,
			Tag: in.SessionName, Terms: []string{in.SessionID}}
	}
	title := "Attach to instance"
	if bare {
		title = "Attach to session"
	}
	picked, err := ui.PickContext(ctx, title, items,
		tui.PickOptions{Placeholder: pickFilterPlaceholder})
	if err != nil {
		return nil, err
	}
	return []client.Instance{live[picked]}, nil
}

// explainNoControl distinguishes the empty answers: no live control
// sessions at all, versus sessions in the store that serve no control socket.
// It names control.enabled, points at the socket-free path, and when the
// target names a recorded session, at resuming that one directly.
func explainNoControl(target string, live []client.Instance) {
	if len(live) > 0 {
		fmt.Fprintln(os.Stderr, "live control sessions:")
		for _, in := range live {
			fmt.Fprintf(os.Stderr, "  %s\n", in.String())
		}
	}
	if target != "" {
		if name := storedSessionName(target); name != "" {
			fmt.Fprintf(os.Stderr, "resume without a socket instead: ajent --resume %s\n", name)
			return
		}
	}
	if len(live) > 0 {
		return
	}
	store, err := session.NewStore()
	if err == nil {
		stderrNote(store.Note())
		if rows, lerr := store.List(config.Cwd()); lerr == nil && len(rows) > 0 {
			fmt.Fprintf(os.Stderr,
				"ajent: no session is serving control; set control.enabled in config.json on the running instance\n"+
					"resume without a socket instead: ajent --resume <name> (or --continue)\n")
			return
		}
	}
	fmt.Fprintf(os.Stderr, "ajent: no live control sessions to attach to\n")
}

// storedSessionName resolves target against the recorded sessions --resume
// would search, naming the session a failed attach can still reach. Empty when
// nothing recorded answers to it.
func storedSessionName(target string) string {
	store, err := session.NewStore()
	if err != nil {
		return ""
	}
	info, err := store.Find(config.Cwd(), target)
	if err != nil {
		return ""
	}
	return info.Label()
}

// attachControls serves the terminal's out-of-band keys until this run ends:
// Esc and ctrl+c interrupt the remote session, a double ctrl+c detaches this
// process, Shift+Tab cycles the permission mode remotely. Everything else is
// local.
func attachControls(ui *tui.UI, view *attachView, done <-chan struct{}, stopLocal func()) {
	// the board is the view's, so wire hints and this terminal's own claims
	// (the quit arm, interrupt notices) arbitrate one hint line. The quit
	// gesture is shared with the host loop, labels only differ.
	hints := view.hints
	quit := quitArm{hints: hints, again: "ctrl+c again to detach", going: "detaching…"}

	for {
		select {
		case <-done:
			return // the run ended; tui never closes Controls itself
		case c, ok := <-ui.Controls():
			if !ok {
				return
			}
			switch c {
			case tui.ControlEscape:
				_, _ = attachInterrupt(ui, view) // a lone Esc is the interrupt key
			case tui.ControlInterrupt:
				stopped, err := attachInterrupt(ui, view)
				if err == nil && stopped != "" {
					if n := interruptNotice(stopped); n != "" {
						hints.ShowFor(n, "", hintNoticeTTL)
					}
					continue // something was running: the press did its work
				}
				if quit.fire() {
					stopLocal()
					return
				}
			case tui.ControlModeCycle:
				attachModeCycle(ui, view, false)
			case tui.ControlModeCycleBack:
				attachModeCycle(ui, view, true)
			case tui.ControlClipboardImage:
				ui.Notify("clipboard images cannot ride an attach; paste text instead", tui.LevelWarn)
			case tui.ControlCopySelection:
				if text, ok := ui.CopySelection(); ok {
					go func() {
						cctx, ccancel := context.WithTimeout(context.Background(), client.CallWait)
						defer ccancel()
						// local clipboard: never crosses the socket
						if err := clipboardWrite(cctx, text,
							clipboard.CopyOptions{TTY: ui.TerminalWriter()}); err == nil {
							ui.Notify("copied to the clipboard", tui.LevelInfo)
						} else {
							ui.Notify(err.Error(), tui.LevelError)
						}
					}()
				}
			case tui.ControlRecallQueued:
				attachRecallQueued(ui, view)
			}
		}
	}
}

// attachInterrupt runs the Esc body remotely, surfacing a refusal.
func attachInterrupt(ui *tui.UI, view *attachView) (string, error) {
	conn := view.connFor()
	if conn == nil {
		return "", errors.New("not connected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), client.CallWait)
	defer cancel()
	stopped, err := conn.Interrupt(ctx)
	if err != nil {
		ui.Notify("interrupt failed: "+err.Error(), tui.LevelWarn)
	}
	return stopped, err
}

// attachModeCycle steps the remote permission mode, surfacing a refusal.
func attachModeCycle(ui *tui.UI, view *attachView, back bool) {
	conn := view.connFor()
	if conn == nil {
		ui.Notify("mode change failed: not connected", tui.LevelWarn)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), client.CallWait)
	defer cancel()
	if _, err := conn.ModeCycle(ctx, back); err != nil {
		ui.Notify("mode change failed: "+err.Error(), tui.LevelWarn)
	}
}

// attachRecallQueued runs Alt+Up against the served run: the newest queued
// prompt pops from the session's queue into this terminal's editor, so editing
// and resubmitting here replaces it. Nothing queued is a silent no-op,
// matching the local key; the recalled row resolves through its drop event.
func attachRecallQueued(ui *tui.UI, view *attachView) {
	conn := view.connFor()
	if conn == nil {
		ui.Notify("recall failed: not connected", tui.LevelWarn)
		return
	}
	rctx, rcancel := context.WithTimeout(context.Background(), client.CallWait)
	defer rcancel()
	row, err := conn.QueueRecall(rctx)
	var wire *ctl.Error
	switch {
	case err == nil:
		if row.Text != "" {
			ui.PrependInput(row.Text)
		}
	case errors.As(err, &wire):
		ui.Notify("recall refused: "+wire.Message, tui.LevelWarn)
	default:
		ui.Notify("recall failed: "+err.Error(), tui.LevelWarn)
	}
}

// rewindPicker opens the context-tree picker over the transcript file and
// rewinds the session onto the chosen entry. The whole recorded tree reads from
// the same file the host terminal uses, so abandoned forks are pickable here
// exactly like a local double-Esc shows them.
func (v *attachView) rewindPicker() {
	conn := v.connFor()
	if conn == nil {
		v.ui.Notify("rewind: not connected", tui.LevelWarn)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), client.CallWait)
	snap, _, err := conn.State(ctx)
	cancel()
	if err != nil {
		v.ui.Notify("rewind: "+err.Error(), tui.LevelWarn)
		return
	}
	entries, _, rerr := session.Read(v.transcript()) // whole tree, forks included
	if rerr != nil {
		v.ui.Notify("rewind: "+rerr.Error(), tui.LevelWarn)
		return
	}
	// head is wire-fresh: the tree matches what the session sits on now
	rowID, ok := rewindPick(v.ui, entries, snap.Head, v.armPick)
	if !ok {
		return // cancelled, or nothing rewindable
	}
	if rerr := v.rewindToRow(conn, entries, rowID); rerr != nil {
		v.ui.Notify("rewind: "+rerr.Error(), tui.LevelWarn)
	}
}

// rewindToRow names rowID as the new head over the wire and pre-fills this
// editor with its prompt, matching what a local double-Esc lands on: edit or
// resend it as the restored branch's first message.
func (v *attachView) rewindToRow(conn *client.Conn, entries []session.Entry, rowID string) error {
	rctx, rcancel := context.WithTimeout(context.Background(), client.CallWait)
	newHead, err := conn.Rewind(rctx, rowID)
	rcancel()
	if err != nil {
		return err
	}
	if _, fill, ok := session.RewindTarget(entries, rowID); ok && fill != "" {
		v.ui.SetInput(fill)
	}
	v.setHeadNow(newHead) // the branch.changed event redraws every view including this one
	return nil
}
