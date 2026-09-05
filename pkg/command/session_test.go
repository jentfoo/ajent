package command

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionCommand(t *testing.T) {
	t.Parallel()

	t.Run("reports_unnamed_session", func(t *testing.T) {
		c := newFakeConsole(t)

		require.NoError(t, sessionCommand(t.Context(), "", c))
		assert.Contains(t, c.noticesSeen()[0], "unnamed")
	})

	t.Run("reports_current_name", func(t *testing.T) {
		c := newFakeConsole(t)
		c.sessionName = "fix-parser"

		require.NoError(t, sessionCommand(t.Context(), "", c))
		assert.Contains(t, c.noticesSeen()[0], "fix-parser")
	})

	t.Run("sets_name", func(t *testing.T) {
		c := newFakeConsole(t)

		require.NoError(t, sessionCommand(t.Context(), "  fix-parser  ", c))
		assert.Equal(t, "fix-parser", c.sessionName)
		assert.Contains(t, c.noticesSeen()[0], "ajent --resume fix-parser")
	})

	// a rejected name is a notice, so the command line does not fail
	t.Run("rejects_conflicting_name", func(t *testing.T) {
		c := newFakeConsole(t)
		c.setSessionName = errors.New("session name already in use")

		require.NoError(t, sessionCommand(t.Context(), "taken", c))
		assert.Empty(t, c.sessionName)
		assert.Contains(t, c.noticesSeen()[0], "already in use")
	})
}
