package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/jentfoo/ajent/pkg/client"
	"github.com/jentfoo/ajent/pkg/clipboard"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tui"
)

// submitBuffer bounds the editor-to-wire handoff; a wedged host delays
// further submissions rather than dropping them.
const submitBuffer = 16

// RunAttach mirrors the session behind o.Socket on this terminal: backfill,
// then the live event stream, with every input riding the control methods an
// out-of-process controller has. It opens no transcript and runs no agent loop;
// the remote run owns model, tools and permission mode. The exit code follows
// the same table as a local run.
func RunAttach(o AttachOptions) int {
	path, live, err := client.Resolve(o.Socket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ajent: %v\n", err)
		listLiveSessions(live)
		return ExitUsage
	}
	conn, hello, err := client.Dial(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ajent: %v\n", err)
		listLiveSessions(live)
		return ExitUsage
	}
	defer conn.Close()

	// the view's background requests (a head-driven redraw) die with this run
	root, stopRoot := context.WithCancel(context.Background())
	defer stopRoot()

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

	view := newAttachView(ui, conn, root)

	// the reader starts before anything requests: replies route through it, so a
	// call issued first would wait forever. Lifecycle surfaces (dialogs, exit)
	// apply immediately; everything else rides the view's backfill buffer.
	exited := make(chan struct{})
	var exitOnce sync.Once
	exitReason := ""
	go conn.Serve(func(ev ctl.Event) {
		switch ev.Name {
		case ctl.EventSessionExit:
			var d ctl.SessionExitData
			_ = ev.Decode(&d)
			exitReason = d.Reason // reader-only until exited closes, which orders the read
			exitOnce.Do(func() { close(exited) })
			return
		case ctl.EventInteractionPending:
			var d ctl.InteractionPendingData
			if ev.Decode(&d) == nil {
				view.beginInteraction(d)
			}
			return
		case ctl.EventInteractionSettled:
			var d ctl.InteractionSettledData
			if ev.Decode(&d) == nil {
				view.settleInteraction(d.ID)
			}
			return
		}
		view.handle(ev)
	})

	// the seat makes this connection the session's one remote controller; a held
	// seat degrades to watching instead of fighting over it
	control, err := conn.ClaimSeat(context.Background(),
		fmt.Sprintf("ajent attach (pid %d)", os.Getpid()), false)
	var wire *ctl.Error
	switch {
	case err == nil:
	case errors.As(err, &wire):
		ui.Notify(wire.Message, tui.LevelWarn) // seat-held names the holder
	default:
		ui.Close() // restore the terminal before explaining
		fmt.Fprintf(os.Stderr, "ajent: attach refused: %v\n", err)
		return ExitTurn
	}

	ctx, cancel := context.WithTimeout(context.Background(), client.CallWait)
	berr := view.backfill(ctx)
	cancel()
	if berr != nil {
		ui.Close() // restore the terminal before explaining
		fmt.Fprintf(os.Stderr, "ajent: reading the session: %v\n", berr)
		return ExitTurn
	}

	label := hello.Session.ID
	if hello.Session.Name != "" {
		label = hello.Session.Name
	}
	ui.Notify(fmt.Sprintf("attached to session %s (host pid %d)", label, hello.PID), tui.LevelInfo)
	if !control {
		ui.Notify("watching read-only: writes need the control seat", tui.LevelWarn)
	}

	view.setRunningIdle() // arm rewind only while the session is at rest
	view.installRecall()  // ↑/↓ recall and ctrl+r search read this session's prompts over the wire

	// submissions serialize through one worker so rapid lines keep their order
	// on the wire, which is the queue order the remote pump applies.
	submissions := make(chan string, submitBuffer)
	go func() {
		for line := range submissions {
			sctx, scancel := context.WithTimeout(context.Background(), client.CallWait)
			err := conn.Submit(sctx, line)
			scancel()
			if err != nil {
				ui.Notify("submit failed: "+err.Error(), tui.LevelWarn)
			}
		}
	}()

	quit := make(chan struct{})
	var quitOnce sync.Once
	stopLocal := func() { quitOnce.Do(func() { close(quit) }) }
	go attachControls(ui, conn, stopLocal)

	// rewind rides the same double-Esc gesture as a local run; the picker reads
	// the tree from the wire and names the head remotely
	ui.SetOnRewind(func() { view.rewindPicker() })

	for {
		select {
		case line, ok := <-ui.Messages():
			if !ok {
				stopLocal()
				return ExitOK // stdin EOF detaches, like a ctrl+c quit
			}
			select {
			case submissions <- line:
			case <-quit:
				return ExitOK
			case <-exited:
				return attachRemoteEnded(ui, exitReason)
			case <-conn.Done():
				return attachConnectionLost(ui, view, exited)
			}
		case <-quit:
			return ExitOK // the user detached; the session keeps running
		case <-exited:
			return attachRemoteEnded(ui, exitReason)
		case <-conn.Done():
			return attachConnectionLost(ui, view, exited)
		}
	}
}

// attachControls serves the terminal's out-of-band keys until quit: Esc and
// ctrl+c interrupt the remote session, a double ctrl+c detaches this process,
// Shift+Tab cycles the permission mode remotely. Everything else is local.
func attachControls(ui *tui.UI, conn *client.Conn, stopLocal func()) {
	// armed is when the first idle ctrl+c landed; inside the window a second one
	// detaches. The hint line holds for exactly the window, like a local quit's.
	var armed time.Time
	var quitHint <-chan time.Time
	hints := newHintBoard(func(text, short string) {
		ui.SetStatusSegment(segment(segHint, text, short))
	})
	arm := hints.Line()

	for {
		select {
		case <-quitHint:
			quitHint = nil
			arm.Set("", "") // the window closed unanswered: free the line
			continue
		case c, ok := <-ui.Controls():
			if !ok {
				return // the UI went away
			}
			switch c {
			case tui.ControlEscape:
				_, _ = attachInterrupt(ui, conn) // a lone Esc is the interrupt key
			case tui.ControlInterrupt:
				stopped, err := attachInterrupt(ui, conn)
				if err == nil && stopped != "" {
					continue // something was running: the press did its work
				}
				if !armed.IsZero() && time.Since(armed) < doublePressWindow {
					hints.Request("detaching…", "")
					stopLocal()
					return
				}
				armed = time.Now()
				quitHint = time.After(doublePressWindow)
				arm.Set("ctrl+c again to detach", "")
			case tui.ControlModeCycle:
				attachModeCycle(ui, conn, false)
			case tui.ControlModeCycleBack:
				attachModeCycle(ui, conn, true)
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
				// recall returns a queued prompt to the session's own editor; there
				// is no wire shape for reaching into this terminal's twin
			}
		}
	}
}

// attachInterrupt runs the Esc body remotely, surfacing a refusal.
func attachInterrupt(ui *tui.UI, conn *client.Conn) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), client.CallWait)
	defer cancel()
	stopped, err := conn.Interrupt(ctx)
	if err != nil {
		ui.Notify("interrupt failed: "+err.Error(), tui.LevelWarn)
	}
	return stopped, err
}

// attachModeCycle steps the remote permission mode, surfacing a refusal.
func attachModeCycle(ui *tui.UI, conn *client.Conn, back bool) {
	ctx, cancel := context.WithTimeout(context.Background(), client.CallWait)
	defer cancel()
	if err := conn.ModeCycle(ctx, back); err != nil {
		ui.Notify("mode change failed: "+err.Error(), tui.LevelWarn)
	}
}

// rewindPicker opens the context-tree picker over the wire transcript and
// rewinds the session onto the chosen entry. Only the current branch is known
// here, so abandoned forks appear once a remote rewind makes one current.
func (v *attachView) rewindPicker() {
	ctx, cancel := context.WithTimeout(context.Background(), client.CallWait)
	tr, _, err := v.conn.Transcript(ctx, "")
	cancel()
	if err != nil {
		v.ui.Notify("rewind: "+err.Error(), tui.LevelWarn)
		return
	}
	head := v.currentHead()
	if head == "" || !slices.ContainsFunc(tr.Entries, func(e ctl.Entry) bool { return e.ID == head }) {
		head = tr.Head
	}

	branch := wireEntries(tr.Entries)
	tree := session.TreeRows(branch, head)
	if len(tree) == 0 {
		v.ui.Notify("nothing to rewind onto yet", tui.LevelInfo)
		return
	}

	items := make([]tui.PickItem, len(tree))
	for i, row := range tree {
		tag, mark := roleTag(row.Kind)
		items[i] = tui.PickItem{Label: row.Guide + rewindBody(row), Tag: tag,
			Mark: mark, Off: !row.Active}
	}
	if len(tree) >= rewindDeferThreshold {
		v.ui.SetDeferHistory(true)
		defer v.ui.SetDeferHistory(false)
	}
	picked, err := v.ui.PickContext(context.Background(), "Rewind to", items,
		tui.PickOptions{Placeholder: "filter", Initial: initialRow(tree, head)})
	if err != nil {
		return // cancelled
	}
	if rerr := v.rewindToRow(branch, tree[picked].ID); rerr != nil {
		v.ui.Notify("rewind: "+rerr.Error(), tui.LevelWarn)
	}
}

// rewindToRow names rowID as the new head over the wire and pre-fills this
// editor with its prompt, matching what a local double-Esc lands on: edit or
// resend it as the restored branch's first message.
func (v *attachView) rewindToRow(branch []session.Entry, rowID string) error {
	rctx, rcancel := context.WithTimeout(context.Background(), client.CallWait)
	newHead, err := v.conn.Rewind(rctx, rowID)
	rcancel()
	if err != nil {
		return err
	}
	if _, fill, ok := session.RewindTarget(branch, rowID); ok && fill != "" {
		v.ui.SetInput(fill)
	}
	v.setHead(newHead) // the head event redraws every view including this one
	return nil
}

// attachRemoteEnded announces the session's exit and reports success: the
// remote run ended, which is not a failure of this viewer.
func attachRemoteEnded(ui *tui.UI, reason string) int {
	ui.Notify("the attached session ended", tui.LevelInfo)
	if reason != "" {
		ui.Notify(reason, tui.LevelWarn)
	}
	return ExitOK
}

// attachConnectionLost reports a socket that died without a session.exit and
// cancels any dialog racer still open.
func attachConnectionLost(ui *tui.UI, view *attachView, exited chan struct{}) int {
	select {
	case <-exited: // the exit raced the close; treat it as an ending
		return attachRemoteEnded(ui, "")
	default:
	}
	view.closeDialogs()
	ui.Notify("control connection lost; the session may still be running", tui.LevelWarn)
	return ExitTurn
}

// listLiveSessions prints the sessions another --attach target could name,
// turning a wrong path into a pickable answer.
func listLiveSessions(live []client.Session) {
	if len(live) == 0 {
		return
	}
	fmt.Fprintln(os.Stderr, "live sessions:")
	for _, s := range live {
		fmt.Fprintf(os.Stderr, "  %s\n", s.String())
	}
}
