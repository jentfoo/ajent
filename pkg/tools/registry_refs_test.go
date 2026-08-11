package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegistryLookupIgnoresEnabled asserts Lookup returns a tool regardless of
// enabled state, while Get respects it — the silent bug picking the wrong one
// causes is what separate methods prevent.
func TestRegistryLookupIgnoresEnabled(t *testing.T) {
	t.Parallel()

	r := New()
	r.Register(&fakeTool{name: "ls"}, false)

	_, ok := r.Get("ls")
	assert.False(t, ok, "Get must refuse a disabled tool")

	got, ok := r.Lookup("ls")
	require.True(t, ok, "Lookup resolves a disabled tool")
	assert.Equal(t, "ls", got.Name())

	_, ok = r.Lookup("ghost")
	assert.False(t, ok, "unknown tools do not resolve")
}

// TestRegistryDisabledListsDisabledOnly asserts Disabled returns the disabled
// tools in declaration order, mirroring Enabled's contract.
func TestRegistryDisabledListsDisabledOnly(t *testing.T) {
	t.Parallel()

	r := New()
	r.Register(&fakeTool{name: "read"}, true)
	r.Register(&fakeTool{name: "ls"}, false)
	r.Register(&fakeTool{name: "find"}, false)

	disabled := r.Disabled()
	got := make([]string, 0, len(disabled))
	for _, t := range disabled {
		got = append(got, t.Name())
	}
	assert.Equal(t, []string{"ls", "find"}, got)
}

// TestRegistryEnableIsAdditive asserts Enable only widens the set, leaving
// already-enabled tools on, unlike SetEnabled which replaces wholesale.
func TestRegistryEnableIsAdditive(t *testing.T) {
	t.Parallel()

	r := New()
	r.Register(&fakeTool{name: "read"}, true)
	r.Register(&fakeTool{name: "ls"}, false)

	r.Enable([]string{"ls"})
	assert.True(t, r.enabled("read"), "Enable must not disable existing tools")
	assert.True(t, r.enabled("ls"))

	r.Enable([]string{"ghost"}) // unknown names ignored, no error
	assert.True(t, r.enabled("ls"))
}

// TestRegistrySourceLabelsGroups asserts RegisterFrom records a source label and
// Source returns it, while Register defaults to builtin.
func TestRegistrySourceLabelsGroups(t *testing.T) {
	t.Parallel()

	r := New()
	r.Register(&fakeTool{name: "read"}, true)
	r.RegisterFrom("filesystem-server", &fakeTool{name: "stat"}, false)

	assert.Equal(t, SourceBuiltin, r.Source("read"))
	assert.Equal(t, "filesystem-server", r.Source("stat"))
	assert.Empty(t, r.Source("ghost"))
}

// TestRegistryTrackerExposedByBuiltins asserts Builtins wires the shared tracker
// so @-expansion can dedupe through it.
func TestRegistryTrackerExposedByBuiltins(t *testing.T) {
	t.Parallel()

	reg, err := Builtins(Options{Cwd: t.TempDir(), SessionID: "t"})
	require.NoError(t, err)
	assert.NotNil(t, reg.Tracker(), "Builtins must expose the shared read tracker")

	// a bare Registry has no tracker, so callers check for nil
	empty := New()
	assert.Nil(t, empty.Tracker())
}
