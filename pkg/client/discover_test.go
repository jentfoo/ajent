package client

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/paths"
)

// TestDiscoverReadsRegistry pins discovery over a real control dir: the fake
// instance's registry record is found without dialing.
func TestDiscoverReadsRegistry(t *testing.T) {
	f := newFakeSession(t)
	f.writeRegistry(t)
	t.Setenv(paths.EnvHome, f.home) // env pinned: not parallel

	live, err := Discover()
	require.NoError(t, err)
	require.Len(t, live, 1)
	assert.Equal(t, f.Path(), live[0].Socket)
	assert.Equal(t, f.sessionID, live[0].SessionID)
	assert.False(t, live[0].Started().IsZero()) // the server's startedAt decodes
}

// TestResolveTargetForms pins the target forms: an exact socket path, a
// session id in the registry, its name, and a unique id prefix.
func TestResolveTargetForms(t *testing.T) {
	f := newFakeSession(t)
	f.writeRegistry(t)
	t.Setenv(paths.EnvHome, f.home) // env pinned: not parallel

	live, err := Discover()
	require.NoError(t, err)
	require.Len(t, live, 1)

	t.Run("exact_path", func(t *testing.T) {
		match, _, rerr := Resolve(f.Path())
		require.NoError(t, rerr)
		require.Len(t, match, 1)
		assert.Equal(t, f.Path(), match[0].Socket)
	})

	t.Run("session_id", func(t *testing.T) {
		match, _, rerr := Resolve(live[0].SessionID)
		require.NoError(t, rerr)
		require.Len(t, match, 1)
		assert.Equal(t, f.Path(), match[0].Socket)
	})

	t.Run("unique_prefix", func(t *testing.T) {
		match, _, rerr := Resolve(live[0].SessionID[:8])
		require.NoError(t, rerr)
		require.Len(t, match, 1)
		assert.Equal(t, f.Path(), match[0].Socket)
	})

	t.Run("unknown_refused", func(t *testing.T) {
		_, _, rerr := Resolve("zz-nothing")
		require.Error(t, rerr)
	})
}

// TestFollowablePrefersSameInstance pins the reconnect rule: the same token
// wins, a single survivor is followed, several answer nil for the picker.
func TestFollowablePrefersSameInstance(t *testing.T) {
	t.Parallel()

	same := Instance{SessionID: "01A", Token: "tokA", Socket: "/a"}
	other := Instance{SessionID: "01A", Token: "tokB", Socket: "/b"}
	foreign := Instance{SessionID: "01B", Token: "tokC", Socket: "/c"}

	t.Run("same_token_wins", func(t *testing.T) {
		got := Followable([]Instance{same, other, foreign}, "01A", "tokA")
		require.Len(t, got, 1)
		assert.Equal(t, "tokA", got[0].Token)
	})

	// one survivor of the session: follow it even under a different token
	t.Run("single_survivor_followed", func(t *testing.T) {
		got := Followable([]Instance{other, foreign}, "01A", "tokA")
		require.Len(t, got, 1)
		assert.Equal(t, "tokB", got[0].Token)
	})

	// several remain: never a silent swap, the picker decides
	t.Run("several_nil", func(t *testing.T) {
		assert.Nil(t, Followable([]Instance{same, other}, "01A", "tokGone"))
	})
}
