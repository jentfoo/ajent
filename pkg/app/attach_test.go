package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
	"github.com/jentfoo/ajent/pkg/paths"
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
	return newAttachRigTerm(t, rig, term, out, false)
}

// newAttachRigTerm is newAttachRig over one chosen terminal, so picker tests
// can drive the attach surface's own input. seat claims the control seat
// before backfill, mirroring attachOnce's claim-then-backfill order.
func newAttachRigTerm(t *testing.T, rig *controlRig, term *tui.UI, out func() string, seat bool) *attachRig {
	t.Helper()

	conn, hello, err := client.Dial(rig.path)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	assert.Equal(t, ctl.Protocol, hello.Protocol)

	root, stopRoot := context.WithCancel(t.Context())
	t.Cleanup(stopRoot)
	view := newAttachView(term, conn, root)
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

	if seat {
		_, err := conn.ClaimSeat(t.Context(), "attach-rig", false)
		require.NoError(t, err)
		view.setSeat(true)
	}

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

func TestAttachRewindPicker(t *testing.T) {
	t.Run("closed_by_remote_rewind", func(t *testing.T) {
		rig := newControlRig(t, func(rec *sessRec) {
			writeMessage(t, rec, "first prompt")
			writeMessage(t, rec, "second prompt")
		})
		wireAttachRewind(t, rig)

		term, _, _ := pipeInputUI(t)
		a := newAttachRigTerm(t, rig, term, func() string { return "" }, false)

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
		}, ctlWait, 5*time.Millisecond)

		// a rewind lands from the other surface: the head moves under the open picker
		a.view.handle(ctl.NewEvent(ctl.EventBranchChanged, 20,
			ctl.BranchChangedData{From: before, Head: before, Cause: rewindCauseRewind}))
		<-done

		a.view.mu.Lock()
		stopped := a.view.pickStop
		a.view.mu.Unlock()
		assert.Nil(t, stopped)                    // disarmed once the picker answered
		assert.Equal(t, before, rig.rec.w.Head()) // nothing rode the stale choice
	})

	// the double-Esc flow over the file: the picker reads this tree from the
	// recorded transcript and a picked prompt rewinds the remote session onto its
	// parent, forked away like a local rewind.
	t.Run("forks_remote", func(t *testing.T) {
		rig := newControlRig(t, func(rec *sessRec) {
			writeMessage(t, rec, "first prompt")
			writeMessage(t, rec, "second prompt")
		})
		wireAttachRewind(t, rig)

		term, feed, out := pipeInputUI(t)
		a := newAttachRigTerm(t, rig, term, out, false)

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
		<-done
		assert.Equal(t, wantHead, rig.rec.w.Head()) // the remote session forked onto it
	})

	// whole-tree parity with a local double-Esc: a fork recorded and then
	// abandoned stays in the transcript file, so the attach picker lists it shaded
	// and rewinding onto it pre-fills this editor like a local rewind.
	t.Run("shows_abandoned_fork", func(t *testing.T) {
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
		a := newAttachRigTerm(t, rig, term, out, false)

		_, err := a.conn.ClaimSeat(t.Context(), "test", false)
		require.NoError(t, err)

		entries, _, err := session.Read(rig.rec.w.Path())
		require.NoError(t, err)
		i := slices.IndexFunc(entries, func(e session.Entry) bool {
			return session.EntryMessageText(e) == "fork prompt"
		})
		require.NotEqual(t, -1, i)
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
		<-done
		assert.Equal(t, wantHead, rig.rec.w.Head()) // the remote session forked onto it

		// the pre-fill names the picked fork's text, not a sibling row's: rewinding
		// onto either fork-side prompt lands on the same head, so only what the
		// editor holds tells the right row was chosen
		feed("\r")
		assert.Equal(t, "fork prompt", <-term.Messages())
	})
}

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

func TestAttachRewindFillsEditor(t *testing.T) {
	rig := newControlRig(t, func(rec *sessRec) {
		writeMessage(t, rec, "first prompt")
		writeMessage(t, rec, "second prompt")
	})
	wireAttachRewind(t, rig)

	term, feed, out := ptyUI(t)
	a := newAttachRigTerm(t, rig, term, out, false)

	_, err := a.conn.ClaimSeat(t.Context(), "test", false)
	require.NoError(t, err)

	branch := rigBranch(t, rig.rec)

	// the fill paints the editor into the live block...
	require.NoError(t, a.view.rewindToRow(a.conn, branch, branch[len(branch)-1].ID))
	require.Eventually(t, func() bool { return strings.Contains(out(), "second prompt") },
		ctlWait, 5*time.Millisecond)
	// ...and submitting proves what the buffer held
	feed("\r")
	assert.Equal(t, "second prompt", <-term.Messages())
}

func TestAttachQueueRecallFillsEditor(t *testing.T) {
	rig := newControlRig(t, nil)
	term, feed, out := ptyUI(t)
	a := newAttachRigTerm(t, rig, term, out, false)

	// park two prompts as rows, the way a gate hold leaves them
	rig.hub.q = newSteerQueue(rig.hub.ui, func(int) {},
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
	assert.Equal(t, "queued two", <-term.Messages())
}

func TestAttachReconnect(t *testing.T) {
	t.Run("refuses_submits", func(t *testing.T) {
		rig := newControlRig(t, nil)
		term, feed, out := ptyUI(t)
		a := newAttachRigTerm(t, rig, term, out, false)

		rig.srv.Close("test") // the registry went with it: nothing to follow
		root, stopRoot := context.WithCancel(t.Context())
		t.Cleanup(stopRoot)
		quit := make(chan struct{})

		var ready, returned atomic.Bool
		go func() {
			r, _ := a.view.reconnectLoop(term, a.conn.Hello(), root, &reconnectPace{}, quit)
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
	})

	t.Run("gives_up_after_window", func(t *testing.T) {
		rig := newControlRig(t, nil)
		term, _, out := ptyUI(t)
		a := newAttachRigTerm(t, rig, term, out, false)

		rig.srv.Close("test") // the registry went with it: nothing to follow
		old := reconnectGiveUp
		reconnectGiveUp = 25 * time.Millisecond
		t.Cleanup(func() { reconnectGiveUp = old })
		root, stopRoot := context.WithCancel(t.Context())
		t.Cleanup(stopRoot)
		quit := make(chan struct{})

		var gaveUp atomic.Bool
		go func() {
			_, gave := a.view.reconnectLoop(term, a.conn.Hello(), root, &reconnectPace{}, quit)
			gaveUp.Store(gave)
		}()
		require.Eventually(t, gaveUp.Load, ctlWait, 5*time.Millisecond)
		assert.True(t, gaveUp.Load())
	})
}

// writeInstanceRecord drops one hand-built discovery record into the session's
// control directory, the shape a second serving instance would have written.
// A peer rig's async sweep can remove the still-empty directory between mkdir
// and write, so a miss recreates and retries.
func writeInstanceRecord(t *testing.T, sessionID string, in client.Instance) {
	t.Helper()
	home, err := paths.Home()
	require.NoError(t, err)
	dir := filepath.Join(home, paths.ControlDirName, sessionID)
	reg := filepath.Join(dir, tokenSuffix(in.Token)+".json")
	b, err := json.Marshal(in)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, paths.DirPerm))
	var werr error
	for range 2 {
		if werr = os.WriteFile(reg, b, 0o600); werr == nil {
			return
		}
		require.NoError(t, os.MkdirAll(dir, paths.DirPerm))
	}
	require.NoError(t, werr)
}

// countAccepts listens at path, counting connections until the listener closes.
// The peer gets nothing: a dial lands and fails its hello read, which still
// proves the dial happened.
func countAccepts(t *testing.T, path string) *atomic.Int64 {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	var n atomic.Int64
	go func() {
		for {
			nc, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			n.Add(1)
			_ = nc.Close()
		}
	}()
	return &n
}

// reconnectOutcome is one reconnect loop's result pair, for channel delivery.
type reconnectOutcome struct{ ready, gaveUp bool }

// launchReconnect runs one reconnect loop over a bare view in its goroutine,
// reporting the outcome once. Cleanup stops it at its next wait.
func launchReconnect(t *testing.T, hello ctl.Hello) chan reconnectOutcome {
	t.Helper()
	term, _ := pipeUI(t)
	root, stopRoot := context.WithCancel(t.Context())
	view := newAttachView(term, nil, root)
	quit := make(chan struct{})
	res := make(chan reconnectOutcome, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ready, gaveUp := view.reconnectLoop(term, hello, root, &reconnectPace{}, quit)
		res <- reconnectOutcome{ready, gaveUp}
	}()
	// the loop exits here, before later cleanups restore the shared test seams
	t.Cleanup(func() { stopRoot(); close(quit); <-done })
	return res
}

// awaitReconnect reads the loop's outcome once it landed.
func awaitReconnect(t *testing.T, res chan reconnectOutcome) reconnectOutcome {
	t.Helper()
	var out reconnectOutcome
	require.Eventually(t, func() bool {
		select {
		case out = <-res:
			return true
		default:
			return false
		}
	}, ctlWait, 5*time.Millisecond)
	return out
}

// fakePicker swaps the picker seam for fn, restoring it at cleanup.
func fakePicker(t *testing.T, fn func(context.Context, *tui.UI, []client.Instance, bool) ([]client.Instance, error)) {
	t.Helper()
	real := pickInstancesFn
	pickInstancesFn = fn
	t.Cleanup(func() { pickInstancesFn = real })
}

// shrinkBackoff shrinks the reconnect backoff for one test, restoring at cleanup.
func shrinkBackoff(t *testing.T, first time.Duration) {
	t.Helper()
	old := reconnectFirst
	reconnectFirst = first
	t.Cleanup(func() { reconnectFirst = old })
}

// liveRecord builds one discovery record this process vouches for, old enough
// that record-only liveness judges it alive rather than fresh.
func liveRecord(sessionID, token, socket string) client.Instance {
	return client.Instance{SessionID: sessionID, Token: token, Socket: socket,
		PID: os.Getpid(), StartedAt: time.Now().Add(-2 * time.Minute).UnixMilli()}
}

// deadRecord is one whose pid this test reaped, old enough to judge dead.
func deadRecord(t *testing.T, sessionID, token, socket string) client.Instance {
	t.Helper()
	gone := exec.CommandContext(t.Context(), "true")
	require.NoError(t, gone.Run())
	return client.Instance{SessionID: sessionID, Token: token, Socket: socket,
		PID: gone.ProcessState.Pid(), StartedAt: time.Now().Add(-2 * time.Minute).UnixMilli()}
}

func TestReconnectLoopPickerPaths(t *testing.T) {
	// several live records of the session and a view whose instance is gone:
	// Followable answers nil, so the picker decides among them. Picker and
	// backoff seams arm before the launch, so the loop goroutine never races
	// the swap.
	setup := func(t *testing.T, pick func(context.Context, *tui.UI, []client.Instance, bool) ([]client.Instance, error)) chan reconnectOutcome {
		t.Helper()
		shrinkBackoff(t, 60*time.Millisecond)
		fakePicker(t, pick)
		rig := newControlRig(t, nil)
		sid := rig.hub.Snapshot().SessionID
		rig.srv.Close("test") // the registry went with it
		writeInstanceRecord(t, sid, liveRecord(sid, "tokAAAAAAAAAAAAAAAA", "/nonexistent/a"))
		writeInstanceRecord(t, sid, liveRecord(sid, "tokBBBBBBBBBBBBBBBB", "/nonexistent/b"))
		return launchReconnect(t, ctl.Hello{Session: ctl.Session{ID: sid}, Token: "origtoken"})
	}

	// a non-cancel picker failure retries through the backoff wait, never a spin
	t.Run("picker_error_honors_backoff", func(t *testing.T) {
		var mu sync.Mutex
		var calls []time.Time
		setup(t, func(context.Context, *tui.UI, []client.Instance, bool) ([]client.Instance, error) {
			mu.Lock()
			calls = append(calls, time.Now())
			mu.Unlock()
			return nil, errors.New("picker broke")
		})

		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(calls) >= 2
		}, ctlWait, 5*time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		assert.GreaterOrEqual(t, calls[1].Sub(calls[0]), 60*time.Millisecond)
	})

	t.Run("picker_cancel_detaches", func(t *testing.T) {
		res := setup(t, func(context.Context, *tui.UI, []client.Instance, bool) ([]client.Instance, error) {
			return nil, tui.ErrCancelled
		})

		out := awaitReconnect(t, res)
		assert.False(t, out.ready)
		assert.False(t, out.gaveUp)
	})

	// the choice adopts the picked token: later rounds dial it without
	// consulting the picker again
	t.Run("picker_choice_dials_picked", func(t *testing.T) {
		rig := newControlRig(t, nil)
		sid := rig.hub.Snapshot().SessionID
		rig.srv.Close("test")
		home, err := paths.Home()
		require.NoError(t, err)
		base := filepath.Join(home, paths.ControlDirName, sid)
		// records first: they pin the session dir against another rig's sweep
		sockA := filepath.Join(base, "aaaaaaaa.sock")
		sockB := filepath.Join(base, "bbbbbbbb.sock")
		writeInstanceRecord(t, sid, liveRecord(sid, "tokAAAAAAAAAAAAAAAA", sockA))
		writeInstanceRecord(t, sid, liveRecord(sid, "tokBBBBBBBBBBBBBBBB", sockB))
		accepts := []*atomic.Int64{countAccepts(t, sockA), countAccepts(t, sockB)}
		shrinkBackoff(t, 40*time.Millisecond)

		var mu sync.Mutex
		var calls int
		fakePicker(t, func(_ context.Context, _ *tui.UI, list []client.Instance, _ bool) ([]client.Instance, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return list[:1], nil
		})

		launchReconnect(t, ctl.Hello{Session: ctl.Session{ID: sid}, Token: "origtoken"})
		// two dials past the pick prove later rounds follow the picked token alone
		require.Eventually(t, func() bool {
			return accepts[0].Load()+accepts[1].Load() >= 2
		}, ctlWait, 5*time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, 1, calls)
	})
}

func TestReconnectLoopSkipsDeadRecords(t *testing.T) {
	rig := newControlRig(t, nil)
	sid := rig.hub.Snapshot().SessionID
	rig.srv.Close("test") // the registry went with it

	// the dead record's socket answers dials: only the record-only liveness
	// filter keeps the loop off it
	home, err := paths.Home()
	require.NoError(t, err)
	base := filepath.Join(home, paths.ControlDirName, sid)
	sock := filepath.Join(base, "deaddead.sock")
	writeInstanceRecord(t, sid, deadRecord(t, sid, "tokDEADDEADDEADDEAD", sock))
	accepts := countAccepts(t, sock)
	shrinkBackoff(t, 25*time.Millisecond)
	old := reconnectGiveUp
	reconnectGiveUp = 200 * time.Millisecond
	t.Cleanup(func() { reconnectGiveUp = old })

	out := awaitReconnect(t, launchReconnect(t, ctl.Hello{Session: ctl.Session{ID: sid}, Token: "origtoken"}))
	assert.True(t, out.gaveUp) // rounds elapsed; nothing live was ever dialed
	assert.Zero(t, accepts.Load())
}

func TestReconnectLoopPacingSpansCycles(t *testing.T) {
	rig := newControlRig(t, nil)
	sid := rig.hub.Snapshot().SessionID
	rig.srv.Close("test") // the registry went with it

	// a listener that accepts and closes immediately: every dial lands on a
	// host-shaped socket and fails its hello fast, like a crash-looping serve
	home, err := paths.Home()
	require.NoError(t, err)
	sock := filepath.Join(home, paths.ControlDirName, sid, "flapflap.sock")
	writeInstanceRecord(t, sid, liveRecord(sid, "tokFLAPFLAPFLAPFLAP", sock))
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	var mu sync.Mutex
	var dials []time.Time
	go func() {
		for {
			nc, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			mu.Lock()
			dials = append(dials, time.Now())
			mu.Unlock()
			_ = nc.Close()
		}
	}()

	shrinkBackoff(t, 60*time.Millisecond)
	term, _ := pipeUI(t)
	root1, stopRoot1 := context.WithCancel(t.Context())
	t.Cleanup(stopRoot1)
	view := newAttachView(term, nil, root1)
	hello := ctl.Hello{Session: ctl.Session{ID: sid}, Token: "origtoken"}
	st := &reconnectPace{}
	quit := make(chan struct{})
	t.Cleanup(func() { close(quit) })

	first := make(chan reconnectOutcome, 1)
	go func() {
		r, g := view.reconnectLoop(term, hello, root1, st, quit)
		first <- reconnectOutcome{r, g}
	}()
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(dials) >= 2
	}, ctlWait, 5*time.Millisecond)
	stopRoot1()
	out := awaitReconnect(t, first)
	assert.False(t, out.ready)

	grown := st.backoff // the channel receive orders it against the loop's writes
	require.Greater(t, grown, reconnectFirst)

	// a second cycle over the same state: its first dial waits out the carried
	// backoff instead of starting over at full speed
	root2, stopRoot2 := context.WithCancel(t.Context())
	t.Cleanup(stopRoot2)
	mu.Lock()
	base := len(dials)
	mu.Unlock()
	before := time.Now()
	second := make(chan reconnectOutcome, 1)
	go func() {
		r, g := view.reconnectLoop(term, hello, root2, st, quit)
		second <- reconnectOutcome{r, g}
	}()
	var waited time.Duration
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		if len(dials) <= base {
			return false
		}
		waited = dials[base].Sub(before)
		return true
	}, ctlWait, 5*time.Millisecond)
	assert.GreaterOrEqual(t, waited, grown/2)
	stopRoot2() // the loop exits before cleanups restore the shared seams
	awaitReconnect(t, second)
}

// attachOnceRig pairs a control rig holding its seat with one freshly dialed
// attach connection, the way RunAttach wires attachOnce: no reader started,
// hello recorded on the view.
type attachOnceRig struct {
	rig     *controlRig
	conn    *client.Conn
	view    *attachView
	hello   ctl.Hello
	out     func() string
	quit    chan struct{}
	outcome chan attachOutcome
}

func newAttachOnceRig(t *testing.T, rig *controlRig) *attachOnceRig {
	t.Helper()

	term, out := pipeUI(t)
	conn, hello, err := client.Dial(rig.path)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	root, stopRoot := context.WithCancel(t.Context())
	t.Cleanup(stopRoot)
	view := newAttachView(term, conn, root)
	view.setHello(hello) // RunAttach records the greeting too; backfill reads it
	return &attachOnceRig{rig: rig, conn: conn, view: view, hello: hello,
		out: out, quit: make(chan struct{}), outcome: make(chan attachOutcome, 1)}
}

// run launches attachOnce in its goroutine; close quit to detach it.
func (a *attachOnceRig) run(force bool) {
	go func() {
		o, _ := attachOnce(a.view, a.conn, a.hello, "forced attach (pid 99)", force,
			make(chan string, submitBuffer), a.quit)
		a.outcome <- o
	}()
}

// awaitOutcome drains the outcome channel once quit has closed the loop.
func (a *attachOnceRig) awaitOutcome(t *testing.T) attachOutcome {
	t.Helper()
	var o attachOutcome
	require.Eventually(t, func() bool {
		select {
		case o = <-a.outcome:
			return true
		default:
			return false
		}
	}, ctlWait, 5*time.Millisecond)
	return o
}

// reconnect drops the wire and dials again on the same view, the shape
// RunAttach's loop drives between attach attempts after a dropped socket.
func (a *attachOnceRig) reconnect(t *testing.T) {
	t.Helper()
	a.conn.Close() // the old reader ends with it
	conn, hello, err := client.Dial(a.rig.path)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	a.conn = conn
	a.hello = hello
	a.view.swapConn(conn)
}

func TestAttachOnceSeatClaim(t *testing.T) {
	t.Run("force_takes_held_seat", func(t *testing.T) {
		rig := newControlRig(t, nil)
		holder := dialControl(t, rig.path)
		holder.hello()
		attachControl(t, holder, "first")

		a := newAttachOnceRig(t, rig)
		a.run(true)

		// the takeover announces to every connection, naming both sides of it
		var lost ctl.SeatLostData
		require.Eventually(t, func() bool {
			select {
			case in, ok := <-holder.inc:
				if !ok {
					return false
				}
				if in.Event != nil && in.Event.Name == ctl.EventSeatLost {
					require.NoError(t, in.Event.Decode(&lost))
					return true
				}
				return false // hello and attach replies pass through here
			default:
				return false
			}
		}, ctlWait, 5*time.Millisecond)
		assert.Equal(t, "first", lost.From)
		assert.Equal(t, "forced attach (pid 99)", lost.To)

		close(a.quit)
		assert.Equal(t, attachDetached, a.awaitOutcome(t))
		// control landed: the observer notice never rendered
		assert.NotContains(t, a.out(), "watching read-only")
	})

	// without force a held seat downgrades this attach to observer, as before
	t.Run("held_seat_stays_observer", func(t *testing.T) {
		rig := newControlRig(t, nil)
		holder := dialControl(t, rig.path)
		holder.hello()
		attachControl(t, holder, "first")

		a := newAttachOnceRig(t, rig)
		a.run(false)

		require.Eventually(t, func() bool {
			return strings.Contains(a.out(), "watching read-only")
		}, ctlWait, 5*time.Millisecond)

		close(a.quit)
		assert.Equal(t, attachDetached, a.awaitOutcome(t))
	})
}

func TestAttachSeatRegainNotice(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	// every seat transition the terminal can see names itself, a regained seat included

	t.Run("holder_eof_keeps_viewer_silent", func(t *testing.T) {
		rig := newControlRig(t, nil)
		holder := dialControl(t, rig.path)
		holder.hello()
		attachControl(t, holder, "first")

		a := newAttachOnceRig(t, rig)
		a.run(false) // blocked at attach: a viewer
		require.Eventually(t, func() bool {
			return strings.Contains(a.out(), "watching read-only")
		}, ctlWait, 5*time.Millisecond)

		// the holder's EOF frees the seat for anyone; this terminal never
		// re-claims while connected, so nothing arrives and it stays a viewer
		require.NoError(t, holder.conn.Close())
		rig.awaitSeatFree(t)
		assert.NotContains(t, a.out(), "you hold the control seat")

		// its current state still answers a write attempt, now naming nobody
		sctx, scancel := context.WithTimeout(context.Background(), client.CallWait)
		err := a.conn.Submit(sctx, ctl.SubmitParams{Line: "anyone there?"})
		scancel()
		var wire *ctl.Error
		require.ErrorAs(t, err, &wire)
		assert.Equal(t, ctl.CodeObserver, wire.Code)
		assert.Contains(t, wire.Message, "nobody holds the control seat")

		close(a.quit)
		assert.Equal(t, attachDetached, a.awaitOutcome(t))
	})

	// a blocked viewer whose blocker leaves during a reconnect gap gains the
	// seat: the flip must be announced against the read-only notice before it
	t.Run("promoted_after_blocker_leaves", func(t *testing.T) {
		rig := newControlRig(t, nil)
		holder := dialControl(t, rig.path)
		holder.hello()
		attachControl(t, holder, "first")

		a := newAttachOnceRig(t, rig)
		a.run(false)
		require.Eventually(t, func() bool {
			return strings.Contains(a.out(), "watching read-only")
		}, ctlWait, 5*time.Millisecond)

		close(a.quit)
		assert.Equal(t, attachDetached, a.awaitOutcome(t))

		require.NoError(t, holder.conn.Close()) // the seat frees while offline
		rig.awaitSeatFree(t)

		a.reconnect(t)
		a.run(false) // the same plain claim now succeeds
		require.Eventually(t, func() bool {
			return strings.Contains(a.out(), "you hold the control seat")
		}, ctlWait, 5*time.Millisecond)

		var held bool
		a.view.call(func() { held = a.view.holder }) // setSeat ran before any notice
		assert.True(t, held)
	})

	// a force attach displaced mid-session reconnects with the same force and
	// takes the seat back; both directions of the takeover name themselves
	t.Run("force_reclaims_after_displacement", func(t *testing.T) {
		rig := newControlRig(t, nil)
		first := dialControl(t, rig.path)
		first.hello()
		attachControl(t, first, "first")

		a := newAttachOnceRig(t, rig)
		a.run(true) // takes the held seat: no read-only news to correct
		require.Eventually(t, func() bool {
			return strings.Contains(a.out(), "attached to session")
		}, ctlWait, 5*time.Millisecond)
		assert.NotContains(t, a.out(), "you hold the control seat")

		// displaced over the wire: the seat.lost event matches this terminal's
		// holder name and downgrades it with its banner
		second := dialControl(t, rig.path)
		second.hello()
		rp := second.call("force", ctl.MethodAttach,
			map[string]any{"control": true, "holder": "second", "force": true})
		require.Nil(t, rp.Error)
		require.Eventually(t, func() bool {
			return strings.Contains(a.out(), `control seat taken by "second"; watching read-only`)
		}, ctlWait, 5*time.Millisecond)

		close(a.quit)
		assert.Equal(t, attachDetached, a.awaitOutcome(t))

		a.reconnect(t)
		a.run(true) // force rides every claim of the run: it takes the seat back
		require.Eventually(t, func() bool {
			return strings.Contains(a.out(), "you hold the control seat")
		}, ctlWait, 5*time.Millisecond)
	})
}

func TestAttachEndExitCodes(t *testing.T) {
	t.Parallel()

	hello := ctl.Hello{Session: ctl.Session{ID: "01TEST", Name: "named"}}
	tests := []struct {
		name   string
		reason string
		want   int
	}{
		{name: "clean_end_ok", want: ExitOK},
		{name: "reason_is_failed_turn", reason: "turn failed: model error", want: ExitTurn},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ui, out := pipeUI(t)
			assert.Equal(t, tc.want, attachEnd(ui, tc.reason, hello))
			if tc.reason == "" {
				require.Eventually(t, func() bool {
					return strings.Contains(out(), "resume it later with: ajent --resume named")
				}, ctlWait, 5*time.Millisecond)
				return
			}
			// the reason is the notice; the resume hint stays clean-end only
			require.Eventually(t, func() bool {
				s := out()
				return strings.Contains(s, tc.reason) && !strings.Contains(s, "resume it later")
			}, ctlWait, 5*time.Millisecond)
		})
	}
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
	rig.rec.busy = func() bool { return ag.Running() }
	rig.hub.ag = ag
	rig.hub.syncLedger()
	rig.hub.rewindTo = func(head, cause string) (string, error) {
		return rig.rec.rewindTo(rig.hub.ui, ag, reg, head, cause)
	}
}
