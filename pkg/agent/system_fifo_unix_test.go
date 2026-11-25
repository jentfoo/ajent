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

// waitLoad runs load off-test and fails once it blocks past the fifo deadline.
func waitLoad(t *testing.T, load func() ([]ProjectInstruction, error), msg string) ([]ProjectInstruction, error) {
	t.Helper()

	var (
		proj []ProjectInstruction
		err  error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		proj, err = load()
	}()
	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond, msg)
	return proj, err
}

func TestLoadProjectInstructionsFifo(t *testing.T) {
	t.Parallel()

	// an unopened fifo at AGENTS.md surfaces an error instead of blocking forever
	t.Run("errors_not_blocks", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "AGENTS.md"), 0o644))

		_, err := waitLoad(t,
			func() ([]ProjectInstruction, error) { return LoadProjectInstructions(dir) }, "blocked on the fifo")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a regular file")
	})

	// a fifo at the project level fails naming the path while already-read files stay
	t.Run("keeps_earlier", func(t *testing.T) {
		global := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(global, "AGENTS.md"), []byte("# Global\n"), 0o644))
		project := t.TempDir()
		require.NoError(t, syscall.Mkfifo(filepath.Join(project, "AGENTS.md"), 0o644))

		proj, err := waitLoad(t,
			func() ([]ProjectInstruction, error) { return LoadProjectInstructions(global, project) },
			"blocked on the fifo")
		require.Error(t, err)
		assert.Contains(t, err.Error(), filepath.Join(project, "AGENTS.md"))
		require.Len(t, proj, 1)
		assert.Equal(t, "# Global\n", proj[0].Body)
	})
}
