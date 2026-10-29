package app

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPeerCredentialsSelfDial verifies the credential snapshot over a real
// socket pair: this process dials itself, so the kernel-reported identity is
// known exactly.
func TestPeerCredentialsSelfDial(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "probe.sock")
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	type dial struct {
		peer peerIdentity
		err  error
	}
	accepted := make(chan dial, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			accepted <- dial{err: aerr}
			return
		}
		defer func() { _ = c.Close() }()
		p, perr := peerCredentials(c)
		accepted <- dial{peer: p, err: perr}
	}()

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	var res dial
	select {
	case res = <-accepted:
	case <-t.Context().Done():
		t.Fatal("timed out waiting for the accepted connection")
	}
	require.NoError(t, res.err)

	assert.True(t, res.peer.sameUser())
	assert.Equal(t, os.Geteuid(), res.peer.UID)
	if runtime.GOOS == "linux" {
		// only linux attests the connecting process's pid
		assert.Equal(t, os.Getpid(), res.peer.PID)
	}
}

func TestPeerIdentitySameUser(t *testing.T) {
	t.Parallel()

	self := peerIdentity{UID: os.Geteuid()}
	other := peerIdentity{UID: os.Geteuid() + 1}
	assert.True(t, self.sameUser())
	assert.False(t, other.sameUser())
}

// TestWriteControlRefusal verifies the refused event a rejected peer reads in
// place of hello.
func TestWriteControlRefusal(t *testing.T) {
	t.Parallel()

	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	done := make(chan string, 1)
	go func() {
		line, err := bufio.NewReader(client).ReadBytes('\n')
		var ev ctl.Event
		if err == nil && json.Unmarshal(line, &ev) == nil && ev.Name == ctl.EventRefused {
			var rd ctl.RefusedData
			if ev.Decode(&rd) == nil {
				done <- rd.Reason
			}
		}
	}()

	writeControlRefusal(server, assert.AnError)

	select {
	case reason := <-done:
		assert.Contains(t, reason, assert.AnError.Error())
	case <-t.Context().Done():
		t.Fatal("timed out waiting for the refused event")
	}
}
