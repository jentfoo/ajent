package mcp

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestLoadOnFirstMessage(t *testing.T) {
	t.Parallel()

	// build once at the test level; every case reuses the same binary
	srv := buildFakeServer(t)

	// a first-message load connects every server and registers all of its tools as enabled
	t.Run("connects", func(t *testing.T) {
		fr := newFakeRegistrar()
		mgr := New(map[string]ServerConfig{
			"fake": {Command: srv},
		}, Options{Registrar: fr})

		// nothing is registered before the first message; no process spawned yet
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
		}, Options{Registrar: fr})
		t.Cleanup(mgr.Close)

		mgr.LoadOnFirstMessage(t.Context())

		// disconnect is a manual act; LoadOnFirstMessage must not reconnect it again
		mgr.Disconnect("fake")
		before := len(fr.AllNames("mcp: fake"))
		mgr.LoadOnFirstMessage(t.Context())
		assert.Nil(t, mgr.serverByName("fake").client())
		// no re-registration either
		assert.Len(t, fr.AllNames("mcp: fake"), before)
	})
}

func TestConfigDisabledServer(t *testing.T) {
	t.Parallel()

	// built at the test level; reused by the live-connection case below
	srv := buildFakeServer(t)

	// a config-disabled server still connects so its tools appear in /tools, but
	// registers each tool as StateDisabled: known and toggleable, never callable by default.
	t.Run("loads_but_stays_inactive", func(t *testing.T) {
		fr := newFakeRegistrar()
		var disabled bool
		mgr := New(map[string]ServerConfig{
			"fake": {Command: srv, Enabled: &disabled},
		}, Options{Registrar: fr})

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
		mgr := New(nil, Options{
			Registrar: fr,
			Restore:   []string{"fake__tool_01"}, // enabled via /tools in the prior session
		})
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
		"fake": {Command: buildFakeServer(t), Args: []string{"-startup-delay=1s"}},
	}, Options{Registrar: fr})
	t.Cleanup(mgr.Close)

	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Connect(t.Context(), "fake") }()

	s := mgr.serverByName("fake")
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.connecting // the dial holds the slot while startup delay blocks it
	}, time.Second, 10*time.Millisecond)

	// remove the server from the map mid-dial; dial must not install its client
	mgr.mu.Lock()
	delete(mgr.servers, "fake")
	mgr.mu.Unlock()

	require.ErrorContains(t, <-errCh, "server removed during connect")
}

func TestConcurrentConnectsShareOneClient(t *testing.T) {
	t.Parallel()

	fr := newFakeRegistrar()
	mgr := New(map[string]ServerConfig{
		"fake": {Command: buildFakeServer(t), Args: []string{"-startup-delay=1s"}},
	}, Options{Registrar: fr})
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

func TestRegisterMarksReadOnlyTools(t *testing.T) {
	t.Parallel()

	fr := newFakeRegistrar()
	mgr := New(nil, Options{Registrar: fr})
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
	mgr := New(nil, Options{Registrar: fr})
	s := mgr.newTestServer("srv")
	defs := []ToolDef{
		{Name: "a", InputSchema: jsonRawObject},
		{Name: "b", InputSchema: jsonRawObject},
	}

	// first registration exposes everything; the user then turns b off via /tools
	mgr.register(s, nil, defs, nil)
	assert.Equal(t, StateEnabled, mustState(fr, "srv__a"))
	fr.set("srv__b", StateDisabled)

	// capture the live split and re-register as a refresh would (unregister first)
	keep := mgr.captureLive(s.source)
	fr.Unregister(s.source)
	mgr.register(s, nil, defs, keep)

	assert.Equal(t, StateEnabled, mustState(fr, "srv__a"))  // live enablement kept
	assert.Equal(t, StateDisabled, mustState(fr, "srv__b")) // explicit off survives re-registration
}

func TestRegisterLiveDisabledBeatsRestore(t *testing.T) {
	t.Parallel()

	fr := newFakeRegistrar()
	mgr := New(nil, Options{Registrar: fr, Restore: []string{"srv__a", "srv__b"}})
	s := mgr.newTestServer("srv")
	defs := []ToolDef{
		{Name: "a", InputSchema: jsonRawObject},
		{Name: "b", InputSchema: jsonRawObject},
	}

	// a prior registration enabled both, then the user disabled b; Restore still names it
	mgr.register(s, nil, defs, &toolState{
		enabled:  map[string]struct{}{"srv__a": {}},
		disabled: map[string]struct{}{"srv__b": {}},
	})

	assert.Equal(t, StateEnabled, mustState(fr, "srv__a"))
	assert.Equal(t, StateDisabled, mustState(fr, "srv__b")) // live off wins over Restore
}

func mustState(fr *fakeRegistrar, name string) State {
	st, ok := fr.state(name)
	if !ok {
		panic("unregistered tool: " + name)
	}
	return st
}

func TestManagerDiscoversResourcesAndPrompts(t *testing.T) {
	t.Parallel()

	fr := newFakeRegistrar()
	mgr := New(map[string]ServerConfig{
		"fake": {Command: buildFakeServer(t)},
	}, Options{Registrar: fr})
	require.NoError(t, mgr.Connect(t.Context(), "fake"))
	t.Cleanup(mgr.Close)

	rs := mgr.ServerResources("fake")
	require.Len(t, rs, 1)
	assert.Equal(t, "the doc", rs[0].Name)

	ps := mgr.ServerPrompts("fake")
	require.Len(t, ps, 1)
	assert.Equal(t, "summarize", ps[0].Name)
	require.Len(t, ps[0].Arguments, 1)
	assert.True(t, ps[0].Arguments[0].Required)
}

func TestManagerRediscoverAfterListChanged(t *testing.T) {
	fr := newFakeRegistrar()
	mgr := New(map[string]ServerConfig{
		"fake": stdioConfig(t, "-notify-list-changed"),
	}, Options{Registrar: fr})

	require.NoError(t, mgr.Connect(t.Context(), "fake"))
	t.Cleanup(mgr.Close)
	_, ok := fr.toolByName("fake__tool_00")
	require.True(t, ok) // connected and registered before the notification fires

	// trigger_listchanged responds AND emits list_changed; rediscovery must complete.
	s := mgr.serverByName("fake")
	rc := s.client()
	require.NotNil(t, rc)
	res, err := rc.Call(t.Context(), "trigger_listchanged", json.RawMessage(`{}`), nil)
	require.NoError(t, err)
	assert.False(t, res.IsError)

	// the async rediscovery re-registers within a short window; wait until it has fully
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

// jsonRawObject is a minimal valid tool schema.
var jsonRawObject = []byte(`{"type":"object","properties":{}}`)

func TestReload(t *testing.T) {
	// reload reads mcp.json, so each case owns a workspace and AJENT_HOME; no t.Parallel

	setup := func(t *testing.T, initial string) (*Manager, *fakeRegistrar, string) {
		t.Helper()
		t.Setenv("AJENT_HOME", mkHome(t))
		ws := t.TempDir()
		mkFile(t, ws+"/.ajent/mcp.json", initial)
		servers, _, err := LoadConfig(ws)
		require.NoError(t, err)

		fr := newFakeRegistrar()
		mgr := New(servers, Options{Registrar: fr, Workspace: ws})
		t.Cleanup(mgr.Close)
		mgr.LoadOnFirstMessage(t.Context())
		return mgr, fr, ws
	}
	cmd := buildFakeServer(t)
	cfgJSON := func(extra string) string {
		return `{"servers":{"fake":{"command":` + strconv.Quote(cmd) + extra + `}}}`
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

func TestReconnectAfterDeath(t *testing.T) {
	t.Parallel()

	fr := newFakeRegistrar()
	mgr := New(map[string]ServerConfig{
		"fake": {Command: buildFakeServer(t), Args: []string{"-die"}},
	}, Options{Registrar: fr})
	t.Cleanup(mgr.Close)

	// disable one tool so the restored enabled set is a strict subset, not everything
	mgr.LoadOnFirstMessage(t.Context())
	die := mgr.serverByName("fake")
	require.NotNil(t, die.client())

	fr.set("fake__tool_01", StateDisabled) // the user turned it off via /tools

	// kill the child through its own trigger_die tool; Execute returns an error
	// result (transport failure) rather than a Go error.
	tool, ok := fr.toolByName("fake__trigger_die")
	require.True(t, ok)
	_, err := tool.Execute(t.Context(), agent.ToolCall{ID: "die", Name: tool.Name()}, nil)
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

func TestManagerClose(t *testing.T) {
	t.Parallel()

	// built once at the test level; reused by both cases below
	srv := buildFakeServer(t)

	// a connected server closes well inside the bound, leaving nothing registered
	t.Run("disconnects_servers", func(t *testing.T) {
		fr := newFakeRegistrar()
		mgr := New(map[string]ServerConfig{
			"fake": {Command: srv},
		}, Options{Registrar: fr})
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
		mgr := New(servers, Options{Registrar: br})

		mgr.Close()

		// every disconnect was entered concurrently; a stalled one never blocks the others
		assert.Len(t, br.entered, len(names))
	})
}
