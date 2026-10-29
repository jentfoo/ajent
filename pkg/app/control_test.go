package app

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/paths"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tui"
	"github.com/jentfoo/ajent/pkg/version"
)

// ctlWait bounds one line read from a control socket.
const ctlWait = 5 * time.Second

// controlRig is one in-process control plane: a hub, broker and transcript in
// an isolated home, with the server bound and an attachable client helper.
type controlRig struct {
	hub    *sessionHub
	broker *interactionBroker
	rec    *sessRec
	srv    *controlServer
	path   string
}

// newControlRig builds the control plane over a real transcript. pre, when
// set, records onto the transcript before the server attaches, the
// pre-attach history only backfill can see. Not parallel: AJENT_HOME pins the
// socket dir into the test's temp home.
func newControlRig(t *testing.T, pre func(rec *sessRec)) *controlRig {
	t.Helper()
	term, _ := pipeUI(t)
	return newControlRigUI(t, pre, term)
}

// shortHome pins AJENT_HOME to a short temp path, so a long test name never
// pushes the session's control socket path over the unix limit.
func shortHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "ctl")
	require.NoError(t, err)
	t.Setenv(config.EnvHome, home)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	return home
}

// newControlRigUI is newControlRig over one chosen terminal, so dialog tests
// can swap the local surface.
func newControlRigUI(t *testing.T, pre func(rec *sessRec), term *tui.UI) *controlRig {
	t.Helper()
	shortHome(t)

	broker := newInteractionBroker(term, nil, nil)
	hub := newSessionHub(hubDeps{
		ui: term, dialogs: broker, workspace: "/repo",
		cwd: "/repo", acct: tokens.New(llm.Model{}),
	}, hubEmitters{})

	store, err := session.NewStore()
	require.NoError(t, err)
	w, err := store.Create(config.Cwd(), session.SessionData{Version: session.Version(), Workspace: "/repo"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	rec := &sessRec{store: store, w: w, rec: session.NewRecorder(w)}
	entries, _, err := session.Read(w.Path())
	require.NoError(t, err)
	rec.hint = rootID(entries)
	hub.bindSession(rec)
	if pre != nil {
		pre(rec)
	}

	srv, _, err := startControl(hub, broker, rec, term == nil)
	require.NoError(t, err)
	srv.start() // the rig is fully wired at construction
	t.Cleanup(func() { srv.Close("test") })
	return &controlRig{hub: hub, broker: broker, rec: rec, srv: srv, path: srv.path}
}

// newServeRig is a rig whose hub has no terminal, the serve-mode shape. local,
// when set, rides the broker as the scripted local racer for dialog tests.
func newServeRig(t *testing.T, local localDialogs) *controlRig {
	t.Helper()
	shortHome(t)

	broker := newInteractionBroker(local, nil, nil)
	hub := newSessionHub(hubDeps{
		dialogs: broker, workspace: "/repo", cwd: "/repo", acct: tokens.New(llm.Model{}),
	}, hubEmitters{})
	return finishControlRig(t, hub, broker, true)
}

// newInteractiveRig builds an interactive rig whose broker races a scripted
// local surface, for dialog tests that need a terminal and a controllable
// racer at once.
func newInteractiveRig(t *testing.T, local localDialogs) *controlRig {
	t.Helper()
	shortHome(t)

	term, _ := pipeUI(t)
	broker := newInteractionBroker(local, nil, nil)
	hub := newSessionHub(hubDeps{
		ui: term, dialogs: broker, workspace: "/repo",
		cwd: "/repo", acct: tokens.New(llm.Model{}),
	}, hubEmitters{})
	return finishControlRig(t, hub, broker, false)
}

// finishControlRig opens the rig's transcript and binds the server onto an
// already-built hub and broker, wiring the driver-time switch callback a
// rewind needs for its head event.
func finishControlRig(t *testing.T, hub *sessionHub, broker *interactionBroker, serve bool) *controlRig {
	t.Helper()

	store, err := session.NewStore()
	require.NoError(t, err)
	w, err := store.Create(config.Cwd(), session.SessionData{Version: session.Version(), Workspace: "/repo"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	rec := &sessRec{store: store, w: w, rec: session.NewRecorder(w)}
	entries, _, err := session.Read(w.Path())
	require.NoError(t, err)
	rec.hint = rootID(entries)
	hub.bindSession(rec)
	rec.onSwitch = func([]llm.Message) {
		hub.syncLedger()
		hub.setHead(rec.w.Head())
	}

	srv, _, err := startControl(hub, broker, rec, serve)
	require.NoError(t, err)
	srv.start() // the rig is fully wired at construction
	t.Cleanup(func() { srv.Close("test") })
	return &controlRig{hub: hub, broker: broker, rec: rec, srv: srv, path: srv.path}
}

// ctlClient is one attached controller: a reader goroutine feeding a channel,
// so tests receive lines through deterministic channel waits.
type ctlClient struct {
	t        *testing.T
	conn     net.Conn
	enc      *ctl.Encoder
	inc      chan ctl.Incoming
	pending  []ctl.Event // events passed while a call waited for its reply
	closeErr error
}

// dialControl attaches to the rig's socket.
func dialControl(t *testing.T, path string) *ctlClient {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	c := &ctlClient{t: t, conn: conn, enc: ctl.NewEncoder(conn), inc: make(chan ctl.Incoming, 4096)}
	go func() {
		dec := ctl.NewDecoder(conn)
		for {
			line, rerr := readLine(dec)
			if rerr != nil {
				c.closeErr = rerr
				close(c.inc)
				return
			}
			in, derr := ctl.DecodeIncoming(line)
			if derr != nil {
				c.closeErr = derr
				close(c.inc)
				return
			}
			c.inc <- in
		}
	}()
	return c
}

// readLine pulls one raw line for client-side decoding.
func readLine(dec *ctl.Decoder) ([]byte, error) {
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// next waits for the next incoming line, failing the test on timeout.
func (c *ctlClient) next() ctl.Incoming {
	c.t.Helper()
	select {
	case in, ok := <-c.inc:
		require.True(c.t, ok)
		return in
	case <-time.After(ctlWait):
		c.t.Fatal("timed out waiting for a control line")
		return ctl.Incoming{}
	}
}

// hello reads the first line and decodes it.
func (c *ctlClient) hello() ctl.Hello {
	c.t.Helper()
	in := c.next()
	require.NotNil(c.t, in.Event)
	require.Equal(c.t, ctl.EventHello, in.Event.Name)
	var h ctl.Hello
	require.NoError(c.t, in.Event.Decode(&h))
	return h
}

// call sends one request and waits for its reply, keeping events it passes.
func (c *ctlClient) call(id, method string, params any) ctl.Reply {
	c.t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(c.t, err)
	require.NoError(c.t, c.enc.Encode(ctl.Request{ID: id, Method: method, Params: raw}))
	for {
		in := c.next()
		if in.Reply != nil {
			require.Equal(c.t, id, in.Reply.ID)
			return *in.Reply
		}
		c.pending = append(c.pending, *in.Event)
	}
}

// nextEvent waits for the next event line, skipping replies.
func (c *ctlClient) nextEvent() ctl.Event {
	c.t.Helper()
	for {
		in := c.next()
		require.NotNil(c.t, in.Event)
		return *in.Event
	}
}

// takePending hands over and clears the events a call waited through.
func (c *ctlClient) takePending() []ctl.Event {
	evts := c.pending
	c.pending = nil
	return evts
}

// collect gathers the first n events named name from pending and the stream.
func (c *ctlClient) collect(name string, n int) []ctl.Event {
	c.t.Helper()
	var out []ctl.Event
	take := func(ev ctl.Event) {
		if ev.Name == name {
			out = append(out, ev)
		}
	}
	for _, ev := range c.takePending() {
		take(ev)
	}
	for len(out) < n {
		take(c.nextEvent())
	}
	return out
}

// drainUntil keeps reading until reports true, discarding lines, so a client
// observing a flood never overflows its own buffers.
func (c *ctlClient) drainUntil(until func() bool) {
	c.t.Helper()
	for !until() {
		select {
		case _, ok := <-c.inc:
			require.True(c.t, ok)
		case <-time.After(ctlWait):
			c.t.Fatal("timed out draining control lines")
		}
	}
}

// awaitExit reads until session.exit arrives.
func (c *ctlClient) awaitExit(t *testing.T) {
	t.Helper()
	for {
		in := c.next()
		if in.Event != nil && in.Event.Name == ctl.EventSessionExit {
			return
		}
	}
}

// awaitSeatFree waits for release-on-EOF after a holder disconnects.
func (rig *controlRig) awaitSeatFree(t *testing.T) {
	t.Helper()
	require.Eventually(t, rig.srv.seatFree, ctlWait*2, 5*time.Millisecond)
}

// seatFree reports whether no connection holds the control seat.
func (s *controlServer) seatFree() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seat == nil
}

// TestControlSocketPathLayoutAndLength pins the socket location and the sun_path
// guard: a home deep enough to overflow the budget fails with a clear error
// rather than a truncated bind.
func TestControlSocketPathLayoutAndLength(t *testing.T) {
	// not parallel: AJENT_HOME pins the path root
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)

	id := "01J8Z9Q5W2K7D4R3M6T0V1B2C"
	path, warn, err := controlSocketPath(id)
	require.NoError(t, err)
	assert.Empty(t, warn) // a fresh directory carries the right mode already
	assert.Equal(t, filepath.Join(home, paths.ControlDirName, id+".sock"), path)
	fi, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm())

	// grow the home until the budget breaks: the id plus suffix still fits, so
	// the depth comes from the directory chain
	deep := home
	for len(deep) < controlSocketMax+64 {
		deep = filepath.Join(deep, "very-long-directory-name-padding-the-path")
	}
	t.Setenv(config.EnvHome, deep)
	_, _, err = controlSocketPath(id)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unix socket limit")
}

// TestBindControlListener covers bind's socket-file contract: a SIGKILL leftover
// with a dead peer is unlinked and rebound, an alive peer refuses the bind,
// and a non-socket file is never deleted.
func TestBindControlListener(t *testing.T) {
	t.Parallel()

	t.Run("stale_rebind", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "s.sock")
		ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", path)
		require.NoError(t, err)
		ul, ok := ln.(*net.UnixListener)
		require.True(t, ok)
		ul.SetUnlinkOnClose(false) // die without cleaning up, like a SIGKILL
		require.NoError(t, ln.Close())
		_, err = (&net.Dialer{}).DialContext(t.Context(), "unix", path) // dead peer probes refuse
		require.Error(t, err)

		ln2, err := bindControlListener(path, "sess-1")
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln2.Close() })

		c, err := (&net.Dialer{}).DialContext(t.Context(), "unix", path) // rebound socket accepts
		require.NoError(t, err)
		_ = c.Close()
	})

	t.Run("refuses_live_peer", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "s.sock")
		ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })

		_, err = bindControlListener(path, "01JSESS")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "01JSESS")
		assert.Contains(t, err.Error(), "already running")
	})

	t.Run("refuses_foreign_file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "s.sock")
		require.NoError(t, os.WriteFile(path, []byte("not a socket"), 0o600))

		_, err := bindControlListener(path, "s")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a socket")
		data, rerr := os.ReadFile(path)
		require.NoError(t, rerr)
		assert.Equal(t, "not a socket", string(data))
	})
}

// TestControlCloseUnlinksSocket pins the shutdown contract: Close removes the
// socket file so the next run on the session starts clean.
func TestControlCloseUnlinkSocket(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newControlRig(t, nil)

	_, err := os.Stat(rig.path)
	require.NoError(t, err)

	rig.srv.Close("done")
	_, err = os.Stat(rig.path)
	assert.True(t, os.IsNotExist(err))
}

// TestControlHelloAnnouncesIdentity pins the hello contract: protocol version,
// session identity, workspace, front-end shape, pid and build version, plus
// the transcript path bulk backfill reads.
func TestControlHelloAnnouncesIdentity(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newControlRig(t, nil)
	c := dialControl(t, rig.path)

	h := c.hello()
	assert.Equal(t, ctl.Protocol, h.Protocol)
	assert.Equal(t, rig.hub.Snapshot().SessionID, h.Session.ID)
	assert.Equal(t, "/repo", h.Workspace)
	assert.Equal(t, "/repo", h.Cwd)
	assert.True(t, h.Interactive)
	assert.False(t, h.Serve)
	assert.Equal(t, os.Getpid(), h.PID)
	assert.Equal(t, version.Version, h.Version)
	assert.Equal(t, rig.rec.w.Path(), h.TranscriptPath)
}

// TestControlHelloServeMode pins the serve shape: no terminal, socket only.
func TestControlHelloServeMode(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newServeRig(t, nil)
	c := dialControl(t, rig.path)

	h := c.hello()
	assert.False(t, h.Interactive)
	assert.True(t, h.Serve)
}

// serveModelsJSON is one never-dialed provider, so the serve run starts with a
// model configured and skips first-run setup.
const serveModelsJSON = `{
  "defaultModel": "fake/m1",
  "providers": {
    "fake": {
      "api": "openai-completions",
      "flavor": "generic",
      "baseUrl": "http://127.0.0.1:1",
      "models": [{ "id": "m1", "contextWindow": 100000, "maxTokens": 4096 }]
    }
  }
}`

// writeServeHome configures an isolated home with control enabled and a model.
func writeServeHome(t *testing.T, controlEnabled bool) {
	t.Helper()
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	require.NoError(t, os.WriteFile(filepath.Join(home, llm.ModelsFileName), []byte(serveModelsJSON), 0o600))
	var conf string
	if controlEnabled {
		conf = `{"control": {"enabled": true}}`
	} else {
		conf = `{}`
	}
	require.NoError(t, os.WriteFile(filepath.Join(home, config.ConfigFileName), []byte(conf), 0o600))
}

// onlySocket reports the one live control socket under the config dir.
func onlySocket(t *testing.T) (string, bool) {
	t.Helper()
	dir, err := paths.Dir()
	require.NoError(t, err)
	entries, rerr := os.ReadDir(filepath.Join(dir, paths.ControlDirName))
	if rerr != nil || len(entries) != 1 {
		return "", false
	}
	name := entries[0].Name()
	if !strings.HasSuffix(name, ".sock") {
		return "", false
	}
	return filepath.Join(dir, paths.ControlDirName, name), true
}

// TestServeRun pins the serve contract end to end: without control.enabled the
// run refuses with a usage error; with it, the socket binds before serving, a
// controller attaches and polls, SIGTERM ends the run cleanly with every
// connection seeing session.exit before the socket unlinks, and /exit through
// submit is the controller's kill switch.
func TestServeRun(t *testing.T) {
	// not parallel: AJENT_HOME isolates config, socket dir and sessions

	t.Run("needs_control_enabled", func(t *testing.T) {
		writeServeHome(t, false)

		assert.Equal(t, ExitUsage, Run(RunOptions{Serve: true}))
	})

	t.Run("lifecycle_sigterm", func(t *testing.T) {
		writeServeHome(t, true)

		code := make(chan int, 1)
		go func() { code <- Run(RunOptions{Serve: true}) }()

		// the socket appears before the run accepts, one per session
		var path string
		require.Eventually(t, func() bool {
			p, ok := onlySocket(t)
			if ok {
				path = p
			}
			return ok
		}, 10*time.Second, 10*time.Millisecond)

		// a controller attaches: serve front end, session identity, pollable state
		c := dialControl(t, path)
		h := c.hello()
		assert.True(t, h.Serve)
		assert.False(t, h.Interactive)
		assert.NotEmpty(t, h.Session.ID)

		rp := c.call("s1", ctl.MethodState, map[string]any{})
		require.Nil(t, rp.Error)

		// SIGTERM ends the run: the signal handler installed before the bind, so
		// the socket above proves the signal will be caught
		require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))

		select {
		case exit := <-code:
			assert.Equal(t, ExitOK, exit)
		case <-time.After(ctlWait):
			t.Fatal("the serve run never exited on SIGTERM")
		}

		// session.exit lands before the socket is gone
		exitSeen := false
		for !exitSeen {
			in := c.next()
			if in.Event != nil && in.Event.Name == ctl.EventSessionExit {
				exitSeen = true
			}
		}
		require.Eventually(t, func() bool {
			_, derr := os.Stat(path)
			return os.IsNotExist(derr)
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("ends_through_exit", func(t *testing.T) {
		writeServeHome(t, true)

		code := make(chan int, 1)
		go func() { code <- Run(RunOptions{Serve: true}) }()

		var path string
		require.Eventually(t, func() bool {
			p, ok := onlySocket(t)
			if ok {
				path = p
			}
			return ok
		}, 10*time.Second, 10*time.Millisecond)

		ctlConn := dialControl(t, path)
		h := ctlConn.hello()
		assert.True(t, h.Serve)
		rp := ctlConn.call("seat", ctl.MethodAttach, map[string]any{"control": true, "holder": "ci"})
		require.Nil(t, rp.Error)

		// a second observer sees the same exit sequence
		obs := dialControl(t, path)
		obs.hello()

		rp = ctlConn.call("exit", ctl.MethodSubmit, map[string]any{"line": "/exit"})
		require.Nil(t, rp.Error)

		select {
		case exit := <-code:
			assert.Equal(t, ExitOK, exit)
		case <-time.After(ctlWait):
			t.Fatal("the serve run never exited on /exit")
		}

		// every connection receives session.exit then EOF, and the socket unlinks
		for _, c := range []*ctlClient{ctlConn, obs} {
			c.awaitExit(t)
			select {
			case _, ok := <-c.inc:
				assert.False(t, ok) // EOF follows session.exit
			case <-time.After(ctlWait):
				t.Fatal("EOF never followed session.exit")
			}
		}
		require.Eventually(t, func() bool {
			_, derr := os.Stat(path)
			return os.IsNotExist(derr)
		}, 5*time.Second, 10*time.Millisecond)
	})
}
