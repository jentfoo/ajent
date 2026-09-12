package mcp

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// buildFakeServer builds the pkg/mcp fakeserver binary from ./testdata/fakeserver
// with the same module so mcp-go wire types match what our client expects. The
// binary lands in the calling test's temp dir and is removed when that test ends.
func buildFakeServer(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "fakeserver")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", out, "./testdata/fakeserver")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fakeserver: %v\n%s", err, b)
	}
	return out
}

// freePort reserves a TCP port for the HTTP fakeserver.
func freePort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close() // release so the fakeserver can bind it
	return port
}

// startHTTP launches the fakeserver over Streamable HTTP and returns its base URL
// once it accepts connections.
func startHTTP(t *testing.T, args ...string) string {
	t.Helper()
	srv := buildFakeServer(t)
	port := freePort(t)
	full := append([]string{"-http", fmt.Sprintf("127.0.0.1:%d", port)}, args...)
	ctx, cancel := context.WithCancel(t.Context())
	c := exec.CommandContext(ctx, srv, full...)
	if err := c.Start(); err != nil {
		t.Fatalf("start fakeserver http: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = c.Process.Kill()
		_ = c.Wait()
	})

	// wait until the HTTP endpoint answers so Connect does not race startup
	url := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
	require.Eventually(t, func() bool {
		d := net.Dialer{Timeout: 500 * time.Millisecond}
		conn, err := d.DialContext(t.Context(), "tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 2*time.Second, 20*time.Millisecond)
	return url
}

// stdioConfig builds a ServerConfig pointing at the fakeserver binary.
func stdioConfig(t *testing.T, args ...string) ServerConfig {
	t.Helper()
	return ServerConfig{Command: buildFakeServer(t), Args: args}
}
