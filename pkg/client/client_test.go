package client

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/paths"
	"github.com/jentfoo/ajent/pkg/session"
)

// fakeSession is a minimal control-socket server: hello, then canned replies
// keyed by method, with queued events preceding them like a retained ring.
type fakeSession struct {
	ln     net.Listener
	home   string
	mu     sync.Mutex
	replay []ctl.Event // events staged for the next connection's stream

	transcriptPath string // hello names this file when set; state.get reports head
	head           string
	log            []ctl.Event   // answered for events.replay, filtered by since
	hangReplay     bool          // never answer replay: the close-wakeup test hangs on it
	hung           chan struct{} // closed once a hangReplay request reaches serveConn
}

// newFakeSession binds a socket in a short temp dir and starts accepting. It
// pins nothing: discovery tests point AJENT_HOME at f.home themselves.
func newFakeSession(t *testing.T) *fakeSession {
	t.Helper()
	home, err := os.MkdirTemp("", "client-test")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	dir := filepath.Join(home, paths.ControlDirName)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", filepath.Join(dir, "01JFAKESESSIONID000000.sock"))
	require.NoError(t, err)

	f := &fakeSession{ln: ln, home: home, hung: make(chan struct{})}
	t.Cleanup(func() { _ = ln.Close() })
	go f.acceptLoop()
	return f
}

// dial attaches a client to the fake session, feeding events into a channel.
func (f *fakeSession) dial(t *testing.T) (*Conn, ctl.Hello, <-chan ctl.Event) {
	t.Helper()
	events := make(chan ctl.Event, 64)
	conn, hello, err := Dial(f.Path())
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	go conn.Serve(func(ev ctl.Event) { events <- ev })
	return conn, hello, events
}

// waitEvent pulls one event with a bound.
func waitEvent(t *testing.T, events <-chan ctl.Event) ctl.Event {
	t.Helper()
	select {
	case ev := <-events:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an event")
		return ctl.Event{}
	}
}

// Path reports the socket path this fake listens on.
func (f *fakeSession) Path() string { return f.ln.Addr().String() }

// serve answers one connection until it goes away.
func (f *fakeSession) acceptLoop() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.serveConn(conn)
	}
}

func (f *fakeSession) serveConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	enc := ctl.NewEncoder(conn)

	// the greeting a real session writes before anything can queue behind it
	f.mu.Lock()
	path, head := f.transcriptPath, f.head
	f.mu.Unlock()
	err := enc.Encode(ctl.NewEvent(ctl.EventHello, 0, ctl.Hello{
		Protocol: ctl.Protocol, Session: ctl.Session{ID: "01JFAKESESSIONID000000", Name: "fake"},
		Workspace: "/repo", Cwd: "/repo", Interactive: true, PID: os.Getpid(),
		TranscriptPath: path,
	}))
	if err != nil {
		return
	}

	f.mu.Lock()
	events := append([]ctl.Event(nil), f.replay...)
	f.replay = nil // one replay per connection, like a session's retained ring
	f.mu.Unlock()

	var seq uint64
	for _, ev := range events {
		seq++
		ev.Seq = seq
		if err := enc.Encode(ev); err != nil {
			return
		}
	}

	dec := ctl.NewDecoder(conn)
	for {
		var req ctl.Request
		if err := dec.Decode(&req); err != nil {
			return
		}
		switch req.Method {
		case ctl.MethodState:
			seq++
			_ = enc.Encode(ctl.NewEvent(ctl.EventTurnStart, seq, ctl.TurnStartData{Model: "p/m"}))
			_ = enc.Encode(ctl.Reply{ID: req.ID, Seq: seq,
				Result: mustJSON(ctl.State{Session: ctl.Session{ID: "01JTEST"}, Head: head})})
		case ctl.MethodSubmit:
			var p ctl.SubmitParams
			_ = json.Unmarshal(req.Params, &p)
			seq++
			_ = enc.Encode(ctl.NewEvent(ctl.EventEcho, seq, ctl.EchoData{Text: p.Line}))
			_ = enc.Encode(ctl.Reply{ID: req.ID, Seq: seq})
		case ctl.MethodInterrupt:
			_ = enc.Encode(ctl.Reply{ID: req.ID,
				Result: mustJSON(ctl.InterruptResult{Stopped: "turn"})})
		case ctl.MethodReplay:
			f.mu.Lock()
			hang, log := f.hangReplay, append([]ctl.Event(nil), f.log...)
			f.mu.Unlock()
			if hang {
				close(f.hung) // the request arrived: any waiting caller may rely on it
				return        // never answered: the close-wakeup test hangs on this
			}
			var p ctl.ReplayParams
			_ = json.Unmarshal(req.Params, &p)
			var past []ctl.Event
			for _, ev := range log {
				if ev.Seq > p.Since {
					past = append(past, ev)
				}
			}
			seq++
			_ = enc.Encode(ctl.Reply{ID: req.ID, Seq: seq,
				Result: mustJSON(ctl.ReplayResult{Events: past})})
		default:
			_ = enc.Encode(ctl.Reply{ID: req.ID,
				Error: ctl.Errf(ctl.CodeUnknownMethod, "unknown method %q", req.Method)})
		}
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// TestDialReadsHello pins the hello contract: protocol version and a working
// request round trip behind it.
func TestDialReadsHello(t *testing.T) {
	t.Parallel()
	f := newFakeSession(t)

	conn, hello, events := f.dial(t)
	assert.Equal(t, ctl.Protocol, hello.Protocol)

	stopped, err := conn.Interrupt(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "turn", stopped)

	// the interrupt carried no observable side effect: nothing streams
	select {
	case ev := <-events:
		t.Fatalf("unexpected event %s", ev.Name)
	default:
	}
}

// TestDialRefusesMissingSocket covers connect failures naming the cause.
func TestDialRefusesMissingSocket(t *testing.T) {
	t.Parallel()

	_, _, err := Dial(filepath.Join(t.TempDir(), "missing.sock"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no such socket")
}

// TestCallReportsWireError pins the error shape: a refused request surfaces as
// *ctl.Error with its code, not a transport failure.
func TestCallReportsWireError(t *testing.T) {
	t.Parallel()
	f := newFakeSession(t)
	conn, _, _ := f.dial(t)

	_, err := conn.Call(t.Context(), "no-such-method", nil, nil)
	require.Error(t, err)
	var wire *ctl.Error
	require.ErrorAs(t, err, &wire)
	assert.Equal(t, ctl.CodeUnknownMethod, wire.Code)
}

// TestServeOrdersEventsBeforeReplies pins the delivery rule a view relies on:
// events queued ahead of a reply arrive first, in order.
func TestServeOrdersEventsBeforeReplies(t *testing.T) {
	t.Parallel()
	f := newFakeSession(t)
	conn, _, events := f.dial(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := conn.Call(t.Context(), ctl.MethodState, nil, nil)
		assert.NoError(t, err)
	}()

	ev := waitEvent(t, events)
	assert.Equal(t, ctl.EventTurnStart, ev.Name)
	<-done
}

// TestSubmitCarriesLine pins the typed helper: submit round trips its line and
// the session-side echo event comes back through Serve.
func TestSubmitCarriesLine(t *testing.T) {
	t.Parallel()
	f := newFakeSession(t)
	conn, _, events := f.dial(t)

	require.NoError(t, conn.Submit(t.Context(), "hello session"))

	ev := waitEvent(t, events)
	assert.Equal(t, ctl.EventEcho, ev.Name)
	var d ctl.EchoData
	require.NoError(t, ev.Decode(&d))
	assert.Equal(t, "hello session", d.Text)
}

// TestCloseWakesCallers pins teardown: a closed connection fails an in-flight
// call that the session never answers, instead of hanging it.
func TestCloseWakesCallers(t *testing.T) {
	t.Parallel()
	f := newFakeSession(t)
	f.hangReplay = true // the replay request this test sends is never answered
	conn, _, _ := f.dial(t)

	errCh := make(chan error, 1)
	go func() {
		// events.replay is the fake's never-answered method; no call budget means
		// the wait can only end through Close
		_, err := conn.Call(t.Context(), ctl.MethodReplay, nil, nil)
		errCh <- err
	}()
	<-f.hung // the request reached the server: the call is registered in flight
	conn.Close()

	select {
	case err := <-errCh:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("call was not woken by Close")
	}
}

// TestDiscoverFindsLiveSockets pins discovery over a real control dir: the
// fake session's socket is found with its hello identity.
func TestDiscoverFindsLiveSockets(t *testing.T) {
	f := newFakeSession(t)
	t.Setenv(paths.EnvHome, f.home) // env pinned: not parallel

	live, err := Discover()
	require.NoError(t, err)
	require.Len(t, live, 1)
	assert.Equal(t, f.Path(), live[0].Path)
}

// TestResolveTargetForms pins the three target forms: an exact path, a session
// id in the control dir, and a unique prefix.
func TestResolveTargetForms(t *testing.T) {
	f := newFakeSession(t)
	t.Setenv(paths.EnvHome, f.home) // env pinned: not parallel

	live, err := Discover()
	require.NoError(t, err)
	require.Len(t, live, 1)

	t.Run("exact_path", func(t *testing.T) {
		path, _, rerr := Resolve(f.Path())
		require.NoError(t, rerr)
		assert.Equal(t, f.Path(), path)
	})

	t.Run("session_id", func(t *testing.T) {
		byID, _, rerr := Resolve(live[0].ID)
		require.NoError(t, rerr)
		assert.Equal(t, f.Path(), byID)
	})

	t.Run("unique_prefix", func(t *testing.T) {
		byPrefix, _, rerr := Resolve(live[0].ID[:8])
		require.NoError(t, rerr)
		assert.Equal(t, f.Path(), byPrefix)
	})

	t.Run("unknown_refused", func(t *testing.T) {
		_, _, rerr := Resolve("zz-nothing")
		require.Error(t, rerr)
	})
}

// TestReplayReturnsEventsPastSince pins the reconnect helper: retained events
// past a watermark arrive in order.
func TestReplayReturnsEventsPastSince(t *testing.T) {
	t.Parallel()
	f := newFakeSession(t)
	f.log = []ctl.Event{
		{Name: ctl.EventEcho, Seq: 3, Data: mustJSON(ctl.EchoData{Text: "old"})},
		{Name: ctl.EventEcho, Seq: 7, Data: mustJSON(ctl.EchoData{Text: "new"})},
	}
	conn, _, _ := f.dial(t)

	evs, err := conn.Replay(t.Context(), 5)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	assert.Equal(t, uint64(7), evs[0].Seq)

	// everything is past zero: the full retained history answers
	evs, err = conn.Replay(t.Context(), 0)
	require.NoError(t, err)
	require.Len(t, evs, 2)
}

// recordTo writes one JSONL transcript of two chained entries and names it in
// the fake's hello and state.get head.
func recordTo(t *testing.T, f *fakeSession) (first, second session.Entry) {
	t.Helper()

	path := filepath.Join(f.home, "session.jsonl")
	first = session.Entry{ID: "01ENTRYA", Type: session.TypeMessage, TS: 1,
		Data: mustJSON(session.MessageData{})}
	second = session.Entry{ID: "01ENTRYB", ParentID: first.ID,
		Type: session.TypeSession, TS: 2}
	var buf bytes.Buffer
	for _, e := range []session.Entry{first, second} {
		line, err := json.Marshal(e)
		require.NoError(t, err)
		buf.Write(line)
		buf.WriteByte('\n')
	}
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))

	f.mu.Lock()
	f.transcriptPath = path
	f.head = second.ID
	f.mu.Unlock()
	return first, second
}

// TestBackfillReadsRecordedBranch pins the bulk history source: one call
// returns the snapshot with its watermark plus the branch through the
// snapshot head, read from the file the hello named.
func TestBackfillReadsRecordedBranch(t *testing.T) {
	f := newFakeSession(t)
	first, second := recordTo(t, f)
	conn, _, _ := f.dial(t)

	assert.Equal(t, f.transcriptPath, conn.Hello().TranscriptPath)

	snap, branch, seq, err := conn.Backfill(t.Context())
	require.NoError(t, err)
	assert.Equal(t, second.ID, snap.Head)
	assert.Positive(t, seq) // the state.get watermark
	require.Len(t, branch, 2)
	assert.Equal(t, first.ID, branch[0].ID) // oldest first
	assert.Equal(t, second.ID, branch[1].ID)
}

// TestBackfillWithoutRecording pins the legible refusal: a hello with no
// transcript path answers an error naming it, never a confusing file failure.
func TestBackfillWithoutRecording(t *testing.T) {
	f := newFakeSession(t)
	conn, _, _ := f.dial(t)

	_, _, _, err := conn.Backfill(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "recording is off")
}

// TestReadBranchSkipsTornTail pins the concurrent-append guarantee: a final
// line the writer has not finished is skipped silently, complete lines stand.
func TestReadBranchSkipsTornTail(t *testing.T) {
	f := newFakeSession(t)
	first, second := recordTo(t, f)
	path := filepath.Join(f.home, "torn.jsonl")

	var buf bytes.Buffer
	for _, e := range []session.Entry{first, second} { // both fully written
		line, err := json.Marshal(e)
		require.NoError(t, err)
		buf.Write(line)
		buf.WriteByte('\n')
	}
	third, terr := json.Marshal(session.Entry{ID: "01ENTRYC", ParentID: second.ID,
		Type: session.TypeMessage, TS: 3}) // the writer is mid-line on this one
	require.NoError(t, terr)
	buf.WriteString(string(third)[:len(string(third))-4])
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))

	branch, err := ReadBranch(path, second.ID)
	require.NoError(t, err)
	require.Len(t, branch, 2) // the torn entry never half-appears
	assert.Equal(t, first.ID, branch[0].ID)
	assert.Equal(t, second.ID, branch[1].ID)
}
