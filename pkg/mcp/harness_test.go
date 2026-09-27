package mcp

import (
	"bufio"
	"context"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
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

// hostOf strips the scheme and path from a base URL, returning just "host:port".
func hostOf(rawURL string) string {
	u := strings.TrimPrefix(rawURL, "http://")
	if i := strings.IndexByte(u, '/'); i >= 0 {
		u = u[:i]
	}
	return u
}

// drainStderr reads whatever the child has written so far; used to surface a bind failure.
func drainStderr(r io.Reader) string {
	b, _ := io.ReadAll(r)
	return strings.TrimSpace(string(b))
}

// startHTTP launches the fakeserver over Streamable HTTP and returns its base URL
// once it accepts connections. The server binds an ephemeral port itself (port 0)
// and prints the actual address, which we read back from its stderr — no shared,
// release-and-rebind port that another test could steal between close and bind.
func startHTTP(t *testing.T, args ...string) string {
	t.Helper()

	srv := buildFakeServer(t)
	full := append([]string{"-http", "127.0.0.1:0"}, args...)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	c := exec.CommandContext(ctx, srv, full...)
	stderr, err := c.StderrPipe()
	if err != nil {
		t.Fatalf("fakeserver stderr pipe: %v", err)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("start fakeserver http: %v", err)
	}

	// read the actual bound address from stderr; fakeserver prints it before serving.
	var url string
	addrCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() { // blocks until a line arrives or the process dies (EOF)
			line := strings.TrimSpace(sc.Text())
			if addr, ok := strings.CutPrefix(line, "listening on "); ok {
				addrCh <- "http://" + addr + "/mcp"
				return
			}
		}
	}()
	select {
	case url = <-addrCh:
	case <-time.After(5 * time.Second): // server never bound (or died before printing)
		t.Fatalf("fakeserver did not report a listening address; stderr: %s", drainStderr(stderr))
	}
	t.Cleanup(func() {
		_ = c.Process.Kill()
		_ = c.Wait()
	})

	// wait until the HTTP endpoint answers so Connect does not race startup
	require.Eventually(t, func() bool {
		d := net.Dialer{Timeout: 500 * time.Millisecond}
		conn, err := d.DialContext(t.Context(), "tcp", hostOf(url))
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
