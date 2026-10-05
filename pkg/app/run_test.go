package app

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsTerminal(t *testing.T) {
	t.Parallel()

	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	assert.False(t, isTerminal(r)) // a pipe routes the run to the headless path
	assert.False(t, isTerminal(w))

	f, err := os.CreateTemp(t.TempDir(), "f")
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	assert.False(t, isTerminal(f)) // a redirected file is not a terminal either
}
