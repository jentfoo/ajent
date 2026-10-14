package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-analyze/bulk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
)

// newTestServer builds a server with no live connection, for direct register()
// unit checks.
func (m *Manager) newTestServer(name string) *server {
	return m.newServer(name, ServerConfig{})
}

// fakeRegistrar records registrations so manager tests can inspect state without
// importing pkg/tools. It keeps both the latest tool object and its state by name.
type fakeRegistrar struct {
	mu       sync.Mutex
	tool     map[string]State // namespaced name to state, latest registration wins
	impls    map[string]agent.Tool
	readOnly []string // names marked read-only via MarkReadOnly
}

func newFakeRegistrar() *fakeRegistrar {
	return &fakeRegistrar{tool: make(map[string]State), impls: make(map[string]agent.Tool)}
}

// RegisterState records the tool and its state under source.
func (f *fakeRegistrar) RegisterState(source string, t agent.Tool, s State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = source
	f.tool[t.Name()] = s
	f.impls[t.Name()] = t
}

// Unregister drops every tool registered under a source.
func (f *fakeRegistrar) Unregister(source string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for n := range f.impls {
		if sourceOf(n) == source {
			delete(f.tool, n)
			delete(f.impls, n)
		}
	}
}

// EnabledNames returns names currently registered enabled.
func (f *fakeRegistrar) EnabledNames(source string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n, s := range f.tool {
		if s == StateEnabled && sourceOf(n) == source {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// DisabledNames returns names currently registered disabled under source.
func (f *fakeRegistrar) DisabledNames(source string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n, s := range f.tool {
		if s == StateDisabled && sourceOf(n) == source {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// AllNames returns every name registered under source regardless of state.
func (f *fakeRegistrar) AllNames(source string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n := range f.tool {
		if sourceOf(n) == source {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// MarkReadOnly records the named tools as read-only.
func (f *fakeRegistrar) MarkReadOnly(names []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readOnly = append(f.readOnly, names...)
}

// readonly returns the recorded read-only names.
func (f *fakeRegistrar) readonly() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.readOnly)
}

func (f *fakeRegistrar) state(name string) (State, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.tool[name]
	return st, ok
}

// set overrides the recorded state for an already-registered name, standing in
// for a /tools toggle against the real registry.
func (f *fakeRegistrar) set(name string, s State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.tool[name]; ok {
		f.tool[name] = s
	}
}

// toolByName returns the agent.Tool registered under name.
func (f *fakeRegistrar) toolByName(name string) (agent.Tool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.impls[name]
	return t, ok
}

// sourceOf derives the grouping source from a namespaced tool name.
func sourceOf(name string) string {
	for j := range len(name) - 1 {
		if name[j] == '_' && name[j+1] == '_' {
			return "mcp: " + name[:j]
		}
	}
	return ""
}

// wired fills the notice hook production wiring always supplies. A test
// overrides only what it exercises on the returned struct.
func wired(base Options) Options {
	if base.Notice == nil {
		base.Notice = func(string, bool) {}
	}
	return base
}

// fastRetry is the reconnect-backoff seam for tests: retries every 25ms, so a
// parked loop wakes inside a short window and a rogue loop surfaces fast.
func fastRetry(int) time.Duration { return 25 * time.Millisecond }

func TestLoadOnFirstMessage(t *testing.T) {
	t.Parallel()

	srv := buildFakeServer(t)

	// a first-message load connects every server and registers all of its tools as enabled
	t.Run("connects", func(t *testing.T) {
		fr := newFakeRegistrar()
		mgr := New(map[string]ServerConfig{
			"fake": {Command: srv},
		}, wired(Options{Registrar: fr}))

		// nothing is registered before the first message, no process spawned yet
		assert.Empty(t, fr.AllNames("mcp: fake"))
		require.Nil(t, mgr.serverByName("fake").client())
		t.Cleanup(mgr.Close)

		mgr.LoadOnFirstMessage(t.Context())

		for _, n := range []string{"tool_00", "tool_01", "tool_02"} {
			st, ok := fr.state("fake__" + n)
			assert.True(t, ok)
			assert.Equal(t, StateEnabled, st) // every tool exposed in full
		}
		// no _search/_load synthetics exist anymore
		_, hasSearch := fr.toolByName("fake_search")
		_, hasLoad := fr.toolByName("fake_load")
		assert.False(t, hasSearch)
		assert.False(t, hasLoad)
	})

	// a second load is a no-op: the server stays connected and its tools are not re-registered
	t.Run("runs_once", func(t *testing.T) {
		fr := newFakeRegistrar()
		mgr := New(map[string]ServerConfig{
			"fake": {Command: srv},
		}, wired(Options{Registrar: fr}))
		t.Cleanup(mgr.Close)

		mgr.LoadOnFirstMessage(t.Context())

		// disconnect is a manual act, so LoadOnFirstMessage must not reconnect it again
		mgr.Disconnect("fake")
		before := len(fr.AllNames("mcp: fake"))
		mgr.LoadOnFirstMessage(t.Context())
		assert.Nil(t, mgr.serverByName("fake").client())
		// no re-registration either
		assert.Len(t, fr.AllNames("mcp: fake"), before)
	})
}

func TestPreload(t *testing.T) {
	t.Parallel()

	// Preload connects and registers every server in the background, so LoadOnFirstMessage
	// then finds them already present instead of dialing again.
	t.Run("registers_in_background", func(t *testing.T) {
		srv := buildFakeServer(t)
		fr := newFakeRegistrar()
		mgr := New(map[string]ServerConfig{
			"fake": {Command: srv, Args: []string{"-startup-delay=200ms"}},
		}, wired(Options{Registrar: fr}))
		t.Cleanup(mgr.Close)

		mgr.Preload() // non-blocking
		require.Eventually(t, func() bool {
			return len(fr.AllNames("mcp: fake")) == 3
		}, 5*time.Second, 20*time.Millisecond)
	})

	// a second Preload is a no-op and LoadOnFirstMessage waits on the same dials.
	t.Run("idempotent", func(t *testing.T) {
		srv := buildFakeServer(t)
		fr := newFakeRegistrar()
		mgr := New(map[string]ServerConfig{
			"fake": {Command: srv, Args: []string{"-startup-delay=200ms"}},
		}, wired(Options{Registrar: fr}))
		t.Cleanup(mgr.Close)

		mgr.Preload()
		mgr.Preload()                       // no-op, single dial in flight
		mgr.LoadOnFirstMessage(t.Context()) // waits for the background preload to settle
		assert.Len(t, fr.AllNames("mcp: fake"), 3)
	})
}

func TestConfigDisabledServer(t *testing.T) {
	t.Parallel()

	srv := buildFakeServer(t)

	// a config-disabled server still connects so its tools appear in /tools, but
	// registers each tool as StateDisabled: known and toggleable, never callable by default.
	t.Run("loads_but_stays_inactive", func(t *testing.T) {
		fr := newFakeRegistrar()
		var disabled bool
		mgr := New(map[string]ServerConfig{
			"fake": {Command: srv, Enabled: &disabled},
		}, wired(Options{Registrar: fr}))

		t.Cleanup(mgr.Close)
		mgr.LoadOnFirstMessage(t.Context())

		srv := mgr.serverByName("fake")
		require.NotNil(t, srv.client())
		for _, n := range []string{"tool_00", "tool_01", "tool_02"} {
			st, ok := fr.state("fake__" + n)
			assert.True(t, ok)
			assert.Equal(t, StateDisabled, st) // visible in /tools but unchecked
		}
	})

	// an explicit session enablement survives resume: a tool the user turned on via
	// /tools (persisted to tools.enabled and fed back as Restore) must come back enabled even when its
	// server is config-disabled. The config flag is only a default, never a veto.
	t.Run("honours_restored_enablement", func(t *testing.T) {
		fr := newFakeRegistrar()
		var disabled bool
		mgr := New(nil, wired(Options{
			Registrar: fr,
			Restore:   []string{"fake__tool_01"}, // enabled via /tools in the prior session
		}))
		s := mgr.newServer("fake", ServerConfig{Enabled: &disabled})

		defs := []ToolDef{
			{Name: "tool_00", InputSchema: jsonRawObject},
			{Name: "tool_01", InputSchema: jsonRawObject},
		}
		mgr.register(s, nil, defs, nil)

		st, ok := fr.state("fake__tool_00")
		require.True(t, ok)
		assert.Equal(t, StateDisabled, st) // never enabled: config-off default holds

		st, ok = fr.state("fake__tool_01")
		require.True(t, ok)
		assert.Equal(t, StateEnabled, st) // explicit /tools enablement wins over the default
	})
}

func TestClaimConnectCoalesces(t *testing.T) {
	t.Parallel()

	var s server // zero value: mutex ready, no client yet
	win, done := s.claimConnect()
	require.True(t, win)
	assert.Nil(t, done)

	// a second claim while the slot is held coalesces onto the same channel
	lose, wait := s.claimConnect()
	assert.False(t, lose)
	require.NotNil(t, wait)

	// releasing hands every waiter the settled error and frees a fresh slot
	s.finishConnect(errors.New("boom"))
	select {
	case <-wait:
		s.mu.Lock()
		err := s.connectErr
		s.mu.Unlock()
		require.EqualError(t, err, "boom")
	default:
		t.Fatal("slot never released for waiters")
	}

	win2, _ := s.claimConnect()
	assert.True(t, win2) // a new attempt may start after the previous one settled
	s.finishConnect(nil)
}

func TestDialAbortsWhenServerRemoved(t *testing.T) {
	t.Parallel()

	fr := newFakeRegistrar()
	mgr := New(map[string]ServerConfig{
		"fake": {Command: buildFakeServer(t), Args: []string{"-startup-delay=200ms"}},
	}, wired(Options{Registrar: fr}))
	t.Cleanup(mgr.Close)

	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Connect(t.Context(), "fake") }()

	s := mgr.serverByName("fake")
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.connecting // the dial holds the slot while startup delay blocks it
	}, time.Second, 10*time.Millisecond)

	// remove the server from the map mid-dial, dial must not install its client
	mgr.mu.Lock()
	delete(mgr.servers, "fake")
	mgr.mu.Unlock()

	require.ErrorContains(t, <-errCh, "server removed during connect")
}

func TestDisconnectDuringDial(t *testing.T) {
	t.Parallel()

	fr := newFakeRegistrar()
	mgr := New(map[string]ServerConfig{
		"fake": {Command: buildFakeServer(t), Args: []string{"-startup-delay=200ms"}},
	}, wired(Options{Registrar: fr}))
	t.Cleanup(mgr.Close)
	// fast retries: a stealth loop would surface within the short Never window
	mgr.backoff = fastRetry

	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Connect(t.Context(), "fake") }()

	s := mgr.serverByName("fake")
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.connecting
	}, 2*time.Second, 5*time.Millisecond)

	// the explicit disconnect lands while the dial is still mid-handshake
	mgr.Disconnect("fake")

	select {
	case err := <-errCh:
		require.ErrorContains(t, err, "disconnected during connect")
	case <-time.After(10 * time.Second):
		t.Fatal("dial never settled after disconnect")
	}

	// the disconnect must survive: no client, no tools, no stealth reconnect loop
	s.mu.Lock()
	down, connecting := s.down, s.connecting
	s.mu.Unlock()
	assert.False(t, down)
	assert.False(t, connecting)
	assert.Empty(t, fr.AllNames("mcp: fake"))
	require.Never(t, func() bool {
		return s.client() != nil || len(fr.AllNames("mcp: fake")) > 0
	}, 400*time.Millisecond, 10*time.Millisecond)

	// a later manual connect works: the mid-dial disconnect poisons nothing
	require.NoError(t, mgr.Connect(t.Context(), "fake"))
	assert.NotEmpty(t, fr.AllNames("mcp: fake"))
}

func TestConnectAfterMidDialDisconnect(t *testing.T) {
	t.Parallel()

	fr := newFakeRegistrar()
	mgr := New(map[string]ServerConfig{
		"fake": {Command: buildFakeServer(t), Args: []string{"-startup-delay=200ms"}},
	}, wired(Options{Registrar: fr}))
	t.Cleanup(mgr.Close)
	mgr.backoff = fastRetry

	first := make(chan error, 1)
	go func() { first <- mgr.Connect(t.Context(), "fake") }()
	s := mgr.serverByName("fake")
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.connecting
	}, 2*time.Second, 5*time.Millisecond)
	mgr.Disconnect("fake")

	// a connect requested after that disconnect coalesces onto the doomed dial,
	// it must not inherit the older request's preemption
	second := make(chan error, 1)
	go func() { second <- mgr.Connect(t.Context(), "fake") }()

	select {
	case err := <-first:
		require.ErrorContains(t, err, "disconnected during connect")
	case <-time.After(10 * time.Second):
		t.Fatal("first dial never settled")
	}
	select {
	case err := <-second:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("second connect never settled")
	}
	assert.NotNil(t, s.client())
	assert.NotEmpty(t, fr.AllNames("mcp: fake"))
}

func TestFailedDialReconnects(t *testing.T) {
	t.Parallel()

	// a stdio server that is offline or not yet installed comes back on its own:
	// the failed dial starts the same capped-backoff loop a death would
	t.Run("stdio_starts_reconnect_loop", func(t *testing.T) {
		srv := buildFakeServer(t) // built up front so the loop finds it on the first retry
		missing := filepath.Join(t.TempDir(), "not-yet")
		fr := newFakeRegistrar()
		mgr := New(map[string]ServerConfig{
			"fake": {Command: missing},
		}, wired(Options{Registrar: fr}))
		t.Cleanup(mgr.Close)
		mgr.backoff = fastRetry // retries land right after the symlink, not at 1s

		require.Error(t, mgr.Connect(t.Context(), "fake"))

		s := mgr.serverByName("fake")
		s.mu.Lock()
		down, failures := s.down, s.failures
		s.mu.Unlock()
		assert.True(t, down)
		assert.Equal(t, 1, failures)

		// /mcp reports the retry rather than a dead-looking server
		rows := mgr.Status(t.Context())
		require.Len(t, rows, 1)
		assert.Equal(t, fmt.Sprintf("reconnecting (%d)", failures), rows[0].State)

		// the server appearing on disk is enough: the loop brings it up and registers
		require.NoError(t, os.Symlink(srv, missing))
		require.Eventually(t, func() bool {
			return len(fr.AllNames("mcp: fake")) > 0
		}, 5*time.Second, 20*time.Millisecond)
		assert.NotNil(t, s.client())
	})

	// network servers have no death supervision, a failed dial must not invent it
	t.Run("network_never_supervised", func(t *testing.T) {
		// a port that just closed: connects fail fast without leaving the manager
		var lc net.ListenConfig
		ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := ln.Addr().(*net.TCPAddr).Port
		require.NoError(t, ln.Close())

		fr := newFakeRegistrar()
		mgr := New(map[string]ServerConfig{
			"net": {URL: fmt.Sprintf("http://127.0.0.1:%d/mcp", port)},
		}, wired(Options{Registrar: fr}))
		t.Cleanup(mgr.Close)

		require.Error(t, mgr.Connect(t.Context(), "net"))

		s := mgr.serverByName("net")
		s.mu.Lock()
		down := s.down
		s.mu.Unlock()
		assert.False(t, down)
	})

	// a disconnect must survive a dial failure pinned to the pre-disconnect
	// snapshot: the stale check and the down flag are one critical section, so
	// retryDial can never flip a just-disconnected server back to down
	t.Run("disconnect_survives_stale_dial_failure", func(t *testing.T) {
		srv := buildFakeServer(t)
		missing := filepath.Join(t.TempDir(), "not-yet")
		fr := newFakeRegistrar()
		mgr := New(map[string]ServerConfig{
			"fake": {Command: missing},
		}, wired(Options{Registrar: fr}))
		t.Cleanup(mgr.Close)

		require.Error(t, mgr.Connect(t.Context(), "fake")) // starts the loop
		s := mgr.serverByName("fake")
		s.mu.Lock()
		down := s.down
		s.mu.Unlock()
		require.True(t, down)

		mgr.Disconnect("fake") // stops the loop and bumps discSeq

		// a dial failure arriving with the pre-disconnect snapshot must be refused
		// even once the server is back on disk
		mgr.retryDial("fake", s, 0)
		require.NoError(t, os.Symlink(srv, missing))

		s.mu.Lock()
		down = s.down
		s.mu.Unlock()
		assert.False(t, down)
		require.Never(t, func() bool {
			return s.client() != nil || len(fr.AllNames("mcp: fake")) > 0
		}, 400*time.Millisecond, 10*time.Millisecond)
	})

	// a superseded loop generation must exit on wake without dialing: a disconnect
	// clears down underneath a loop parked in backoff, so a failed dial after it
	// spawns a second loop and only the newest generation may keep retrying
	t.Run("superseded_loop_exits", func(t *testing.T) {
		fr := newFakeRegistrar()
		mgr := New(map[string]ServerConfig{
			"fake": {Command: "unused"}, // the dial seam below fails first
		}, wired(Options{Registrar: fr}))
		t.Cleanup(mgr.Close)

		var dials atomic.Int32
		mgr.connectClient = func(context.Context, string, ServerConfig) (*Client, error) {
			dials.Add(1)
			return nil, errors.New("unreachable")
		}
		mgr.backoff = fastRetry // the parked loop wakes within the window, not at 1s
		s := mgr.serverByName("fake")
		s.mu.Lock()
		s.down = true
		s.loopSeq = 1
		s.mu.Unlock()
		go mgr.reconnectLoop(s, 1)

		s.mu.Lock() // a newer loop claims the server before the parked one wakes
		s.loopSeq = 2
		s.mu.Unlock()
		// ~16 wakes pass under fastRetry: a superseded loop dialing once fails this
		require.Never(t, func() bool { return dials.Load() > 0 },
			400*time.Millisecond, 10*time.Millisecond)
	})

	// the full interleaving that once stacked loops: disconnect lands between two
	// failed dials, parking the first loop while down is clear, and the second
	// failure spawns a replacement. Only one backoff chain may dial from there.
	t.Run("disconnect_between_failed_dials_keeps_one_loop", func(t *testing.T) {
		fr := newFakeRegistrar()
		mgr := New(map[string]ServerConfig{
			"fake": {Command: "unused"},
		}, wired(Options{Registrar: fr}))
		t.Cleanup(mgr.Close)

		var dials atomic.Int32
		mgr.connectClient = func(context.Context, string, ServerConfig) (*Client, error) {
			dials.Add(1)
			return nil, errors.New("unreachable")
		}
		mgr.backoff = fastRetry

		require.Error(t, mgr.Connect(t.Context(), "fake")) // starts the first loop
		mgr.Disconnect("fake")                             // parks it with down cleared
		require.Error(t, mgr.Connect(t.Context(), "fake")) // spawns the replacement

		// one chain retries every fastRetry; over the window that budget is ~2 user
		// dials plus 400ms/25ms wakes, and a stacked second chain doubles past it
		require.Never(t, func() bool { return dials.Load() > 2+400/25+2 },
			400*time.Millisecond, 10*time.Millisecond)
	})
}

func TestConcurrentConnectsShareOneClient(t *testing.T) {
	t.Parallel()

	fr := newFakeRegistrar()
	mgr := New(map[string]ServerConfig{
		"fake": {Command: buildFakeServer(t), Args: []string{"-startup-delay=200ms"}},
	}, wired(Options{Registrar: fr}))
	t.Cleanup(mgr.Close)

	// a gate releases every goroutine together so they all arrive while the first
	// dial is still held open by the server's startup delay.
	start := make(chan struct{})
	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; errs[i] = mgr.Connect(t.Context(), "fake") }()
	}
	close(start)
	wg.Wait()

	for _, e := range errs {
		require.NoError(t, e)
	}
	c := mgr.serverByName("fake").client()
	require.NotNil(t, c) // exactly one client installed and serving
}

func TestBadSchemaNeverRegistered(t *testing.T) {
	t.Parallel()

	srv := buildFakeServer(t)
	fr := newFakeRegistrar()
	mgr := New(map[string]ServerConfig{
		"fake": {Command: srv, Args: []string{"-bad-schema"}},
	}, wired(Options{Registrar: fr}))
	t.Cleanup(mgr.Close)

	mgr.LoadOnFirstMessage(t.Context())

	names := fr.AllNames("mcp: fake")
	assert.Contains(t, names, "fake__tool_00") // the server still yields a usable session
	assert.NotContains(t, names, "fake__bad_schema")

	// regression: every schema that reaches the registry is sound, so nothing
	// malformed can flow from Registry.Schemas() into a provider request body
	for _, n := range names {
		tool, ok := fr.toolByName(n)
		require.True(t, ok)
		assert.Empty(t, schemaDefect(tool.Schema().Parameters))
	}
}

func TestRepeatedBadSchemaStaysQuiet(t *testing.T) {
	t.Parallel()

	srv := buildFakeServer(t)
	fr := newFakeRegistrar()
	var notices []string
	mgr := New(map[string]ServerConfig{
		"fake": {Command: srv, Args: []string{"-bad-schema"}},
	}, wired(Options{
		Registrar: fr,
		Notice:    func(msg string, warn bool) { notices = append(notices, msg) },
	}))
	t.Cleanup(mgr.Close)

	mgr.LoadOnFirstMessage(t.Context())
	require.Len(t, notices, 1) // the first discovery warns once, naming the tool
	assert.Contains(t, notices[0], `tool "bad_schema"`)
	assert.Equal(t, 1, strings.Count(notices[0], "mcp fake:")) // the prefix lands exactly once

	// re-discovery of the same defect (reconnect here, list_changed in the wild)
	// logs the repeat but never re-notifies history
	mgr.Disconnect("fake")
	require.NoError(t, mgr.Connect(t.Context(), "fake"))
	assert.Len(t, notices, 1)
	warns := bulk.SliceFilter(func(line string) bool {
		return strings.Contains(line, `tool "bad_schema"`)
	}, mgr.serverByName("fake").logs.lines())
	assert.Len(t, warns, 2) // both passes recorded in /mcp logs

	// a config edit resets the dedupe, so a fix-then-rebreak cycle re-notifies.
	// Re-applying an unchanged declaration (parsed fresh, as Reload does) keeps it.
	mgr.Disconnect("fake")
	s := mgr.serverByName("fake")
	var fresh map[string]ServerConfig
	b, err := json.Marshal(map[string]ServerConfig{"fake": s.config()})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &fresh)) // new pointers, same content
	mgr.applyConfig(t.Context(), s, fresh["fake"])
	require.NoError(t, mgr.Connect(t.Context(), "fake"))
	assert.Len(t, notices, 1)

	mgr.Disconnect("fake")
	edited := s.config()
	edited.Timeout = FlexDuration(90 * time.Second) // inert for the connection, still an edit
	mgr.applyConfig(t.Context(), s, edited)
	require.NoError(t, mgr.Connect(t.Context(), "fake"))
	assert.Len(t, notices, 2) // fresh defect state, fresh notice
}

func TestRegisterMarksReadOnlyTools(t *testing.T) {
	t.Parallel()

	fr := newFakeRegistrar()
	mgr := New(nil, wired(Options{Registrar: fr}))
	s := mgr.newTestServer("srv")

	defs := []ToolDef{
		{Name: "read1", InputSchema: jsonRawObject, ReadOnly: true},
		{Name: "write1", InputSchema: jsonRawObject},
	}
	mgr.register(s, nil, defs, nil) // all enabled

	assert.Contains(t, fr.readonly(), "srv__read1")
	assert.NotContains(t, fr.readonly(), "srv__write1") // not annotated read-only
}

func TestRegisterPreservesLiveDisabled(t *testing.T) {
	t.Parallel()

	fr := newFakeRegistrar()
	mgr := New(nil, wired(Options{Registrar: fr}))
	s := mgr.newTestServer("srv")
	defs := []ToolDef{
		{Name: "a", InputSchema: jsonRawObject},
		{Name: "b", InputSchema: jsonRawObject},
	}

	// first registration exposes everything, the user then turns b off via /tools
	mgr.register(s, nil, defs, nil)
	assert.Equal(t, StateEnabled, mustState(fr, "srv__a"))
	fr.set("srv__b", StateDisabled)

	// capture the live split and re-register as a refresh would (unregister first)
	keep := mgr.captureLive(s.source, ServerConfig{})
	fr.Unregister(s.source)
	mgr.register(s, nil, defs, keep)

	assert.Equal(t, StateEnabled, mustState(fr, "srv__a"))  // live enablement kept
	assert.Equal(t, StateDisabled, mustState(fr, "srv__b")) // explicit off survives re-registration
}

func TestRegisterLiveDisabledBeatsRestore(t *testing.T) {
	t.Parallel()

	fr := newFakeRegistrar()
	mgr := New(nil, wired(Options{Registrar: fr, Restore: []string{"srv__a", "srv__b"}}))
	s := mgr.newTestServer("srv")
	defs := []ToolDef{
		{Name: "a", InputSchema: jsonRawObject},
		{Name: "b", InputSchema: jsonRawObject},
	}

	// a prior registration enabled both, then the user disabled b, and Restore still names it
	mgr.register(s, nil, defs, &toolState{
		enabled:  map[string]struct{}{"srv__a": {}},
		disabled: map[string]struct{}{"srv__b": {}},
	})

	assert.Equal(t, StateEnabled, mustState(fr, "srv__a"))
	assert.Equal(t, StateDisabled, mustState(fr, "srv__b")) // live off wins over Restore
}

func TestImpliedEnabled(t *testing.T) {
	t.Parallel()

	var off bool
	tests := []struct {
		name    string
		restore []string
		cfg     ServerConfig
		tool    string
		want    bool
	}{
		{name: "config enabled default", cfg: ServerConfig{}, tool: "srv__a", want: true},
		{name: "config disabled default", cfg: ServerConfig{Enabled: &off}, tool: "srv__a", want: false},
		{name: "restore member wins", restore: []string{"srv__a"}, cfg: ServerConfig{}, tool: "srv__a", want: true},
		{name: "restore non-member off", restore: []string{"srv__b"}, cfg: ServerConfig{}, tool: "srv__a", want: false},
		{name: "restore beats config off", restore: []string{"srv__a"}, cfg: ServerConfig{Enabled: &off}, tool: "srv__a", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := New(nil, wired(Options{Registrar: newFakeRegistrar(), Restore: tt.restore}))
			assert.Equal(t, tt.want, mgr.impliedEnabled(tt.cfg, tt.tool))
		})
	}
}

func TestCaptureKeep(t *testing.T) {
	t.Parallel()

	fr := newFakeRegistrar()
	mgr := New(nil, wired(Options{Registrar: fr}))
	s := mgr.newTestServer("srv")
	defs := []ToolDef{
		{Name: "a", InputSchema: jsonRawObject},
		{Name: "b", InputSchema: jsonRawObject},
	}

	// era A registers both enabled, then b is turned off via /tools
	mgr.register(s, nil, defs, nil)
	fr.set("srv__b", StateDisabled)

	// the config flips to disabled while the era-A registration still stands,
	// its re-registration not yet landed
	var off bool
	mgr.applyConfig(t.Context(), s, ServerConfig{Enabled: &off})

	keep := mgr.captureKeep(s)
	assert.Empty(t, keep.enabled) // a sits at its era default, never classified as a toggle
	assert.Equal(t, map[string]struct{}{"srv__b": {}}, keep.disabled)
}

func TestCaptureLiveSubtractsDefaults(t *testing.T) {
	t.Parallel()

	fr := newFakeRegistrar()
	mgr := New(nil, wired(Options{Registrar: fr}))
	s := mgr.newTestServer("srv")
	defs := []ToolDef{
		{Name: "a", InputSchema: jsonRawObject},
		{Name: "b", InputSchema: jsonRawObject},
	}

	// both register enabled under a config-enabled default, then b is toggled off
	mgr.register(s, nil, defs, nil)
	fr.set("srv__b", StateDisabled)

	// against the config that produced this split only the deliberate toggle remains,
	// so an unchanged re-registration keeps it while anything untouched follows defaults
	keep := mgr.captureLive(s.source, ServerConfig{})
	assert.Empty(t, keep.enabled)
	assert.Equal(t, map[string]struct{}{"srv__b": {}}, keep.disabled)

	// against a config-disabled era the same split reads as pure default: b was never
	// a toggle and a must have been turned on deliberately
	var off bool
	keep = mgr.captureLive(s.source, ServerConfig{Enabled: &off})
	assert.Equal(t, map[string]struct{}{"srv__a": {}}, keep.enabled)
	assert.Empty(t, keep.disabled)

	// a restored enabled set makes the implication per name: only b's extra enablement departs
	fr.set("srv__b", StateEnabled)
	restored := New(nil, wired(Options{Registrar: fr, Restore: []string{"srv__a"}}))
	keep = restored.captureLive(s.source, ServerConfig{})
	assert.Empty(t, keep.disabled)
	assert.Equal(t, map[string]struct{}{"srv__b": {}}, keep.enabled)
}

func mustState(fr *fakeRegistrar, name string) State {
	st, ok := fr.state(name)
	if !ok {
		panic("unregistered tool: " + name)
	}
	return st
}

func TestManagerRediscoverAfterListChanged(t *testing.T) {
	fr := newFakeRegistrar()
	mgr := New(map[string]ServerConfig{
		"fake": stdioConfig(t, "-notify-list-changed"),
	}, wired(Options{Registrar: fr}))

	require.NoError(t, mgr.Connect(t.Context(), "fake"))
	t.Cleanup(mgr.Close)
	_, ok := fr.toolByName("fake__tool_00")
	require.True(t, ok) // connected and registered before the notification fires

	// trigger_listchanged responds AND emits list_changed, so rediscovery must complete.
	s := mgr.serverByName("fake")
	rc := s.client()
	require.NotNil(t, rc)
	res, err := rc.Call(t.Context(), "trigger_listchanged", json.RawMessage(`{}`), nil)
	require.NoError(t, err)
	assert.False(t, res.IsError)

	// the async rediscovery re-registers within a short window, and wait until it has fully
	// settled (no pass in flight) so Close below cannot race its Unregister+Register.
	require.Eventually(t, func() bool {
		s.mu.Lock()
		busy := s.rediscovering
		s.mu.Unlock()
		_, ok := fr.toolByName("fake__tool_00")
		return !busy && ok
	}, 5*time.Second, 20*time.Millisecond, "rediscovery deadlocked stdio or dropped registration")

	// a follow-up call still works after notification handling
	s = mgr.serverByName("fake")
	rc = s.client()
	require.NotNil(t, rc)
	res2, err := rc.Call(t.Context(), "tool_00", json.RawMessage(`{}`), nil)
	require.NoError(t, err)
	assert.False(t, res2.IsError)
}

// heldRegistrar stalls RegisterState calls made while armed, so a test can land
// a disconnect between a scan's live check and its registration.
type heldRegistrar struct {
	*fakeRegistrar
	hold    atomic.Bool
	started chan struct{} // receives one token when the first armed call parks
	release chan struct{}
}

func (h *heldRegistrar) RegisterState(source string, t agent.Tool, s State) {
	if h.hold.Load() {
		select {
		case h.started <- struct{}{}:
		default:
		}
		<-h.release // closed channels read freely, so later calls pass through
	}
	h.fakeRegistrar.RegisterState(source, t, s)
}

func TestRediscanRevertAfterDisconnect(t *testing.T) {
	t.Parallel()

	srv := buildFakeServer(t)
	fr := &heldRegistrar{
		fakeRegistrar: newFakeRegistrar(),
		started:       make(chan struct{}, 1),
		release:       make(chan struct{}),
	}
	mgr := New(map[string]ServerConfig{
		"fake": {Command: srv},
	}, wired(Options{Registrar: fr}))
	t.Cleanup(mgr.Close)

	require.NoError(t, mgr.Connect(t.Context(), "fake"))
	s := mgr.serverByName("fake")
	require.Len(t, fr.AllNames("mcp: fake"), 3)

	// park the pass inside its first RegisterState, after its live check passed
	fr.hold.Store(true)
	mgr.rediscan(t.Context(), s, nil)
	select {
	case <-fr.started:
	case <-time.After(5 * time.Second):
		t.Fatal("scan never reached registration")
	}

	// the disconnect lands while the pass sits between its check and register,
	// so the fresh registration must be compensated once it completes
	mgr.Disconnect("fake")
	close(fr.release)

	require.Eventually(t, func() bool { // the pass settles before its revert can run
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.rediscovering
	}, 5*time.Second, 10*time.Millisecond)
	assert.Empty(t, fr.AllNames("mcp: fake")) // nothing bound to the closed client survives
	require.Never(t, func() bool {
		return len(fr.AllNames("mcp: fake")) > 0
	}, 300*time.Millisecond, 10*time.Millisecond)
}

func TestDisconnectDuringRediscan(t *testing.T) {
	t.Parallel()

	srv := buildFakeServer(t)
	var mu sync.Mutex
	var notices []string
	fr := newFakeRegistrar()
	mgr := New(map[string]ServerConfig{
		"fake": {Command: srv},
	}, wired(Options{
		Registrar: fr,
		Notice:    func(msg string, warn bool) { mu.Lock(); defer mu.Unlock(); notices = append(notices, msg) },
	}))
	t.Cleanup(mgr.Close)

	require.NoError(t, mgr.Connect(t.Context(), "fake"))
	s := mgr.serverByName("fake")

	// the disconnect lands while the pass is listing (or just after): either way
	// it wins, and its fallout never reaches history as a warning
	mgr.rediscan(t.Context(), s, nil)
	mgr.Disconnect("fake")

	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.rediscovering
	}, 5*time.Second, 10*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	// holds under every interleaving: a failed list is self-inflicted fallout
	// (see TestReportScanError), and nothing the pass registered survives
	assert.Empty(t, notices)
	assert.Empty(t, fr.AllNames("mcp: fake"))
	require.Never(t, func() bool {
		return len(fr.AllNames("mcp: fake")) > 0
	}, 300*time.Millisecond, 10*time.Millisecond)
}

func TestInstallScanStaleClient(t *testing.T) {
	t.Parallel()

	srv := buildFakeServer(t)
	fr := newFakeRegistrar()
	mgr := New(map[string]ServerConfig{
		"fake": {Command: srv},
	}, wired(Options{Registrar: fr}))
	t.Cleanup(mgr.Close)

	require.NoError(t, mgr.Connect(t.Context(), "fake"))
	s := mgr.serverByName("fake")
	stale := s.client()
	defs := s.defsSnapshot()

	// disconnect then reconnect: a successor owns the registration now
	mgr.Disconnect("fake")
	require.NoError(t, mgr.Connect(t.Context(), "fake"))
	names := fr.AllNames("mcp: fake")
	before, ok := fr.toolByName("fake__tool_00")
	require.True(t, ok)

	// a pass bound to the replaced client must not touch the successor's tools
	mgr.installScan(s, stale, defs, nil)

	after, ok := fr.toolByName("fake__tool_00")
	require.True(t, ok)
	assert.Same(t, before, after) // no re-registration happened at all
	assert.Equal(t, names, fr.AllNames("mcp: fake"))
}

func TestReportScanError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		disconnected bool // a disconnect landed since the pass captured its snapshot
		canceled     bool // the caller's context is already done
		wantWarn     bool // whether history sees a warning
	}{
		{name: "real failure warns", wantWarn: true},
		{name: "disconnect stays quiet", disconnected: true},
		{name: "canceled scan stays quiet", canceled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var notices []string
			mgr := New(map[string]ServerConfig{"fake": {}}, wired(Options{
				Registrar: newFakeRegistrar(),
				Notice:    func(msg string, warn bool) { notices = append(notices, msg) },
			}))
			s := mgr.serverByName("fake")

			disc := s.discSnapshot()
			if tt.disconnected {
				s.mu.Lock()
				s.discSeq++ // the disconnect lands after the snapshot in real flow
				s.mu.Unlock()
			}
			var cancel context.CancelFunc
			ctx := t.Context()
			if tt.canceled {
				ctx, cancel = context.WithCancel(ctx)
				cancel() // the caller gave up before the pass reported
			}
			s.reportScanError(errors.New("tools/list: file already closed"), disc, ctx)

			if tt.wantWarn {
				require.Len(t, notices, 1)
				assert.Contains(t, notices[0], "re-discover failed")
				return
			}
			assert.Empty(t, notices) // fallout never reaches history
			logs := strings.Join(mgr.Logs("fake"), "\n")
			assert.Contains(t, logs, "re-discover aborted")
		})
	}
}

func TestBridgeFailureLogs(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var notices []string
	fr := newFakeRegistrar()
	mgr := New(map[string]ServerConfig{
		"fake": {
			Command: buildFakeServer(t),
			Args:    []string{"-slow"}, // every call blocks until the client gives up
			Timeout: FlexDuration(50 * time.Millisecond),
		},
	}, wired(Options{
		Registrar: fr,
		Notice:    func(msg string, warn bool) { mu.Lock(); defer mu.Unlock(); notices = append(notices, msg) },
	}))
	t.Cleanup(mgr.Close)

	require.NoError(t, mgr.Connect(t.Context(), "fake")) // the status row still says connected
	tool, ok := fr.toolByName("fake__tool_00")
	require.True(t, ok)
	res, err := tool.Execute(t.Context(), agent.ToolCall{ID: "1", Name: tool.Name()}, agent.NewOutput(agent.NopSink{}, "c"))
	require.NoError(t, err) // a failed call is a result, never an abort
	assert.True(t, res.IsError)

	// the diag lands synchronously inside Execute, so the ring holds it on return
	logs := strings.Join(mgr.Logs("fake"), "\n")
	assert.Contains(t, logs, `call failed: call "tool_00"`)

	mu.Lock()
	defer mu.Unlock()
	assert.Empty(t, notices) // no history entry behind the log line
}

// jsonRawObject is a minimal valid tool schema.
var jsonRawObject = []byte(`{"type":"object","properties":{}}`)

func TestReload(t *testing.T) {
	// reload reads mcp.json, so each case owns a workspace and AJENT_HOME, no t.Parallel

	cmd := buildFakeServer(t)
	cfgJSON := func(extra string) string {
		return `{"servers":{"fake":{"command":` + strconv.Quote(cmd) + extra + `}}}`
	}
	setup := func(t *testing.T, initial string) (*Manager, *fakeRegistrar, string) {
		t.Helper()

		t.Setenv("AJENT_HOME", mkHome(t))
		ws := t.TempDir()
		mkFile(t, ws+"/.ajent/mcp.json", initial)
		servers, _, err := LoadConfig(ws)
		require.NoError(t, err)

		fr := newFakeRegistrar()
		mgr := New(servers, wired(Options{Registrar: fr, Workspace: ws}))
		t.Cleanup(mgr.Close)
		mgr.LoadOnFirstMessage(t.Context())
		return mgr, fr, ws
	}

	t.Run("filter_change_reregisters", func(t *testing.T) {
		mgr, fr, ws := setup(t, cfgJSON(""))
		require.NotEmpty(t, fr.AllNames("mcp: fake"))
		before := mgr.serverByName("fake").client()
		require.NotNil(t, before)

		mkFile(t, ws+"/.ajent/mcp.json", cfgJSON(`,"excludeTools":["tool_01"]`))
		require.NoError(t, mgr.Reload(t.Context()))

		require.Eventually(t, func() bool { // rediscan re-registers asynchronously
			return !slices.Contains(fr.AllNames("mcp: fake"), "fake__tool_01")
		}, 5*time.Second, 20*time.Millisecond)
		assert.Contains(t, fr.AllNames("mcp: fake"), "fake__tool_00")
		assert.Same(t, before, mgr.serverByName("fake").client()) // same process, no restart
	})

	t.Run("connection_change_notices", func(t *testing.T) {
		mgr, _, ws := setup(t, cfgJSON(""))
		before := mgr.serverByName("fake").client()
		require.NotNil(t, before)

		mkFile(t, ws+"/.ajent/mcp.json", cfgJSON(`,"args":["-tools","1"]`))
		require.NoError(t, mgr.Reload(t.Context()))

		assert.Same(t, before, mgr.serverByName("fake").client()) // left running on purpose
		assert.Contains(t, strings.Join(mgr.Logs("fake"), "\n"), "connection config changed")
	})

	t.Run("filter_applies_despite_connection_change", func(t *testing.T) {
		mgr, fr, ws := setup(t, cfgJSON(""))
		before := mgr.serverByName("fake").client()
		require.NotNil(t, before)

		// both halves change at once: the filter must not be held hostage by the transport
		mkFile(t, ws+"/.ajent/mcp.json", cfgJSON(`,"args":["-tools","3"],"excludeTools":["tool_01"]`))
		require.NoError(t, mgr.Reload(t.Context()))

		require.Eventually(t, func() bool {
			return !slices.Contains(fr.AllNames("mcp: fake"), "fake__tool_01")
		}, 5*time.Second, 20*time.Millisecond)
		assert.Same(t, before, mgr.serverByName("fake").client())
		assert.Contains(t, strings.Join(mgr.Logs("fake"), "\n"), "connection config changed")
	})

	t.Run("removed_server_disconnects", func(t *testing.T) {
		mgr, fr, ws := setup(t, cfgJSON(""))
		require.NotEmpty(t, fr.AllNames("mcp: fake"))

		mkFile(t, ws+"/.ajent/mcp.json", `{"servers":{}}`)
		require.NoError(t, mgr.Reload(t.Context()))

		assert.Empty(t, fr.AllNames("mcp: fake")) // tools go with the server
		assert.Nil(t, mgr.serverByName("fake"))
	})

	// flipping enabled false -> true must reach a connected server: nothing was ever
	// toggled by hand, so the captured split holds no deliberate state to keep
	t.Run("enabled_flip_activates", func(t *testing.T) {
		mgr, fr, ws := setup(t, cfgJSON(`,"enabled":false`))
		require.NotEmpty(t, fr.AllNames("mcp: fake"))
		for _, n := range []string{"tool_00", "tool_01", "tool_02"} {
			assert.Equal(t, StateDisabled, mustState(fr, "fake__"+n))
		}

		mkFile(t, ws+"/.ajent/mcp.json", cfgJSON(`,"enabled":true`))
		require.NoError(t, mgr.Reload(t.Context()))

		require.Eventually(t, func() bool {
			for _, n := range []string{"tool_00", "tool_01", "tool_02"} {
				st, ok := fr.state("fake__" + n)
				if !ok || st != StateEnabled {
					return false
				}
			}
			return true
		}, 5*time.Second, 20*time.Millisecond)
	})

	// flipping enabled true -> false must not leave the just-disabled tools callable,
	// the security-relevant direction of the same capture path
	t.Run("enabled_flip_deactivates", func(t *testing.T) {
		mgr, fr, ws := setup(t, cfgJSON(""))
		require.NotEmpty(t, fr.AllNames("mcp: fake"))

		mkFile(t, ws+"/.ajent/mcp.json", cfgJSON(`,"enabled":false`))
		require.NoError(t, mgr.Reload(t.Context()))

		require.Eventually(t, func() bool {
			for _, n := range []string{"tool_00", "tool_01", "tool_02"} {
				st, ok := fr.state("fake__" + n)
				if !ok || st != StateDisabled {
					return false
				}
			}
			return true
		}, 5*time.Second, 20*time.Millisecond)
	})

	// a genuine /tools toggle must survive a refresh that keeps the same defaults:
	// only pre-reload default state is reconciled away, never user choices
	t.Run("user_toggle_survives_reload", func(t *testing.T) {
		mgr, fr, ws := setup(t, cfgJSON(""))
		require.NotEmpty(t, fr.AllNames("mcp: fake"))
		fr.set("fake__tool_01", StateDisabled) // the user turned it off via /tools
		before, ok := fr.toolByName("fake__tool_00")
		require.True(t, ok)

		mkFile(t, ws+"/.ajent/mcp.json", cfgJSON(`,"timeout":"90s"`)) // unrelated filter edit
		require.NoError(t, mgr.Reload(t.Context()))

		// the refresh re-registered (fresh tool objects) and both states held
		require.Eventually(t, func() bool {
			after, ok := fr.toolByName("fake__tool_00")
			off, okOff := fr.state("fake__tool_01")
			on, okOn := fr.state("fake__tool_02")
			return ok && after != before && okOff && off == StateDisabled &&
				okOn && on == StateEnabled
		}, 5*time.Second, 20*time.Millisecond)
	})

	// a flip landing while the server is down reaches its reconnect: the pre-death
	// split was captured as deliberate toggles only, so it cannot pin old defaults over
	t.Run("enabled_flip_reaches_dead_server", func(t *testing.T) {
		mgr, fr, ws := setup(t, cfgJSON(`,"args":["-die"]`))
		require.NotEmpty(t, fr.AllNames("mcp: fake"))

		tool, ok := fr.toolByName("fake__trigger_die")
		require.True(t, ok)
		_, err := tool.Execute(t.Context(), agent.ToolCall{ID: "die", Name: tool.Name()}, agent.NewOutput(agent.NopSink{}, "c"))
		require.NoError(t, err)             // a dead transport is a result, never an abort
		require.Eventually(t, func() bool { // death noticed and tools unregistered
			return len(fr.AllNames("mcp: fake")) == 0
		}, 5*time.Second, 20*time.Millisecond)

		mkFile(t, ws+"/.ajent/mcp.json", cfgJSON(`,"args":["-die"],"enabled":false`))
		require.NoError(t, mgr.Reload(t.Context()))

		require.Eventually(t, func() bool { // reconnected under the new default: all off
			for _, n := range []string{"tool_00", "tool_01", "tool_02"} {
				st, ok := fr.state("fake__" + n)
				if !ok || st != StateDisabled {
					return false
				}
			}
			return true
		}, 15*time.Second, 50*time.Millisecond)
	})
}

// blockingRegistrar stalls every Unregister, standing in for a server whose
// disconnect will not finish. It records each entry so a test can see how many
// disconnects were in flight at once.
type blockingRegistrar struct {
	*fakeRegistrar
	entered chan string
	release chan struct{}
}

func (b *blockingRegistrar) Unregister(source string) {
	b.entered <- source
	<-b.release
}

// pauseRegistrar stalls the first Unregister made while armed, parking a
// re-registration swap after its liveness check so a test can fire a capture
// inside that window.
type pauseRegistrar struct {
	*fakeRegistrar
	hold    atomic.Bool
	started chan struct{}
	release chan struct{}
}

func (p *pauseRegistrar) Unregister(source string) {
	if p.hold.Swap(false) { // later calls pass through
		p.started <- struct{}{}
		<-p.release
	}
	p.fakeRegistrar.Unregister(source)
}

func TestReconnectCaptureEra(t *testing.T) {
	t.Parallel()

	srv := buildFakeServer(t)
	pr := &pauseRegistrar{
		fakeRegistrar: newFakeRegistrar(),
		started:       make(chan struct{}, 1),
		release:       make(chan struct{}),
	}
	mgr := New(map[string]ServerConfig{
		"fake": {Command: srv},
	}, wired(Options{Registrar: pr}))
	t.Cleanup(mgr.Close)
	mgr.backoff = fastRetry

	require.NoError(t, mgr.Connect(t.Context(), "fake"))
	s := mgr.serverByName("fake")
	require.Len(t, pr.AllNames("mcp: fake"), 3) // era A: everything enabled by default

	// reload flips the server off, then parks its re-registration inside the swap:
	// the standing split is era A while the config is already era B
	var off bool
	pr.hold.Store(true)
	mgr.applyConfig(t.Context(), s, ServerConfig{Command: srv, Enabled: &off})
	select {
	case <-pr.started:
	case <-time.After(5 * time.Second):
		t.Fatal("reload re-registration never reached its swap")
	}

	// the child dies inside the window: the capture must classify the standing
	// split against era A, so nothing pins era-A defaults over era B
	mgr.reconnect(s, s.client())

	for _, n := range []string{"tool_00", "tool_01", "tool_02"} {
		assert.Equal(t, StateDisabled, mustState(pr.fakeRegistrar, "fake__"+n))
	}

	// the parked pass holds a client the reconnect closed, so its own revert
	// empties the source rather than leaving dead-client tools registered
	close(pr.release)
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.rediscovering
	}, 5*time.Second, 10*time.Millisecond)
	assert.Empty(t, pr.AllNames("mcp: fake"))
}

func TestReconnectAfterDeath(t *testing.T) {
	t.Parallel()

	fr := newFakeRegistrar()
	mgr := New(map[string]ServerConfig{
		"fake": {Command: buildFakeServer(t), Args: []string{"-die"}},
	}, wired(Options{Registrar: fr}))
	t.Cleanup(mgr.Close)
	mgr.backoff = fastRetry // the respawn is found on the first retry, not at 1s

	// disable one tool so the restored enabled set is a strict subset, not everything
	mgr.LoadOnFirstMessage(t.Context())
	die := mgr.serverByName("fake")
	require.NotNil(t, die.client())

	fr.set("fake__tool_01", StateDisabled) // the user turned it off via /tools

	// kill the child through its own trigger_die tool, and Execute returns an error
	// result (transport failure) rather than a Go error.
	tool, ok := fr.toolByName("fake__trigger_die")
	require.True(t, ok)
	_, err := tool.Execute(t.Context(), agent.ToolCall{ID: "die", Name: tool.Name()}, agent.NewOutput(agent.NopSink{}, "c"))
	require.NoError(t, err) // a dead transport is a result, never an abort

	// the server's tools drop out while it reconnects
	require.Eventually(t, func() bool {
		return len(fr.AllNames("mcp: fake")) == 0
	}, 5*time.Second, 20*time.Millisecond)

	// and come back once the child respawns: enablements restored, but the /tools
	// disablement survives too rather than reverting to enabled.
	require.Eventually(t, func() bool {
		for _, n := range []string{"tool_00", "tool_02"} {
			st, ok := fr.state("fake__" + n)
			if !ok || st != StateEnabled {
				return false
			}
		}
		st, ok := fr.state("fake__tool_01")
		return ok && st == StateDisabled // explicit off survives a reconnect
	}, 10*time.Second, 50*time.Millisecond)
	assert.NotNil(t, mgr.serverByName("fake").client())
}

func TestReconnectStaleWatcher(t *testing.T) {
	t.Parallel()

	srv := buildFakeServer(t)

	// /mcp disconnect followed by /mcp connect installs a fresh client inside the old
	// watcher's lag window, and that healthy connection must survive its late EOF.
	t.Run("successor_survives", func(t *testing.T) {
		var mu sync.Mutex
		var warns []string
		fr := newFakeRegistrar()
		mgr := New(map[string]ServerConfig{
			"fake": {Command: srv},
		}, wired(Options{Registrar: fr, Notice: func(msg string, warn bool) {
			if !warn {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			warns = append(warns, msg)
		}}))
		t.Cleanup(mgr.Close)

		require.NoError(t, mgr.Connect(t.Context(), "fake"))
		s := mgr.serverByName("fake")
		stale := s.client()
		mgr.Disconnect("fake")
		require.NoError(t, mgr.Connect(t.Context(), "fake")) // the successor
		live := s.client()
		names := fr.AllNames("mcp: fake")

		mgr.reconnect(s, stale) // a late exit notice for the replaced client

		assert.Same(t, live, s.client()) // the fresh connection is untouched
		assert.Equal(t, names, fr.AllNames("mcp: fake"))
		s.mu.Lock()
		down := s.down
		s.mu.Unlock()
		assert.False(t, down)
		mu.Lock()
		defer mu.Unlock()
		assert.Empty(t, warns) // no "server exited; reconnecting" either
	})

	// a stale notice after an explicit disconnect must not start a reconnect loop:
	// the user's disconnect is never reverted.
	t.Run("disconnected_stays_down", func(t *testing.T) {
		fr := newFakeRegistrar()
		mgr := New(map[string]ServerConfig{
			"fake": {Command: srv},
		}, wired(Options{Registrar: fr}))
		t.Cleanup(mgr.Close)
		mgr.backoff = fastRetry // a rogue loop would surface inside the window

		require.NoError(t, mgr.Connect(t.Context(), "fake"))
		s := mgr.serverByName("fake")
		stale := s.client()
		mgr.Disconnect("fake")

		mgr.reconnect(s, stale)

		assert.Nil(t, s.client())
		assert.Empty(t, fr.AllNames("mcp: fake"))
		require.Never(t, func() bool {
			return s.client() != nil || len(fr.AllNames("mcp: fake")) > 0
		}, 400*time.Millisecond, 10*time.Millisecond)
	})

	// the identity check only rejects foreign clients: a notice about the installed
	// client is still a real death. The call returns after its loop's first retry,
	// so assert the settled outcome rather than transient down state.
	t.Run("current_client_reconnects", func(t *testing.T) {
		fr := newFakeRegistrar()
		mgr := New(map[string]ServerConfig{
			"fake": {Command: srv},
		}, wired(Options{Registrar: fr}))
		t.Cleanup(mgr.Close)
		mgr.backoff = fastRetry // the re-dial lands on the first retry, not at 1s

		require.NoError(t, mgr.Connect(t.Context(), "fake"))
		s := mgr.serverByName("fake")

		mgr.reconnect(s, s.client()) // a real death for the installed client

		assert.Contains(t, strings.Join(mgr.Logs("fake"), "\n"), "server exited")
		require.Eventually(t, func() bool { // the loop re-dials and re-registers
			return len(fr.AllNames("mcp: fake")) > 0
		}, 5*time.Second, 20*time.Millisecond)
		assert.NotNil(t, s.client())
	})
}

func TestManagerClose(t *testing.T) {
	t.Parallel()

	srv := buildFakeServer(t)

	// a connected server closes well inside the bound, leaving nothing registered
	t.Run("disconnects_servers", func(t *testing.T) {
		fr := newFakeRegistrar()
		mgr := New(map[string]ServerConfig{
			"fake": {Command: srv},
		}, wired(Options{Registrar: fr}))
		mgr.LoadOnFirstMessage(t.Context())
		require.NotEmpty(t, fr.AllNames("mcp: fake"))

		mgr.Close()

		assert.Nil(t, mgr.serverByName("fake").client())
		assert.Empty(t, fr.AllNames("mcp: fake"))
	})

	// a stalled disconnect neither blocks the others nor holds shutdown open past
	// the bound: the user pressed Ctrl+C and the app has to go.
	t.Run("bounded_when_stalled", func(t *testing.T) {
		names := []string{"a", "b", "c"}
		br := &blockingRegistrar{
			fakeRegistrar: newFakeRegistrar(),
			entered:       make(chan string, len(names)),
			release:       make(chan struct{}),
		}
		t.Cleanup(func() { close(br.release) })
		servers := make(map[string]ServerConfig, len(names))
		for _, n := range names {
			servers[n] = ServerConfig{Command: srv}
		}
		mgr := New(servers, wired(Options{Registrar: br}))
		mgr.closeAfter = 50 * time.Millisecond // the sweep bound, not the product 1s

		mgr.Close()

		// every disconnect was entered concurrently, so a stalled one never blocks the others
		assert.Len(t, br.entered, len(names))
	})
}
