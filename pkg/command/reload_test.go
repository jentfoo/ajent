package command

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReloadCommand(t *testing.T) {
	t.Parallel()

	t.Run("calls_console", func(t *testing.T) {
		c := newFakeConsole(t)
		require.NoError(t, reloadCommand(context.Background(), "", c))
		assert.Equal(t, 1, c.reloaded)
		assert.Empty(t, c.noticesSeen()) // the host owns every progress notice
	})
	t.Run("error_notified", func(t *testing.T) {
		c := newFakeConsole(t)
		c.reloadErr = errors.New("config.json: unexpected end of JSON input")
		require.NoError(t, reloadCommand(context.Background(), "", c))
		assert.True(t, c.noticeContains("unexpected end of JSON input"))
	})
}
