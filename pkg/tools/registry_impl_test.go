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

func (r *Registry) enabled(name string) bool {
	for _, rt := range r.tools {
		if rt.tool.Name() == name {
			return rt.enabled
		}
	}
	return false
}
