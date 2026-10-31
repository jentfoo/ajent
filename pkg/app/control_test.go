package app

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/client"
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
// pushes the session's control socket path over the unix limit: t.TempDir embeds
// the test name, which is exactly what blows the budget.
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
		ui: term, dialogs: broker, workspace: config.Cwd(),
		cwd: "/repo", acct: tokens.New(llm.Model{}),
	})

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

	srv, _, err := startControl(t.Context(), hub, broker, rec, term == nil, nil)
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
	})
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
	})
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
	rec.onSwitch = func([]llm.Message, string) {
		hub.syncLedger()
		hub.setHead(rec.w.Head(), rewindCauseRewind)
	}

	srv, _, err := startControl(t.Context(), hub, broker, rec, serve, nil)
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
	in, ok := <-c.inc
	require.True(c.t, ok)
	return in
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

// TestTokenSuffixDiffersSameMillisecond pins the instance file names: tokenSuffix
// takes the random tail of a token, so tokens minted in the same millisecond
// (identical ULID timestamp prefixes) still name different files.
func TestTokenSuffixDiffersSameMillisecond(t *testing.T) {
	t.Parallel()
	a := "01ABCD0123456789ABCDEFGHXY"
	b := "01ABCD0123456789ZZZZZZZZZZZZ"
	assert.Equal(t, a[:10], b[:10]) // same timestamp prefix
	assert.NotEqual(t, tokenSuffix(a), tokenSuffix(b))
	assert.Equal(t, a[len(a)-tokenPrefixLen:], tokenSuffix(a))
}

// TestPidDead separates proof of death from everything undecidable: only a
// reaped pid collects, so PID reuse and signal refusals never unlink a live peer.
func TestPidDead(t *testing.T) {
	t.Parallel()

	t.Run("zero_pid_undecidable", func(t *testing.T) {
		assert.False(t, pidDead(0))
	})

	t.Run("self_pid_alive", func(t *testing.T) {
		assert.False(t, pidDead(os.Getpid()))
	})

	t.Run("reaped_child_dead", func(t *testing.T) {
		cmd := exec.CommandContext(t.Context(), "true")
		require.NoError(t, cmd.Run())
		assert.True(t, pidDead(cmd.ProcessState.Pid()))
	})
}

// TestCollectStaleNeverDials pins the passive sweep: a live peer's files
// survive a sweep without its socket ever accepting a connection.

// TestControlCollectStale pins the passive registry sweep: a live peer's files
// survive without its socket ever accepting a connection, and dead instances are
// collected with their sockets while this instance is never judged.
func TestControlCollectStale(t *testing.T) {
	t.Run("never_dials", func(t *testing.T) {
		rig := newServeRig(t, nil) // not parallel: AJENT_HOME pins the control dir
		// a fresh registry for a live foreign pid: the sweep must leave it alone
		foreign := filepath.Join(filepath.Dir(rig.srv.regPath), "deadbeef.json")
		reg := controlRegistry{SessionID: "other", PID: os.Getpid(), Token: "deadbeef",
			Socket: filepath.Join(filepath.Dir(rig.srv.regPath), "deadbeef.sock")}
		b, err := json.Marshal(reg)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(foreign, b, 0o600))
		ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", reg.Socket)
		require.NoError(t, err)
		var accepted atomic.Bool
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				accepted.Store(true)
				_ = c.Close()
			}
		}()
		t.Cleanup(func() { _ = ln.Close() })

		rig.srv.collectStale()

		_, err = os.Stat(foreign)
		require.NoError(t, err)
		assert.False(t, accepted.Load())
	})

	// the registry GC: an entry whose writer pid is dead and old enough is
	// collected with its socket, empty session dirs go with it, and this
	// instance's own record is never judged.
	t.Run("removes_dead_instances", func(t *testing.T) {
		rig := newControlRig(t, nil)
		base := filepath.Dir(rig.srv.regPath) // <control>/<sessionID>
		root := filepath.Dir(base)            // <control>

		// a dead sibling in its own session directory, old enough to judge: its
		// writer pid is a process this one reaped, so no dial is needed to know it
		gone := exec.CommandContext(t.Context(), "true")
		require.NoError(t, gone.Run())
		deadDir := filepath.Join(root, "01DEADSESSION000000000000000")
		require.NoError(t, os.MkdirAll(deadDir, paths.DirPerm))
		deadSock := filepath.Join(deadDir, "deadbeef.sock")
		deadReg := filepath.Join(deadDir, "deadbeef.json")
		writeControlDead := controlRegistry{SessionID: "01DEADSESSION000000000000000",
			Token: "deadbeef0000000000000000", Socket: deadSock, PID: gone.ProcessState.Pid()}
		b, err := json.Marshal(writeControlDead)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(deadReg, b, 0o600))
		past := time.Now().Add(-2 * gcStaleRegistry)
		require.NoError(t, os.Chtimes(deadReg, past, past))

		// this instance's own token is never judged: a stale self record survives,
		// whatever its pid says
		ownDir := filepath.Join(root, "01OWNSESSION000000000000000")
		require.NoError(t, os.MkdirAll(ownDir, paths.DirPerm))
		ownReg := filepath.Join(ownDir, "selfrecord.json")
		writeControlOwn := controlRegistry{SessionID: "01OWNSESSION000000000000000",
			Token: rig.srv.token, Socket: filepath.Join(ownDir, "selfrecord.sock"), PID: gone.ProcessState.Pid()}
		b, err = json.Marshal(writeControlOwn)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(ownReg, b, 0o600))
		require.NoError(t, os.Chtimes(ownReg, past, past))

		rig.srv.collectStale()

		assert.NoFileExists(t, deadReg)
		assert.NoFileExists(t, deadSock)
		assert.NoDirExists(t, deadDir) // the emptied session dir went too
		require.FileExists(t, ownReg)  // the own-token record survived untouched

		// the live instance survives its own GC pass
		require.FileExists(t, rig.srv.path)
		require.FileExists(t, rig.srv.regPath)
	})
}

// TestControlLayoutPinsPerInstanceNames pins the per-instance file layout and
// the sun_path guard: socket and registry live under one per-session directory
// named for the token prefix, and a home deep enough to overflow the budget
// fails with a clear error rather than a truncated bind.
func TestControlLayoutPinsPerInstanceNames(t *testing.T) {
	t.Run("pins_per_instance_names", func(t *testing.T) {
		home := shortHome(t)

		id := "01J8Z9Q5W2K7D4R3M6"
		token := "01ABCD0123456789ABCDEFGHXY"
		dir, sock, reg, warn, err := controlLayout(id, token)
		require.NoError(t, err)
		assert.Empty(t, warn) // a fresh directory carries the right mode already
		assert.Equal(t, filepath.Join(home, paths.ControlDirName, id), dir)
		assert.Equal(t, filepath.Join(dir, tokenSuffix(token)+".sock"), sock)
		assert.Equal(t, filepath.Join(dir, tokenSuffix(token)+".json"), reg)
		fi, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm())
	})

	t.Run("overflow_fails_legibly", func(t *testing.T) {
		home := shortHome(t)

		id := "01J8Z9Q5W2K7D4R3M6"
		token := "01ABCD0123456789ABCDEFGHXY"

		// grow the home until the budget breaks: the session dir plus token prefix
		// still fits, so the depth comes from the directory chain
		deep := home
		for len(deep) < controlSocketMax+64 {
			deep = filepath.Join(deep, "very-long-directory-name-padding-the-path")
		}
		t.Setenv(config.EnvHome, deep)
		_, _, _, _, err := controlLayout(id, token)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unix socket limit")
	})
}

// TestBindControlListener covers bind's socket-file contract: a SIGKILL
// leftover at this token's own path is unlinked and rebound, and a non-socket
// file is never deleted. There is no probe: sockets are per instance, so no
// live peer can sit at this path, and bindControlListener itself never dials.
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
		_, err = os.Stat(path)
		require.NoError(t, err) // the leftover is what bind has to clear

		ln2, err := bindControlListener(t.Context(), path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln2.Close() })

		c, err := (&net.Dialer{}).DialContext(t.Context(), "unix", path) // rebound socket accepts
		require.NoError(t, err)
		_ = c.Close()
	})

	t.Run("rebinds_own_live_path", func(t *testing.T) {
		// a second bind at this instance's own path wins by unlinking the first:
		// the file belongs to this token, so a wedged predecessor never blocks a
		// restart. The first listener's fd survives until its own Close.
		path := filepath.Join(t.TempDir(), "s.sock")
		ln, err := bindControlListener(t.Context(), path)
		require.NoError(t, err)

		ln2, err := bindControlListener(t.Context(), path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln2.Close() })

		c, err := (&net.Dialer{}).DialContext(t.Context(), "unix", path)
		require.NoError(t, err) // the rebound listener serves, not the abandoned one
		_ = c.Close()
		_ = ln.Close()
	})

	t.Run("refuses_foreign_file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "s.sock")
		require.NoError(t, os.WriteFile(path, []byte("not a socket"), 0o600))

		_, err := bindControlListener(t.Context(), path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a socket")
		data, rerr := os.ReadFile(path)
		require.NoError(t, rerr)
		assert.Equal(t, "not a socket", string(data))
	})
}

// TestControlRegistryTracksInstanceState pins the discovery record: written
// after listen with the instance identity, rewritten on seat claim and yield,
// so pickers stay current without dialing.
func TestControlRegistryTracksInstanceState(t *testing.T) {
	rig := newControlRig(t, nil)

	raw, err := os.ReadFile(rig.srv.regPath)
	require.NoError(t, err)
	var reg controlRegistry
	require.NoError(t, json.Unmarshal(raw, &reg))
	assert.Equal(t, rig.hub.Snapshot().SessionID, reg.SessionID)
	assert.Equal(t, rig.srv.token, reg.Token)
	assert.Equal(t, rig.srv.path, reg.Socket)
	assert.Positive(t, reg.PID)
	assert.False(t, reg.SeatHeld)

	// the wire shape agrees with the discovery reader: the same file decodes as
	// a client.Instance with its start time intact
	var asInstance client.Instance
	require.NoError(t, json.Unmarshal(raw, &asInstance))
	assert.False(t, asInstance.Started().IsZero())
	assert.Equal(t, rig.srv.token, asInstance.Token)

	c := dialControl(t, rig.path)
	c.hello()
	rp := c.call("a", ctl.MethodAttach, map[string]any{"control": true, "holder": "ci"})
	require.Nil(t, rp.Error)

	raw, err = os.ReadFile(rig.srv.regPath)
	require.NoError(t, err)
	reg = controlRegistry{}
	require.NoError(t, json.Unmarshal(raw, &reg))
	assert.True(t, reg.SeatHeld)
	started := reg.StartedAt

	// yield hands the seat back and the registry follows
	rp = c.call("y", ctl.MethodSeatYield, map[string]any{})
	require.Nil(t, rp.Error)
	raw, err = os.ReadFile(rig.srv.regPath)
	require.NoError(t, err)
	reg = controlRegistry{} // reuse merges: absent fields must not keep old values
	require.NoError(t, json.Unmarshal(raw, &reg))
	assert.False(t, reg.SeatHeld)
	assert.Equal(t, started, reg.StartedAt) // stamped once, rewrites never drift it

	// close removes this instance's own files only
	rig.srv.Close("test")
	assert.NoFileExists(t, rig.srv.path)
	assert.NoFileExists(t, rig.srv.regPath)
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

// TestControlCloseDrainsLanes pins the shutdown ordering: with a rewind
// stalled on the state lane and a verb stalled on the executor lane, the exit
// event holds until both finish, so no branch.changed can publish after it.
func TestControlCloseDrainsLanes(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newControlRig(t, nil)
	lane := newStateLane()
	rig.srv.lane = lane

	c := dialControl(t, rig.path)
	c.hello()
	var exited atomic.Bool
	go func() {
		for in := range c.inc {
			if in.Event != nil && in.Event.Name == ctl.EventSessionExit {
				exited.Store(true)
			}
		}
	}()

	// both stalls release exactly once, from the body or from cleanup on a
	// failed assertion, so Close never waits its bound out needlessly
	verbRelease := make(chan struct{})
	var releaseVerb, freeLane sync.Once
	t.Cleanup(func() {
		releaseVerb.Do(func() { close(verbRelease) })
		freeLane.Do(lane.unlock)
	})
	require.True(t, rig.srv.exec.run(func() { <-verbRelease }))
	lane.lock()

	closed := make(chan struct{})
	go func() {
		rig.srv.Close("done")
		close(closed)
	}()
	require.Never(t, exited.Load, 150*time.Millisecond, 10*time.Millisecond)

	freeLane.Do(lane.unlock)
	releaseVerb.Do(func() { close(verbRelease) })
	require.Eventually(t, exited.Load, 5*time.Second, 5*time.Millisecond)
	<-closed
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
	assert.Equal(t, config.Cwd(), h.Workspace) // the recall-store key a host keyboard would use
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

// onlySocket reports the one live control socket under the config dir: one
// per-session directory holding one instance's socket.
func onlySocket(t *testing.T) (string, bool) {
	t.Helper()
	dir, err := paths.Dir()
	require.NoError(t, err)
	sessions, rerr := os.ReadDir(filepath.Join(dir, paths.ControlDirName))
	if rerr != nil || len(sessions) != 1 || !sessions[0].IsDir() {
		return "", false
	}
	files, ferr := os.ReadDir(filepath.Join(dir, paths.ControlDirName, sessions[0].Name()))
	if ferr != nil {
		return "", false
	}
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".sock") {
			return filepath.Join(dir, paths.ControlDirName, sessions[0].Name(), f.Name()), true
		}
	}
	return "", false
}

// TestServeRun pins the serve contract end to end: the socket binds before
// serving, a controller attaches and polls, SIGTERM ends the run cleanly with
// every connection seeing session.exit before this instance's files unlink,
// and /exit through submit is the controller's kill switch. Serve implies
// control whatever control.enabled says: the socket is the only front end.
func TestServeRun(t *testing.T) {
	// not parallel: AJENT_HOME isolates config, socket dir and sessions

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

		exit := <-code
		assert.Equal(t, ExitOK, exit)

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

		exit := <-code
		assert.Equal(t, ExitOK, exit)

		// every connection receives session.exit then EOF, and the socket unlinks
		for _, c := range []*ctlClient{ctlConn, obs} {
			c.awaitExit(t)
			_, ok := <-c.inc
			assert.False(t, ok) // EOF follows session.exit
		}
		require.Eventually(t, func() bool {
			_, derr := os.Stat(path)
			return os.IsNotExist(derr)
		}, 5*time.Second, 10*time.Millisecond)
	})
}
