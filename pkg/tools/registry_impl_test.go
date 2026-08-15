package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistryDeclarationOrder(t *testing.T) {
	t.Parallel()

	r := New()
	for _, n := range []string{"read", "write", "edit"} {
		r.Register(&fakeTool{name: n}, true)
	}
	assert.Equal(t, []string{"read", "write", "edit"}, r.Names())
	enabled := r.Enabled()
	got := make([]string, 0, len(enabled))
	for _, t := range enabled {
		got = append(got, t.Name())
	}
	assert.Equal(t, []string{"read", "write", "edit"}, got)
}

func TestRegistryDefaultEnableDisable(t *testing.T) {
	t.Parallel()

	r := New()
	r.Register(&fakeTool{name: "bash"}, true)
	r.Register(&fakeTool{name: "find"}, false)

	assert.True(t, r.enabled("bash"))
	assert.False(t, r.enabled("find"))

	_, ok := r.Get("bash")
	assert.True(t, ok) // enabled tools resolve
	_, ok = r.Get("find")
	assert.False(t, ok) // disabled ones do not
}

func TestRegistrySetEnabledWithUnknownNameIgnored(t *testing.T) {
	t.Parallel()

	r := New()
	for _, n := range []string{"a", "b"} {
		r.Register(&fakeTool{name: n}, true)
	}
	r.SetEnabled([]string{"a", "ghost"}) // ghost is unknown and ignored
	assert.Equal(t, []string{"a"}, r.Names())
}

func TestRegistrySchemaCacheInvalidatedBySetEnabled(t *testing.T) {
	t.Parallel()

	r := New()
	for _, n := range []string{"read", "write"} {
		r.Register(&fakeTool{name: n}, true)
	}
	schemas1 := r.Schemas()
	assert.Len(t, schemas1, 2)

	r.SetEnabled([]string{"write"}) // busts the cache
	schemas2 := r.Schemas()
	require.Len(t, schemas2, 1)
	assert.Equal(t, "write", schemas2[0].Name)
}

// TestRegistryEnablePromotesDisabled verifies Enable widens a disabled MCP tool to
// enabled so its schema reaches the prompt (the post-first-prompt /tools path).
func TestRegistryEnablePromotesDisabled(t *testing.T) {
	t.Parallel()

	r := New()
	for _, n := range []string{"a", "b"} {
		r.RegisterState("mcp: srv", &fakeTool{name: "srv__" + n}, StateDisabled)
	}

	assert.Empty(t, r.Schemas())
	_, ok := r.Get("srv__a")
	assert.False(t, ok) // disabled tools are not callable by name

	r.Enable([]string{"srv__b"}) // promotion widens to enabled
	names := r.Names()
	require.Len(t, names, 1)
	assert.Equal(t, "srv__b", names[0])
	assert.Len(t, r.Schemas(), 1) // promoted schema now reaches the prompt
}

// TestRegistryMarkReadOnlyMetadata verifies read-only marking is queryable by
// name and dropped with the tool it belongs to.
func TestRegistryMarkReadOnlyMetadata(t *testing.T) {
	t.Parallel()

	r := New()
	for _, n := range []string{"srv__a", "srv__b"} {
		r.RegisterFrom("mcp: srv", &fakeTool{name: n}, true)
	}
	assert.False(t, r.ReadOnly("srv__a")) // default is not read-only
	assert.False(t, r.ReadOnly("unknown"))

	r.MarkReadOnly([]string{"srv__b", "ghost"}) // ghost is unknown and ignored
	assert.True(t, r.ReadOnly("srv__b"))
	assert.False(t, r.ReadOnly("srv__a")) // unmarked stays false

	r.Unregister("mcp: srv") // the mark goes with its tool
	assert.False(t, r.ReadOnly("srv__b"))
}

// TestRegistryEnabledNamesBySource verifies EnabledNames scopes to a source.
func TestRegistryEnabledNamesBySource(t *testing.T) {
	t.Parallel()

	r := New()
	r.RegisterFrom("mcp: x", &fakeTool{name: "x__1"}, true)
	r.RegisterFrom("mcp: x", &fakeTool{name: "x__2"}, false)
	r.Register(&fakeTool{name: "read"}, true)

	names := r.EnabledNames("mcp: x")
	assert.Equal(t, []string{"x__1"}, names) // only the enabled one from that source
}

func (r *Registry) enabled(name string) bool {
	for _, rt := range r.tools {
		if rt.tool.Name() == name {
			return rt.state == StateEnabled
		}
	}
	return false
}
