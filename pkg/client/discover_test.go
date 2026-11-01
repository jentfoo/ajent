package client

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/paths"
)

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

// reapChild starts a child process and waits for it, returning the one pid
// this test knows is gone.
func reapChild(t *testing.T) int {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "true")
	require.NoError(t, cmd.Start())
	require.NoError(t, cmd.Wait())
	return cmd.Process.Pid
}

func TestInstanceDead(t *testing.T) {
	t.Parallel()

	gone := reapChild(t)
	old := time.Now().Add(-2 * staleAfter).UnixMilli()
	fresh := time.Now().UnixMilli()

	t.Run("live_pid", func(t *testing.T) {
		assert.False(t, InstanceDead(Instance{PID: os.Getpid(), StartedAt: old}))
	})

	t.Run("reaped_pid_old_record", func(t *testing.T) {
		assert.True(t, InstanceDead(Instance{PID: gone, StartedAt: old}))
	})

	// the age gate holds even a provably dead pid for its first minute, matching
	// the sweep's grace for a writer that may still be binding
	t.Run("reaped_pid_fresh_record", func(t *testing.T) {
		assert.False(t, InstanceDead(Instance{PID: gone, StartedAt: fresh}))
	})

	// no usable pid is nothing to judge, so the record stays
	t.Run("no_pid_reads_alive", func(t *testing.T) {
		assert.False(t, InstanceDead(Instance{StartedAt: old}))
	})
}

func TestDiscoverLiveFiltersStale(t *testing.T) {
	f := newFakeSession(t)
	f.writeRegistry(t) // live: this process's pid, fresh start

	stale := Instance{
		SessionID: "01STALE0000000000000", Token: "oldtoken",
		Socket: filepath.Join(f.home, "gone.sock"), PID: reapChild(t),
		StartedAt: time.Now().Add(-2 * staleAfter).UnixMilli(),
	}
	b, err := json.Marshal(stale)
	require.NoError(t, err)
	dir := filepath.Dir(f.Path())
	require.NoError(t, os.WriteFile(filepath.Join(dir, "oldtoken.json"), b, 0o600))
	t.Setenv(paths.EnvHome, f.home) // env pinned: not parallel

	all, err := Discover()
	require.NoError(t, err)
	assert.Len(t, all, 2) // discovery itself returns records verbatim

	live, err := DiscoverLive()
	require.NoError(t, err)
	require.Len(t, live, 1)
	assert.Equal(t, f.sessionID, live[0].SessionID)
}

func TestSavedSessionsOpensStore(t *testing.T) {
	t.Setenv(paths.EnvHome, t.TempDir()) // env pinned: not parallel

	saved, err := SavedSessions() // opening the store creates the root
	require.NoError(t, err)
	assert.Empty(t, saved)
}

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
