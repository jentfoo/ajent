package mcp

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// TestNotificationHandlerDoesNotDeadlock verifies the fix for a stdio deadlock: mcp-go
// delivers notifications on its single stdout-reader goroutine, so a handler that does
// blocking I/O (like re-discovering after tools/list_changed) must not run inline there.
//
// The server emits list_changed when trigger_listchanged is called — deterministically,
// and only after the client has registered handlers. If OnNotification ran its handler
// on the reader goroutine, the rediscovery inside it would wait for a response the same
// blocked reader can never deliver and every later request would hang.
func TestNotificationHandlerDoesNotDeadlock(t *testing.T) {
	t.Parallel()

	c, err := Connect(t.Context(), "fake", stdioConfig(t, "-notify-list-changed"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	var rediscovered atomic.Bool
	// mimic the manager: handle list_changed by synchronously re-listing tools.
	c.OnNotification(func(n mcp.JSONRPCNotification) {
		if n.Method != "notifications/tools/list_changed" {
			return
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		defs, err := c.Tools(ctx)
		if err == nil && len(defs) > 0 { // any non-empty result proves the reader stayed live
			rediscovered.Store(true)
		}
	})

	// trigger_listchanged makes the server respond AND emit list_changed in one burst.
	res, err := c.Call(t.Context(), "trigger_listchanged", json.RawMessage(`{}`), nil)
	require.NoError(t, err)
	assertFalse(res.IsError)

	// The rediscovery inside the notification handler must complete — proving it ran off
	// mcp-go's reader goroutine and did not block subsequent I/O.
	require.Eventually(t, func() bool { return rediscovered.Load() }, 5*time.Second, 20*time.Millisecond,
		"notification-handler re-discovery deadlocked the stdio transport")

	// a follow-up request must still work after notification handling
	res2, err := c.Call(t.Context(), "tool_00", json.RawMessage(`{}`), nil)
	require.NoError(t, err)
	assertFalse(res2.IsError)
}

func assertFalse(b bool) {
	if b {
		panic("expected false")
	}
}
