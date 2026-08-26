package mcp

import (
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
)

// newTestServer builds a detached server not owned by any manager, for direct
// register() unit checks that need no live connection.
func (m *Manager) newTestServer(name string) *server {
	s := newServer(name, ServerConfig{})
	return s
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

	// a first-message load connects every server and registers all of its tools as enabled.
	t.Run("connects", func(t *testing.T) {
		fr := newFakeRegistrar()
		mgr := New(map[string]ServerConfig{
			"fake": {Command: buildFakeServer(t)},
		}, Options{Registrar: fr})

		// nothing is registered before the first message; no process spawned yet
		assert.Empty(t, fr.AllNames("mcp: fake"))
		require.Nil(t, mgr.serverByName("fake").client())

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

	// a second load is a no-op: the server stays connected and its tools are not re-registered.
	t.Run("runs_once", func(t *testing.T) {
		fr := newFakeRegistrar()
		mgr := New(map[string]ServerConfig{
			"fake": {Command: buildFakeServer(t)},
		}, Options{Registrar: fr})

		mgr.LoadOnFirstMessage(t.Context())
		first, ok := fr.toolByName("fake__tool_00")
		require.True(t, ok)

		// disconnect is a manual act; LoadOnFirstMessage must not reconnect it again
		mgr.Disconnect("fake")
		mgr.LoadOnFirstMessage(t.Context())
		assert.Nil(t, mgr.serverByName("fake").client())
		_ = first // registration object itself is unchanged; the point is no reconnect
	})
}

func TestConfigDisabledServer(t *testing.T) {
	t.Parallel()

	// a config-disabled server still connects so its tools appear in /tools, but
	// registers each tool as StateDisabled: known and toggleable, never callable by default.
	t.Run("loads_but_stays_inactive", func(t *testing.T) {
		fr := newFakeRegistrar()
		disabled := false
		mgr := New(map[string]ServerConfig{
			"fake": {Command: buildFakeServer(t), Enabled: &disabled},
		}, Options{Registrar: fr})

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
		disabled := false
		mgr := New(nil, Options{
			Registrar: fr,
			Restore:   []string{"fake__tool_01"}, // enabled via /tools in the prior session
		})
		s := newServer("fake", ServerConfig{Enabled: &disabled})

		defs := []ToolDef{
			{Name: "tool_00", InputSchema: jsonRawObject},
			{Name: "tool_01", InputSchema: jsonRawObject},
		}
		mgr.register(s, defs, nil)

		st, ok := fr.state("fake__tool_00")
		require.True(t, ok)
		assert.Equal(t, StateDisabled, st) // never enabled: config-off default holds

		st, ok = fr.state("fake__tool_01")
		require.True(t, ok)
		assert.Equal(t, StateEnabled, st) // explicit /tools enablement wins over the default
	})
}

// TestRegisterMarksReadOnlyTools verifies bridged read-only tools are recorded on
// the registrar as publication metadata.
func TestRegisterMarksReadOnlyTools(t *testing.T) {
	t.Parallel()

	fr := newFakeRegistrar()
	mgr := New(nil, Options{Registrar: fr})
	s := mgr.newTestServer("srv")

	defs := []ToolDef{
		{Name: "read1", InputSchema: jsonRawObject, ReadOnly: true},
		{Name: "write1", InputSchema: jsonRawObject},
	}
	mgr.register(s, defs, nil) // all enabled

	assert.Contains(t, fr.readonly(), "srv__read1")
	assert.NotContains(t, fr.readonly(), "srv__write1") // not annotated read-only
}

// TestManagerDiscoversResourcesAndPrompts verifies resources and prompts captured at
// connect time are exposed through the manager's API.
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

// TestManagerRediscoverAfterListChanged drives tools/list_changed through the manager's
// real notification path: trigger_listchanged makes the server emit it, and rediscovery
// must re-list + re-register without deadlocking stdio (see Client.OnNotification).
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

	// a follow-up call still works after notification handling.
	s = mgr.serverByName("fake")
	rc = s.client()
	require.NotNil(t, rc)
	res2, err := rc.Call(t.Context(), "tool_00", json.RawMessage(`{}`), nil)
	require.NoError(t, err)
	assert.False(t, res2.IsError)
}

// jsonRawObject is a minimal valid tool schema.
var jsonRawObject = []byte(`{"type":"object","properties":{}}`)
