package mcp

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFetchGoStdioEndToEnd drives the real mcp-fetch-go stdio server (which emits
// notifications/tools/list_changed at startup, like many real servers) through a full
// connect-to-list-call cycle via the Manager. It is skipped when that binary is not on
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

	s := mgr.serverByName("browser")
	require.NotNil(t, s, "browser server not configured")
	c := s.client()
	require.NotNil(t, c, "browser server never connected")

	defs, err := c.Tools(t.Context())
	require.NoError(t, err)
	t.Logf("discovered %d tool(s)", len(defs))
	for _, d := range defs {
		t.Logf("- %s: %s", d.Name, d.Description)
	}

	if len(defs) > 0 {
		res, err := c.Call(t.Context(), defs[0].Name, []byte(`{"url":"https://example.com"}`), nil)
		require.NoError(t, err)
		t.Logf("fetch call: isError=%v content-lines=%d", res.IsError, len(res.Content))
	}
}
