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
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/paths"
	"github.com/jentfoo/ajent/pkg/session"
)

// fakeSession is a minimal control-socket server: hello, then canned replies
// keyed by method. Its registry file sits where a real instance writes one, so
// discovery resolves it by file like any live instance.
type fakeSession struct {
	ln     net.Listener
	home   string
	mu     sync.Mutex
	staged []ctl.Event // events staged for the next connection's stream

	transcriptPath string // hello names this file when set; state.get reports head
	head           string
	sessionID      string
	hangState      bool          // never answer state.get: the close-wakeup test hangs on it
	hung           chan struct{} // closed once a hangState request reaches serveConn
}

// newFakeSession binds a socket in a short temp dir and starts accepting. It
// pins nothing: discovery tests point AJENT_HOME at f.home themselves.
func newFakeSession(t *testing.T) *fakeSession {
	t.Helper()
	home, err := os.MkdirTemp("", "client-test")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	const id = "01JFAKESESSIONID000000"
	dir := filepath.Join(home, paths.ControlDirName, id)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", filepath.Join(dir, "deadbeef.sock"))
	require.NoError(t, err)

	f := &fakeSession{ln: ln, home: home, sessionID: id, hung: make(chan struct{})}
	t.Cleanup(func() { _ = ln.Close() })
	go f.acceptLoop()
	return f
}

// writeRegistry publishes the fake's discovery record, the file a real
// instance writes after listen.
func (f *fakeSession) writeRegistry(t *testing.T) {
	t.Helper()
	b, err := json.Marshal(Instance{
		SessionID: f.sessionID, Token: "deadbeef0000000000000000000",
		Socket: f.Path(), PID: os.Getpid(), StartedAt: time.Now().UnixMilli(),
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(f.Path()), "deadbeef.json"), b, 0o600))
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

// waitEvent pulls one event; a stall fails via the test timeout.
func waitEvent(t *testing.T, events <-chan ctl.Event) ctl.Event {
	t.Helper()
	return <-events
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
	events := append([]ctl.Event(nil), f.staged...)
	f.staged = nil // staged events precede the request loop, like live fanout
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
			f.mu.Lock()
			hang := f.hangState
			f.mu.Unlock()
			if hang {
				close(f.hung) // the request arrived: any waiting caller may rely on it
				return        // never answered: the close-wakeup test hangs on this
			}
			seq++
			_ = enc.Encode(ctl.NewEvent(ctl.EventTurnStart, seq, ctl.TurnStartData{Model: "p/m"}))
			_ = enc.Encode(ctl.Reply{ID: req.ID, Seq: seq,
				Result: mustJSON(ctl.State{Session: ctl.Session{ID: "01JTEST"}, Head: head})})
		case ctl.MethodSubmit:
			var p ctl.SubmitParams
			_ = json.Unmarshal(req.Params, &p)
			// the committed rendering rides entry.append; there is no echo
			seq++
			_ = enc.Encode(ctl.NewEvent(ctl.EventEntryAppend, seq, ctl.EntryData{
				Entry: ctl.Entry{ID: "01COMMITTED0000000000000000",
					Type: string(session.TypeMessage),
					Data: mustJSON(session.MessageData{Message: llm.Text(llm.RoleUser, p.Line)})},
			}))
			_ = enc.Encode(ctl.Reply{ID: req.ID, Seq: seq})
		case ctl.MethodInterrupt:
			_ = enc.Encode(ctl.Reply{ID: req.ID,
				Result: mustJSON(ctl.InterruptResult{Stopped: "turn"})})
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

// TestDial pins connection setup: the greeting decodes into a usable conn and
// a missing socket refuses.
func TestDial(t *testing.T) {
	t.Run("reads_hello", func(t *testing.T) {
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
	})

	t.Run("refuses_missing_socket", func(t *testing.T) {
		_, _, err := Dial(filepath.Join(t.TempDir(), "missing.sock"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no such socket")
	})
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
// the committed rendering arrives as its entry, never an echo.
func TestSubmitCarriesLine(t *testing.T) {
	t.Parallel()
	f := newFakeSession(t)
	conn, _, events := f.dial(t)

	require.NoError(t, conn.Submit(t.Context(), ctl.SubmitParams{Line: "hello session"}))

	ev := waitEvent(t, events)
	assert.Equal(t, ctl.EventEntryAppend, ev.Name)
	var d ctl.EntryData
	require.NoError(t, ev.Decode(&d))
	var md session.MessageData
	require.NoError(t, json.Unmarshal(d.Entry.Data, &md))
	assert.Equal(t, "hello session", llm.FirstBlockText(md.Message.Content))
}

// TestCloseWakesCallers pins teardown: a closed connection fails an in-flight
// call that the session never answers, instead of hanging it.
func TestCloseWakesCallers(t *testing.T) {
	t.Parallel()
	f := newFakeSession(t)
	f.mu.Lock()
	f.hangState = true // the state.get this test sends is never answered
	f.mu.Unlock()
	conn, _, _ := f.dial(t)

	errCh := make(chan error, 1)
	go func() {
		// state.get is the fake's never-answered method; no call budget means
		// the wait can only end through Close
		_, err := conn.Call(t.Context(), ctl.MethodState, nil, nil)
		errCh <- err
	}()
	<-f.hung // the request reached the server: the call is registered in flight
	conn.Close()

	err := <-errCh
	require.Error(t, err)
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

// TestBackfill pins the bulk history source: one call returns snapshot plus
// watermark with the branch through the head, and no recording is a clean empty.
func TestBackfill(t *testing.T) {
	t.Run("reads_recorded_branch", func(t *testing.T) {
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
	})

	t.Run("without_recording", func(t *testing.T) {
		f := newFakeSession(t)
		conn, _, _ := f.dial(t)

		_, _, _, err := conn.Backfill(t.Context())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "recording is off")
	})
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
