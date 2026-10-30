package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

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

// reconnectBackoff bounds the retry cadence while the socket is down.
const (
	reconnectFirst = 250 * time.Millisecond
	reconnectMax   = 5 * time.Second

	// reconnectHintWait is the offline stretch before the attach-again hint
	// surfaces once. Retries continue at the backoff cap regardless.
	reconnectHintWait = 30 * time.Second
)

// reconnectGiveUp is the offline stretch after which attach stops retrying and
// exits with a distinct code, so a script can tell a lost session from a clean
// end. A var so tests can shrink it.
var reconnectGiveUp = 5 * time.Minute

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
// the same table as a local run. A dropped socket reconnects rather than
// exiting bare, rediscovery and banners included.
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
		Palette: o.Palette, Images: o.Images})
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

	// one instance connects directly, several open the picker (never a silent
	// pick), and bare --attach picks among every live session
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

	hello := ctl.Hello{}
	var conn *client.Conn
	view := newAttachView(ui, nil, root, "")
	holderName := fmt.Sprintf("ajent attach (pid %d)", os.Getpid())

	// one pump and one control reader serve every reconnect cycle: per-cycle
	// copies would leak the old goroutine and race for the terminal's channels
	quit := make(chan struct{})
	var quitOnce sync.Once
	stopLocal := func() { quitOnce.Do(func() { close(quit) }) }
	defer stopLocal()
	go attachControls(ui, view, stopLocal)
	submissions := make(chan string, submitBuffer)
	defer close(submissions)
	go pumpSubmissions(view, submissions)

	// run attaches once, then reconnects follow the state machine until the
	// session ends or this terminal detaches
	var backfillFails int
	for {
		if conn == nil {
			conn, hello, err = client.Dial(picked[0].Socket)
			if err != nil {
				ui.Close() // restore the terminal before explaining
				fmt.Fprintf(os.Stderr, "ajent: %v\n", err)
				return ExitUsage
			}
			view.setHello(hello)
			view.setTranscript(hello.TranscriptPath)
		}

		outcome, exitReason := attachOnce(view, conn, hello, holderName, submissions, quit)
		conn.Close()

		switch outcome {
		case attachEnded:
			// a clean end is definitive: the transcript stays, so the hint is
			// resume, never attach
			ui.Notify("the attached session ended", tui.LevelInfo)
			if exitReason != "" {
				ui.Notify(exitReason, tui.LevelWarn)
			}
			ui.Notify("resume it later with: ajent --resume "+resumeLabel(hello), tui.LevelInfo)
			return ExitOK
		case attachDetached:
			return ExitOK
		case attachDropped:
			backfillFails = 0
			view.connDown() // helpers report not connected until a reconnect swaps one in
		case attachBackfill:
			// the instance serves but its transcript does not read (deleted under
			// it, moved): back off, or the dial-success path spins the seat
			backfillFails++
			view.connDown()
			ui.Notify("attach failed: the transcript could not be read; retrying", tui.LevelWarn)
		}
		if backfillFails > 0 {
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
		// session id, then resnapshot and render only the difference
		ready, gaveUp := view.reconnectLoop(ui, hello, root, quit)
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

// attachOnce runs one attach session to an outcome: backfill, seat claim, the
// event pump and the input loop. The reason is set only when the session
// itself exited.
func attachOnce(view *attachView, conn *client.Conn, hello ctl.Hello, holderName string,
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
				view.ui.Notify("control seat taken by "+seatLabel(d.To)+"; watching read-only", tui.LevelWarn)
			}
			return
		}
		view.handle(ev)
	})

	// the seat claim carries the standard request budget: a wedged-but-alive
	// host fails into the reconnect loop instead of hanging before any banner
	cctx, ccancel := context.WithTimeout(context.Background(), client.CallWait)
	control, err := conn.ClaimSeat(cctx, holderName, false)
	ccancel()
	var wire *ctl.Error
	switch {
	case err == nil:
	case errors.As(err, &wire):
		view.ui.Notify(wire.Message, tui.LevelWarn) // seat-held names the holder
	default:
		return attachDropped, ""
	}

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

// reconnectLoop keeps trying until one instance of the session serves again.
// It prefers the same instance; one other live instance is followed with a
// naming banner; several open the picker, never a silent swap. Lines typed
// while offline are refused with their text kept, never held by the retry. It
// reports whether a connection is ready, false when the user detached, and
// true in gaveUp when the offline stretch outlasts reconnectGiveUp: a lost
// session exits with a distinct code instead of retrying forever.
func (v *attachView) reconnectLoop(ui *tui.UI, hello ctl.Hello, root context.Context, quit <-chan struct{}) (ready, gaveUp bool) {
	ui.Notify("connection lost; reconnecting", tui.LevelWarn)

	backoff := reconnectFirst
	started := time.Now()
	hinted := false
	for {
		if !hinted && time.Since(started) >= reconnectHintWait {
			// the hint a drop eventually owes: attaching again, never a resume,
			// the session may be gone for good
			ui.Notify("still offline; the session may have exited, attach again later with: ajent --attach "+
				resumeLabel(hello), tui.LevelWarn)
			hinted = true
		}
		if time.Since(started) >= reconnectGiveUp {
			return false, true
		}
		live, err := client.Discover()
		if err == nil {
			follow := client.Followable(live, hello.Session.ID, hello.Token)
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
					v.setHello(nh)
					return true, false
				}
			case 0:
				// none serving yet; fall through to the wait
			default:
				picked, perr := pickInstances(root, ui, follow, false)
				if perr != nil {
					if errors.Is(perr, tui.ErrCancelled) {
						return false, false
					}
					continue // picker failed; keep retrying rather than die
				}
				hello.Token = picked[0].Token // the user chose: follow it like a first attach
				continue                      // dial through the same path next round
			}
		}
		select {
		case line, ok := <-ui.Messages():
			if !ok {
				return false, false // stdin EOF detaches, like a ctrl+c quit
			}
			v.notConnected(line)
		case <-quit:
			return false, false // the double-ctrl+c detach works mid-reconnect too
		case <-root.Done():
			return false, false
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, reconnectMax)
	}
}

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

// attachControls serves the terminal's out-of-band keys until quit: Esc and
// ctrl+c interrupt the remote session, a double ctrl+c detaches this process,
// Shift+Tab cycles the permission mode remotely. Everything else is local.
func attachControls(ui *tui.UI, view *attachView, stopLocal func()) {
	// the board is the view's, so wire hints and this terminal's own claims
	// (the quit arm, interrupt notices) arbitrate one hint line. The quit
	// gesture is shared with the host loop, labels only differ.
	hints := view.hints
	quit := quitArm{hints: hints, again: "ctrl+c again to detach", going: "detaching…"}

	for c := range ui.Controls() { // the channel closing is the UI going away
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
	text, err := conn.QueueRecall(rctx)
	var wire *ctl.Error
	switch {
	case err == nil:
		if text != "" {
			ui.PrependInput(text)
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
