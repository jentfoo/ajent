package mcp

import (
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestFetchGoStdioEndToEnd drives the real mcp-fetch-go stdio server (which emits
// notifications/tools/list_changed at startup, like many real servers) through a full
// connect → list → call cycle via the Manager. It is skipped when that binary is not on
// PATH, and guards against the notification deadlock regressing.
func TestFetchGoStdioEndToEnd(t *testing.T) {
	path, err := exec.LookPath("mcp-fetch-go")
	if err != nil {
		t.Skip("mcp-fetch-go not installed; skipping integration test")
	}

	mgr := New(map[string]ServerConfig{
		"browser": {Command: path},
	}, Options{Registrar: newFakeRegistrar(), Notice: func(msg string, w bool) {
		t.Logf("mcp notice(warn=%v): %s", w, msg)
	}})

	mgr.LoadOnFirstMessage(t.Context()) // connects every configured server in full

	var c *Client
	for i := 0; i < 20 && c == nil; i++ { // LoadOnFirstMessage is synchronous, but be safe
		s := mgr.serverByName("browser")
		if s != nil {
			s.mu.Lock()
			c = s.c
			s.mu.Unlock()
		}
		if c == nil {
			time.Sleep(50 * time.Millisecond)
		}
	}
	require.NotNil(t, c, "browser server never connected")

	defs, err := c.Tools(t.Context())
	require.NoError(t, err)
	t.Logf("discovered %d tool(s)", len(defs))
	for _, d := range defs {
		fmt.Printf("- %s: %s\n", d.Name, d.Description)
	}

	if len(defs) > 0 {
		res, err := c.Call(t.Context(), defs[0].Name, []byte(`{"url":"https://example.com"}`), nil)
		require.NoError(t, err)
		t.Logf("fetch call: isError=%v content-lines=%d", res.IsError, len(res.Content))
	}
}
