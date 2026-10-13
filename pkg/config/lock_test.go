package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLockSave(t *testing.T) {
	// not parallel: AJENT_HOME and lockWait pin process state
	t.Run("acquires_exclusively", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("AJENT_HOME", home)
		release := LockSave()
		require.NotNil(t, release)

		sibling, err := os.OpenFile(filepath.Join(home, lockFileName), os.O_CREATE|os.O_RDWR, SecretPerm)
		require.NoError(t, err)
		t.Cleanup(func() { _ = sibling.Close() })

		require.ErrorIs(t, lockExclusive(sibling), errLockContended)

		release()
		assert.NoError(t, lockExclusive(sibling)) // free again once released
	})

	t.Run("degrades_when_contended", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("AJENT_HOME", home)
		held, err := os.OpenFile(filepath.Join(home, lockFileName), os.O_CREATE|os.O_RDWR, SecretPerm)
		require.NoError(t, err)
		t.Cleanup(func() { _ = held.Close() })
		require.NoError(t, lockExclusive(held))
		shortenWait(t)

		start := time.Now()
		release := LockSave() // returns without the lock instead of hanging on it
		require.NotNil(t, release)
		assert.Less(t, time.Since(start), 10*time.Second)
		t.Cleanup(release)
	})

	t.Run("creates_one_shared_lockfile", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("AJENT_HOME", home)
		release := LockSave()
		require.NotNil(t, release)
		t.Cleanup(release)

		assert.FileExists(t, filepath.Join(home, lockFileName))
	})

	t.Run("degrades_when_open_fails", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "not-a-dir")
		require.NoError(t, os.WriteFile(blocker, []byte("x"), SecretPerm))
		t.Setenv("AJENT_HOME", blocker) // Dir's MkdirAll fails, so the lockfile cannot exist

		release := LockSave()
		require.NotNil(t, release)
		t.Cleanup(release)
	})
}

// shortenWait shrinks the advisory-lock deadline and restores it at cleanup.
func shortenWait(t *testing.T) {
	t.Helper()

	old := lockWait
	lockWait = 20 * time.Millisecond
	t.Cleanup(func() { lockWait = old })
}

func TestSaveAcrossSiblings(t *testing.T) {
	// four sibling Sets race saves over one user layer, as two ajent processes
	// share it; the shared save lock serializes each read-modify-write so every key
	// survives.
	t.Setenv("AJENT_HOME", t.TempDir())

	const siblings, saves = 4, 6
	var wg sync.WaitGroup
	for i := 0; i < siblings; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, _, err := Load(Options{Workspace: "."})
			assert.NoError(t, err)
			for j := 0; j < saves; j++ {
				_, err = s.Save("user", fmt.Sprintf("keys.s%d.k%d", i, j), true)
				assert.NoError(t, err)
			}
		}(i)
	}
	wg.Wait()

	s, _, err := Load(Options{Workspace: "."})
	require.NoError(t, err)
	for i := 0; i < siblings; i++ {
		for j := 0; j < saves; j++ {
			key := fmt.Sprintf("keys.s%d.k%d", i, j)
			_, _, ok := s.Explain(key)
			assert.True(t, ok, key) // a lost sibling save shows up here
		}
	}
}
