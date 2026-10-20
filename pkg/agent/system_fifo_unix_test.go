//go:build unix

package agent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadProjectInstructionsFifo pins the launch/reload guard: an unopened
// fifo at AGENTS.md surfaces an error instead of blocking forever.
func TestLoadProjectInstructionsFifo(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "AGENTS.md"), 0o644))

	done := make(chan error, 1)
	go func() {
		_, err := LoadProjectInstructions(dir)
		done <- err
	}()
	require.Eventually(t, func() bool {
		select {
		case err := <-done:
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not a regular file")
			return true
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond, "blocked on the fifo")
}

// TestLoadProjectInstructionsFifoKeepsEarlier pins the partial-load rule: a
// fifo at the project AGENTS.md fails the load naming the path while the
// user-global file already read stays in the result.
func TestLoadProjectInstructionsFifoKeepsEarlier(t *testing.T) {
	t.Parallel()

	global := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(global, "AGENTS.md"), []byte("# Global\n"), 0o644))
	project := t.TempDir()
	require.NoError(t, syscall.Mkfifo(filepath.Join(project, "AGENTS.md"), 0o644))

	var (
		proj []ProjectInstruction
		err  error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		proj, err = LoadProjectInstructions(global, project)
	}()
	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond, "blocked on the fifo")

	require.Error(t, err)
	assert.Contains(t, err.Error(), filepath.Join(project, "AGENTS.md"))
	require.Len(t, proj, 1)
	assert.Equal(t, "# Global\n", proj[0].Body)
}
