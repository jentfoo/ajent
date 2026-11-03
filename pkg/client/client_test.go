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
	recall         ctl.RecallResult      // canned queue.recall answer
	input          string                // last input.set text received
	hangState      bool                  // never answer state.get: the close-wakeup test hangs on it
	hung           chan struct{}         // closed once a hangState request reaches serveConn
	toolsSet       []string              // last tools.set names received
	settingSet     ctl.SettingsSetParams // last settings.set params received
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

// inputText reports the last input.set text the fake received.
func (f *fakeSession) inputText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.input
}

// toolsSetNames reports the last tools.set names the fake received.
func (f *fakeSession) toolsSetNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.toolsSet
}

// settingSetParams reports the last settings.set params the fake received.
func (f *fakeSession) settingSetParams() ctl.SettingsSetParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settingSet
}

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
		case ctl.MethodInputSet:
			var p ctl.InputSetParams
			if json.Unmarshal(req.Params, &p) == nil {
				f.mu.Lock()
				f.input = p.Text
				f.mu.Unlock()
			}
			_ = enc.Encode(ctl.Reply{ID: req.ID})
		case ctl.MethodQueueRecall:
			_ = enc.Encode(ctl.Reply{ID: req.ID, Result: mustJSON(f.recall)})
		case ctl.MethodSeatYield:
			_ = enc.Encode(ctl.Reply{ID: req.ID})
		case ctl.MethodToolsSet:
			var p ctl.ToolsSetParams
			_ = json.Unmarshal(req.Params, &p)
			f.mu.Lock()
			f.toolsSet = p.Names
			f.mu.Unlock()
			var rows []ctl.ToolState
			if len(p.Names) > 0 {
				rows = []ctl.ToolState{{Name: p.Names[0], Enabled: true}}
			}
			_ = enc.Encode(ctl.Reply{ID: req.ID, Result: mustJSON(ctl.ToolsSetResult{Rows: rows})})
		case ctl.MethodModelsGet:
			_ = enc.Encode(ctl.Reply{ID: req.ID, Result: mustJSON(ctl.ModelsData{
				Active: "test/alpha", Models: []ctl.ModelState{{Key: "test/alpha"}}})})
		case ctl.MethodModelSet:
			var p ctl.ModelSetParams
			_ = json.Unmarshal(req.Params, &p)
			if p.Model == "missing" {
				_ = enc.Encode(ctl.Reply{ID: req.ID, Error: ctl.Errf(ctl.CodeNotFound, "no model matches %s", p.Model)})
				continue
			}
			_ = enc.Encode(ctl.Reply{ID: req.ID, Result: mustJSON(ctl.ModelSetResult{
				Model: ctl.ModelState{Key: p.Model}})})
		case ctl.MethodModeSet:
			var p ctl.ModeSetParams
			_ = json.Unmarshal(req.Params, &p)
			// ModeSetParams and ModeResult share one field, so the conversion carries it
			_ = enc.Encode(ctl.Reply{ID: req.ID, Result: mustJSON(ctl.ModeResult(p))})
		case ctl.MethodSettingsGet:
			_ = enc.Encode(ctl.Reply{ID: req.ID, Result: mustJSON(ctl.SettingsData{Rows: []ctl.SettingRow{
				{Label: "Block images", Key: "images.block", Value: json.RawMessage("false"), Source: "default", Editable: true}}})})
		case ctl.MethodSettingsSet:
			var p ctl.SettingsSetParams
			_ = json.Unmarshal(req.Params, &p)
			f.mu.Lock()
			f.settingSet = p
			f.mu.Unlock()
			_ = enc.Encode(ctl.Reply{ID: req.ID, Result: mustJSON(ctl.SettingsSetResult{Row: ctl.SettingRow{
				Label: "x", Key: p.Key, Value: p.Value, Source: "session", Editable: true}})})
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

func TestQueueRecall(t *testing.T) {
	t.Parallel()

	t.Run("decodes_row", func(t *testing.T) {
		f := newFakeSession(t)
		f.mu.Lock()
		f.recall = ctl.RecallResult{ID: "c1", Text: "queued prompt"}
		f.mu.Unlock()
		conn, _, _ := f.dial(t)

		res, err := conn.QueueRecall(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "c1", res.ID)
		assert.Equal(t, "queued prompt", res.Text)
	})

	t.Run("empty_when_nothing_queued", func(t *testing.T) {
		f := newFakeSession(t)
		conn, _, _ := f.dial(t)

		res, err := conn.QueueRecall(t.Context())
		require.NoError(t, err)
		assert.Zero(t, res)
	})
}

func TestInputSet(t *testing.T) {
	t.Parallel()
	f := newFakeSession(t)
	conn, _, _ := f.dial(t)

	require.NoError(t, conn.InputSet(t.Context(), "draft text"))
	assert.Equal(t, "draft text", f.inputText())
}

func TestYieldSeat(t *testing.T) {
	t.Parallel()
	f := newFakeSession(t)
	conn, _, _ := f.dial(t)

	require.NoError(t, conn.YieldSeat(t.Context()))
}

func TestToolsSetCarriesNames(t *testing.T) {
	t.Parallel()
	f := newFakeSession(t)
	conn, _, _ := f.dial(t)

	res, err := conn.ToolsSet(t.Context(), []string{"read", "ls"})
	require.NoError(t, err)
	require.Len(t, res.Rows, 1)
	assert.Equal(t, "read", res.Rows[0].Name)
	assert.True(t, res.Rows[0].Enabled)
	assert.Equal(t, []string{"read", "ls"}, f.toolsSetNames())
}

func TestModels(t *testing.T) {
	t.Parallel()

	t.Run("lists", func(t *testing.T) {
		f := newFakeSession(t)
		conn, _, _ := f.dial(t)

		res, err := conn.Models(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "test/alpha", res.Active)
		require.Len(t, res.Models, 1)
		assert.Equal(t, "test/alpha", res.Models[0].Key)
	})

	t.Run("set_decodes_and_reports_refusals", func(t *testing.T) {
		f := newFakeSession(t)
		conn, _, _ := f.dial(t)

		res, err := conn.ModelSet(t.Context(), "beta")
		require.NoError(t, err)
		assert.Equal(t, "beta", res.Model.Key)

		_, err = conn.ModelSet(t.Context(), "missing")
		var we *ctl.Error
		require.ErrorAs(t, err, &we)
		assert.Equal(t, ctl.CodeNotFound, we.Code)
	})
}

func TestModeSet(t *testing.T) {
	t.Parallel()
	f := newFakeSession(t)
	conn, _, _ := f.dial(t)

	mode, err := conn.ModeSet(t.Context(), "auto+write")
	require.NoError(t, err)
	assert.Equal(t, "auto+write", mode)
}

func TestSettings(t *testing.T) {
	t.Parallel()

	t.Run("lists_rows", func(t *testing.T) {
		f := newFakeSession(t)
		conn, _, _ := f.dial(t)

		res, err := conn.Settings(t.Context())
		require.NoError(t, err)
		require.Len(t, res.Rows, 1)
		assert.Equal(t, "images.block", res.Rows[0].Key)
		assert.Equal(t, "default", res.Rows[0].Source)
	})

	t.Run("set_round_trips", func(t *testing.T) {
		f := newFakeSession(t)
		conn, _, _ := f.dial(t)

		res, err := conn.SettingsSet(t.Context(), ctl.SettingsSetParams{
			Key: "images.block", Value: json.RawMessage("true"), Persist: "user"})
		require.NoError(t, err)
		assert.Equal(t, "images.block", res.Row.Key)
		assert.Equal(t, "session", res.Row.Source)
		assert.JSONEq(t, "true", string(f.settingSetParams().Value))
		assert.Equal(t, "user", f.settingSetParams().Persist)
	})
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
