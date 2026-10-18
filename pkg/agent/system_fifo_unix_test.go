//go:build unix

package agent

import (
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
