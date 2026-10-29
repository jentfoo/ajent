package app

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/paths"
)

// serveModelsJSON is one never-dialed provider, so the serve run starts with a
// model configured and skips first-run setup.
const serveModelsJSON = `{
  "defaultModel": "fake/m1",
  "providers": {
    "fake": {
      "api": "openai-completions",
      "flavor": "generic",
      "baseUrl": "http://127.0.0.1:1",
      "models": [{ "id": "m1", "contextWindow": 100000, "maxTokens": 4096 }]
    }
  }
}`

// writeServeHome configures an isolated home with control enabled and a model.
func writeServeHome(t *testing.T, controlEnabled bool) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	require.NoError(t, os.WriteFile(filepath.Join(home, llm.ModelsFileName), []byte(serveModelsJSON), 0o600))
	var conf string
	if controlEnabled {
		conf = `{"control": {"enabled": true}}`
	} else {
		conf = `{}`
	}
	require.NoError(t, os.WriteFile(filepath.Join(home, config.ConfigFileName), []byte(conf), 0o600))
	return home
}

// TestRunServeNeedsControlEnabled pins the usage error: serve without the
// deliberate opt-in never starts.
func TestRunServeNeedsControlEnabled(t *testing.T) {
	// not parallel: AJENT_HOME isolates the config layers
	writeServeHome(t, false)

	assert.Equal(t, ExitUsage, Run(RunOptions{Serve: true}))
}

// TestServeRunLifecycle pins the serve contract end to end: the socket binds
// before the run serves, a controller attaches and polls, a SIGTERM ends the
// run cleanly, every connection sees session.exit, and the socket unlinks.
func TestServeRunLifecycle(t *testing.T) {
	// not parallel: AJENT_HOME isolates config, socket dir and sessions
	writeServeHome(t, true)

	code := make(chan int, 1)
	go func() { code <- Run(RunOptions{Serve: true}) }()

	// the socket appears before the run accepts, one per session
	var path string
	require.Eventually(t, func() bool {
		p, ok := onlySocket(t)
		if ok {
			path = p
		}
		return ok
	}, 10*time.Second, 10*time.Millisecond, "the serve socket never bound")

	// a controller attaches: serve front end, session identity, pollable state
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	c := &ctlClient{t: t, conn: conn, enc: ctl.NewEncoder(conn), inc: make(chan ctl.Incoming, 128)}
	go func() {
		dec := ctl.NewDecoder(conn)
		for {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				close(c.inc)
				return
			}
			if in, derr := ctl.DecodeIncoming(raw); derr == nil {
				c.inc <- in
			}
		}
	}()
	h := c.hello()
	assert.True(t, h.Serve)
	assert.False(t, h.Interactive)
	assert.NotEmpty(t, h.Session.ID)

	rp := c.call("s1", ctl.MethodState, map[string]any{})
	require.Nil(t, rp.Error)

	// SIGTERM ends the run: the signal handler installed before the bind, so
	// the socket above proves the signal will be caught
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))

	select {
	case exit := <-code:
		assert.Equal(t, ExitOK, exit)
	case <-time.After(10 * time.Second):
		t.Fatal("the serve run never exited on SIGTERM")
	}

	// session.exit lands, then EOF, then the socket is gone
	exitSeen := false
	deadline := time.Now().Add(ctlWait)
	for !exitSeen && time.Now().Before(deadline) {
		in, ok := <-c.inc
		require.True(t, ok)
		if in.Event != nil && in.Event.Name == ctl.EventSessionExit {
			exitSeen = true
		}
	}
	assert.True(t, exitSeen, "session.exit never arrived")
	require.Eventually(t, func() bool {
		_, derr := os.Stat(path)
		return os.IsNotExist(derr)
	}, 5*time.Second, 10*time.Millisecond, "the socket never unlinked")
}

// TestServeRunEndsThroughExit pins the controller's kill switch: a
// controller claims the seat, `/exit` through submit routes like any line,
// every connection receives session.exit then EOF, the socket unlinks and
// the run exits clean.
func TestServeRunEndsThroughExit(t *testing.T) {
	// not parallel: AJENT_HOME isolates config, socket dir and sessions
	writeServeHome(t, true)

	code := make(chan int, 1)
	go func() { code <- Run(RunOptions{Serve: true}) }()

	var path string
	require.Eventually(t, func() bool {
		p, ok := onlySocket(t)
		if ok {
			path = p
		}
		return ok
	}, 10*time.Second, 10*time.Millisecond, "the serve socket never bound")

	ctlConn := dialControl(t, path)
	h := ctlConn.hello()
	assert.True(t, h.Serve)
	rp := ctlConn.call("seat", ctl.MethodAttach, map[string]any{"control": true, "holder": "ci"})
	require.Nil(t, rp.Error)

	// a second observer sees the same exit sequence
	obs := dialControl(t, path)
	obs.hello()

	rp = ctlConn.call("exit", ctl.MethodSubmit, map[string]any{"line": "/exit"})
	require.Nil(t, rp.Error)

	select {
	case exit := <-code:
		assert.Equal(t, ExitOK, exit)
	case <-time.After(10 * time.Second):
		t.Fatal("the serve run never exited on /exit")
	}

	// every connection receives session.exit then EOF, and the socket unlinks
	for _, c := range []*ctlClient{ctlConn, obs} {
		exitSeen := false
		for !exitSeen {
			in, ok := <-c.inc
			if !ok {
				t.Fatal("socket closed before session.exit arrived")
			}
			if in.Event != nil && in.Event.Name == ctl.EventSessionExit {
				exitSeen = true
			}
			_ = in
		}
		select {
		case _, ok := <-c.inc:
			assert.False(t, ok, "EOF should follow session.exit")
		case <-time.After(ctlWait):
			t.Fatal("EOF never followed session.exit")
		}
	}
	require.Eventually(t, func() bool {
		_, derr := os.Stat(path)
		return os.IsNotExist(derr)
	}, 5*time.Second, 10*time.Millisecond, "the socket never unlinked")
}

// onlySocket reports the one live control socket under the config dir.
func onlySocket(t *testing.T) (string, bool) {
	t.Helper()
	dir, err := paths.Dir()
	require.NoError(t, err)
	entries, rerr := os.ReadDir(filepath.Join(dir, controlDirName))
	if rerr != nil || len(entries) != 1 {
		return "", false
	}
	name := entries[0].Name()
	if !strings.HasSuffix(name, ".sock") {
		return "", false
	}
	return filepath.Join(dir, controlDirName, name), true
}
