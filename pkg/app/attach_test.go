package app

import (
	"context"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/client"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tui"
)

// attachRig pairs a control rig with one attached client view: the socket is
// live, events flow through the view's handler, and the terminal output is
// captured for assertions.
type attachRig struct {
	rig    *controlRig
	conn   *client.Conn
	view   *attachView
	out    func() string
	events chan ctl.Event

	mu      sync.Mutex
	handled []ctl.Event
}

// newAttachRig attaches a fresh plain-mode terminal to rig and completes its
// backfill, so tests assert against rendered history plus live events.
func newAttachRig(t *testing.T, rig *controlRig) *attachRig {
	t.Helper()

	term, out := pipeUI(t)
	return newAttachRigTerm(t, rig, term, out)
}

// newAttachRigTerm is newAttachRig over one chosen terminal, so picker tests
// can drive the attach surface's own input.
func newAttachRigTerm(t *testing.T, rig *controlRig, term *tui.UI, out func() string) *attachRig {
	t.Helper()

	conn, hello, err := client.Dial(rig.path)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	assert.Equal(t, ctl.Protocol, hello.Protocol)

	root, stopRoot := context.WithCancel(t.Context())
	t.Cleanup(stopRoot)
	view := newAttachView(term, conn, root, hello.TranscriptPath)
	view.setHello(hello) // RunAttach records the greeting too; recall keys off it
	view.swapConn(conn)
	a := &attachRig{rig: rig, conn: conn, view: view, out: out,
		events: make(chan ctl.Event, 512)}
	go conn.Serve(func(ev ctl.Event) {
		a.mu.Lock()
		a.handled = append(a.handled, ev)
		a.mu.Unlock()
		select {
		case a.events <- ev:
		default: // a slow reader never blocks delivery
		}
		view.handle(ev)
	})

	require.NoError(t, view.backfill(t.Context()))
	return a
}

// rigBranch reads the recorded transcript's current branch, oldest first: the
// same file path hello names, which is what the view itself reads.
func rigBranch(t *testing.T, rec *sessRec) []session.Entry {
	t.Helper()
	entries, _, err := session.Read(rec.w.Path())
	require.NoError(t, err)
	return session.Branch(entries, rec.w.Head())
}

// TestAttachRewindPickerClosedByRemoteRewind pins the cancel hook the shared
// picker arms: a branch.changed from the other surface closes this terminal's
// open picker, so it never answers with a tree the head has left and no rewind
// rides the stale choice.
func TestAttachRewindPickerClosedByRemoteRewind(t *testing.T) {
	rig := newControlRig(t, func(rec *sessRec) {
		writeMessage(t, rec, "first prompt")
		writeMessage(t, rec, "second prompt")
	})
	wireAttachRewind(t, rig)

	term, _, _ := pipeInputUI(t)
	a := newAttachRigTerm(t, rig, term, func() string { return "" })

	_, err := a.conn.ClaimSeat(t.Context(), "test", false)
	require.NoError(t, err)

	before := rig.rec.w.Head()
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.view.rewindPicker()
	}()
	require.Eventually(t, func() bool {
		a.view.mu.Lock()
		defer a.view.mu.Unlock()
		return a.view.pickStop != nil
	}, ctlWait, 5*time.Millisecond, "picker never armed its cancel hook")

	// a rewind lands from the other surface: the head moves under the open picker
	a.view.handle(ctl.NewEvent(ctl.EventBranchChanged, 20,
		ctl.BranchChangedData{From: before, Head: before, Cause: rewindCauseRewind}))
	select {
	case <-done:
	case <-time.After(ctlWait):
		t.Fatal("remote rewind did not close the open picker")
	}

	a.view.mu.Lock()
	stopped := a.view.pickStop
	a.view.mu.Unlock()
	assert.Nil(t, stopped)                    // disarmed once the picker answered
	assert.Equal(t, before, rig.rec.w.Head()) // nothing rode the stale choice
}

// TestAttachRewindPickerForksRemote pins the double-Esc flow over the file:
// the picker reads this tree from the recorded transcript and a picked prompt
// rewinds the remote session onto its parent, forked away like a local rewind.
func TestAttachRewindPickerForksRemote(t *testing.T) {
	rig := newControlRig(t, func(rec *sessRec) {
		writeMessage(t, rec, "first prompt")
		writeMessage(t, rec, "second prompt")
	})
	wireAttachRewind(t, rig)

	term, feed, out := pipeInputUI(t)
	a := newAttachRigTerm(t, rig, term, out)

	_, err := a.conn.ClaimSeat(t.Context(), "test", false)
	require.NoError(t, err)

	branch := rigBranch(t, rig.rec)
	rowID := branch[len(branch)-1].ID // the newest recorded prompt is pickable last
	wantHead, fill, ok := session.RewindTarget(branch, rowID)
	require.True(t, ok)
	assert.Equal(t, "second prompt", fill)

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.view.rewindPicker()
	}()
	feed("second\n") // the pick overlay reads one line as its filter in plain mode
	select {
	case <-done:
	case <-time.After(ctlWait):
		t.Fatal("rewind picker did not complete")
	}
	assert.Equal(t, wantHead, rig.rec.w.Head()) // the remote session forked onto it
}

// TestAttachRewindPickerShowsAbandonedFork pins whole-tree parity with a local
// double-Esc: a fork recorded and then abandoned stays in the transcript file,
// so the attach picker lists it shaded and rewinding onto it moves the remote
// session off its current chain, pre-filling this editor like a local rewind.
func TestAttachRewindPickerShowsAbandonedFork(t *testing.T) {
	rig := newControlRig(t, func(rec *sessRec) {
		writeMessage(t, rec, "first prompt")
		writeMessage(t, rec, "second prompt")
		entries, _, err := session.Read(rec.w.Path())
		require.NoError(t, err)
		rec.w.SetHead(entries[0].ID) // fork at the root: what a rewind then re-prompt leaves
		writeMessage(t, rec, "fork prompt")
		main, _, err := session.Read(rec.w.Path())
		require.NoError(t, err)
		rec.w.SetHead(main[len(main)-2].ID) // back onto the first chain: the fork stays abandoned
	})
	wireAttachRewind(t, rig)

	term, feed, out := ptyUI(t)
	a := newAttachRigTerm(t, rig, term, out)

	_, err := a.conn.ClaimSeat(t.Context(), "test", false)
	require.NoError(t, err)

	entries, _, err := session.Read(rig.rec.w.Path())
	require.NoError(t, err)
	i := slices.IndexFunc(entries, func(e session.Entry) bool {
		return session.EntryMessageText(e) == "fork prompt"
	})
	require.NotEqual(t, -1, i, "fixture fork entry not found")
	wantHead, _, ok := session.RewindTarget(entries, entries[i].ID)
	require.True(t, ok)

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.view.rewindPicker()
	}()
	// inline mode keys route live, so feed only once the overlay can take them
	require.Eventually(t, func() bool { return strings.Contains(out(), "Rewind to") },
		ctlWait, 5*time.Millisecond)
	feed("fork\r") // the abandoned fork is only pickable if the whole tree was read
	select {
	case <-done:
	case <-time.After(ctlWait):
		t.Fatal("rewind picker did not complete")
	}
	assert.Equal(t, wantHead, rig.rec.w.Head()) // the remote session forked onto it

	// the pre-fill names the picked fork's text, not a sibling row's: rewinding
	// onto either fork-side prompt lands on the same head, so only what the
	// editor holds tells the right row was chosen
	feed("\r")
	select {
	case line := <-term.Messages():
		assert.Equal(t, "fork prompt", line)
	case <-time.After(ctlWait):
		t.Fatal("rewind did not pre-fill the editor")
	}
}

// TestAttachRewindObserverRefused pins the refusal path: a rewind from a
// connection without the control seat moves nothing.
func TestAttachRewindObserverRefused(t *testing.T) {
	rig := newControlRig(t, func(rec *sessRec) {
		writeMessage(t, rec, "first prompt")
	})
	wireAttachRewind(t, rig)

	a := newAttachRig(t, rig) // no seat claimed: an observer

	branch := rigBranch(t, rig.rec)
	rowID := branch[len(branch)-1].ID

	require.Error(t, a.view.rewindToRow(a.conn, branch, rowID))
	assert.Equal(t, rig.rec.w.Head(), branch[len(branch)-1].ID) // nothing moved
}

// TestAttachRewindFillsEditor pins the double-Esc tail: rewinding pre-fills
// this editor with the picked prompt so it can be edited or re-sent as the
// restored branch's first message, exactly like a local rewind.
func TestAttachRewindFillsEditor(t *testing.T) {
	rig := newControlRig(t, func(rec *sessRec) {
		writeMessage(t, rec, "first prompt")
		writeMessage(t, rec, "second prompt")
	})
	wireAttachRewind(t, rig)

	term, feed, out := ptyUI(t)
	a := newAttachRigTerm(t, rig, term, out)

	_, err := a.conn.ClaimSeat(t.Context(), "test", false)
	require.NoError(t, err)

	branch := rigBranch(t, rig.rec)

	// the fill paints the editor into the live block...
	require.NoError(t, a.view.rewindToRow(a.conn, branch, branch[len(branch)-1].ID))
	require.Eventually(t, func() bool { return strings.Contains(out(), "second prompt") },
		ctlWait, 5*time.Millisecond)
	// ...and submitting proves what the buffer held
	feed("\r")
	select {
	case line := <-term.Messages():
		assert.Equal(t, "second prompt", line)
	case <-time.After(ctlWait):
		t.Fatal("rewind did not pre-fill the editor")
	}
}

// TestAttachQueueRecallFillsEditor pins Alt+Up over the wire: the newest
// queued prompt pops from the session's queue into THIS terminal's editor, so
// editing and resubmitting here replaces it; an observer refuses legibly.
func TestAttachQueueRecallFillsEditor(t *testing.T) {
	rig := newControlRig(t, nil)
	term, feed, out := ptyUI(t)
	a := newAttachRigTerm(t, rig, term, out)

	// park two prompts as rows, the way a gate hold leaves them
	rig.hub.q = newSteerQueue(rig.hub.ui, func(int) {}, func() {},
		func(context.Context) {}, func() []agent.MessageInfo { return nil }, rig.hub.queueDroppedCause)
	rig.hub.q.hold(agent.Input{Text: "queued one"}, "queued one", 0)
	rig.hub.q.hold(agent.Input{Text: "queued two"}, "queued two", 0)

	// an observer's Alt+Up refuses, the queue untouched
	attachRecallQueued(term, a.view)
	require.Equal(t, 2, rig.hub.q.pending())

	_, err := a.conn.ClaimSeat(t.Context(), "test", false)
	require.NoError(t, err)

	attachRecallQueued(term, a.view) // LIFO: the newest pops into this editor
	require.Equal(t, 1, rig.hub.q.pending())
	require.Eventually(t, func() bool { return strings.Contains(out(), "queued two") },
		ctlWait, 5*time.Millisecond)

	// submitting proves what the buffer held: the edit resubmits from here
	feed("\r")
	select {
	case line := <-term.Messages():
		assert.Equal(t, "queued two", line)
	case <-time.After(ctlWait):
		t.Fatal("recall did not fill the editor")
	}
}

// TestAttachSubmitRefusedWithoutRouter pins the legible refusal: submitting to
// a run still wiring reports unavailable, never a silent drop.
func TestAttachSubmitRefusedWithoutRouter(t *testing.T) {
	rig := newControlRig(t, nil)
	a := newAttachRig(t, rig)

	_, err := a.conn.ClaimSeat(t.Context(), "test", false)
	require.NoError(t, err)
	err = a.conn.Submit(t.Context(), ctl.SubmitParams{Line: "hello"})
	require.Error(t, err)
	var wire *ctl.Error
	require.ErrorAs(t, err, &wire)
	assert.Equal(t, ctl.CodeUnavailable, wire.Code)
}

// TestAttachReconnectRefusesSubmits pins the offline rule: a line typed while
// the state machine is between instances fails legibly inline with its text
// kept in the editor, never held hostage by the retry.
func TestAttachReconnectRefusesSubmits(t *testing.T) {
	rig := newControlRig(t, nil)
	term, feed, out := ptyUI(t)
	a := newAttachRigTerm(t, rig, term, out)

	rig.srv.Close("test") // the registry went with it: nothing to follow
	root, stopRoot := context.WithCancel(t.Context())
	t.Cleanup(stopRoot)
	quit := make(chan struct{})

	var ready, returned atomic.Bool
	go func() {
		r, _ := a.view.reconnectLoop(term, a.conn.Hello(), root, quit)
		ready.Store(r)
		returned.Store(true)
	}()

	feed("held text\r") // the loop owes this line a legible inline refusal
	require.Eventually(t, func() bool {
		return strings.Contains(out(), "connection lost") &&
			strings.Contains(out(), "not connected") &&
			strings.Count(out(), "held text") >= 2 // typed once, restored to the editor
	}, ctlWait, 5*time.Millisecond)

	stopRoot()
	require.Eventually(t, returned.Load, ctlWait, 5*time.Millisecond)
	assert.False(t, ready.Load()) // the loop exited without a reconnect
}

// TestReconnectGivesUpAfterWindow pins the loss exit path: an offline stretch
// past the give-up window reports back, so RunAttach can exit with the distinct
// lost-session code instead of retrying forever.
func TestReconnectGivesUpAfterWindow(t *testing.T) {
	rig := newControlRig(t, nil)
	term, _, out := ptyUI(t)
	a := newAttachRigTerm(t, rig, term, out)

	rig.srv.Close("test") // the registry went with it: nothing to follow
	old := reconnectGiveUp
	reconnectGiveUp = 25 * time.Millisecond
	t.Cleanup(func() { reconnectGiveUp = old })
	root, stopRoot := context.WithCancel(t.Context())
	t.Cleanup(stopRoot)
	quit := make(chan struct{})

	var gaveUp atomic.Bool
	go func() {
		_, gave := a.view.reconnectLoop(term, a.conn.Hello(), root, quit)
		gaveUp.Store(gave)
	}()
	require.Eventually(t, gaveUp.Load, ctlWait, 5*time.Millisecond)
	assert.True(t, gaveUp.Load())
}

// captureStderr runs fn with os.Stderr swapped for a pipe, returning what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	old := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = old
	require.NoError(t, w.Close())

	b, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(b)
}

// TestExplainNoControlSuggestsResume pins the none-live resolution: a target
// that names a recorded session but no live instance points at resuming that
// session without a socket, while an unknown target stays generic.
func TestExplainNoControlSuggestsResume(t *testing.T) {
	rig := newControlRig(t, nil)
	rig.srv.Close("test") // the registry went with it; the transcript stays

	id := rig.hub.Snapshot().SessionID
	t.Run("full_id", func(t *testing.T) {
		out := captureStderr(t, func() { explainNoControl(id, nil) })
		assert.Contains(t, out, "ajent --resume "+id)
	})

	t.Run("id_prefix", func(t *testing.T) {
		out := captureStderr(t, func() { explainNoControl(id[:12], nil) })
		assert.Contains(t, out, "ajent --resume "+id)
	})

	t.Run("unknown_target_stays_generic", func(t *testing.T) {
		out := captureStderr(t, func() { explainNoControl("01UNKNOWN0000", nil) })
		assert.NotContains(t, out, "ajent --resume 01UNKNOWN0000")
		assert.Contains(t, out, "control.enabled")
	})

	// bare --attach with nothing live: the same guidance, no target to name
	t.Run("bare_nothing_live", func(t *testing.T) {
		out := captureStderr(t, func() { explainNoControl("", nil) })
		assert.Contains(t, out, "control.enabled")
		assert.Contains(t, out, "ajent --resume")
	})
}

// pipeInputUI opens a plain-mode TUI over pipes like pipeUI, also returning
// the input writer so tests can drive dialogs that read one line.
func pipeInputUI(t *testing.T) (*tui.UI, func(string), func() string) {
	t.Helper()

	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
	})
	ui, err := tui.New(tui.Options{In: inR, Out: outW, Mode: tui.ModePlain})
	require.NoError(t, err)
	t.Cleanup(ui.Close)

	var mu sync.Mutex
	var sb strings.Builder
	go func() { _, _ = io.Copy(&lockedWriter{mu: &mu, sb: &sb}, outR) }()
	return ui, func(data string) { _, _ = io.WriteString(inW, data) }, func() string {
		mu.Lock()
		defer mu.Unlock()
		return sb.String()
	}
}

// ptyUI runs one inline-mode terminal over a real pty: the returned writer
// feeds keys and submits land on Messages. Plain mode has no key loop, which
// hides editor fills; TERM must name a real terminal or ResolveMode downgrades
// to plain before anything can be observed.
func ptyUI(t *testing.T) (*tui.UI, func(string), func() string) {
	t.Helper()
	t.Setenv("TERM", "xterm")

	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = master.Close() })

	var unlock int32 // TIOCSPTLCK: zero unlocks the slave
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK,
		uintptr(unsafe.Pointer(&unlock)))
	require.Zero(t, errno)

	var ptyNo int32
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN,
		uintptr(unsafe.Pointer(&ptyNo)))
	require.Zero(t, errno)

	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(int(ptyNo)), os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = slave.Close() })

	ui, err := tui.New(tui.Options{In: slave, Out: slave, Mode: tui.ModeInline})
	require.NoError(t, err)
	t.Cleanup(ui.Close)

	var mu sync.Mutex
	var sb strings.Builder
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := master.Read(buf) // blocks until the terminal paints or closes
			if n > 0 {
				mu.Lock()
				sb.Write(buf[:n])
				mu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}()
	return ui, func(keys string) { _, _ = master.WriteString(keys) }, func() string {
		mu.Lock()
		defer mu.Unlock()
		return sb.String()
	}
}

// wireAttachRewind arms the rewind seam over one rig with a live agent and
// registry, so session.rewind requests rebuild the branch like the driver's.
func wireAttachRewind(t *testing.T, rig *controlRig) {
	t.Helper()

	reg, warnings := llm.NewRegistry(llm.File{
		Providers: map[string]llm.ProviderConfig{"p": {Models: []llm.ModelConfig{{ID: "a"}}}},
	}, nil, llm.RegistryOptions{})
	require.Empty(t, warnings)
	st := &agent.State{Model: reg.Active(), Tokens: tokens.New(reg.Active())}
	ag := agent.New(st, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) { return &llm.ScriptedProvider{}, nil },
		Sinks:    []agent.Sink{rig.rec.rec.Sink(rig.hub.sink())},
		Env:      agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
	})
	rig.hub.ag = ag
	rig.hub.syncLedger()
	rig.hub.rewindTo = func(head, cause string) (string, error) {
		return rig.rec.rewindTo(rig.hub.ui, ag, reg, head, cause)
	}
}
