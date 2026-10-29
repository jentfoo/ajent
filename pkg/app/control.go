package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/paths"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tui"
)

const (
	// controlSocketMax is the sun_path budget the enable path guards: linux
	// allows 108 bytes, darwin 104, and 100 leaves room for the terminator.
	controlSocketMax = 100

	// controlStaleDial bounds the liveness probe of a pre-existing socket.
	controlStaleDial = 500 * time.Millisecond

	// connQueue is the soft bound on one connection's staged outbound lines:
	// past it a connection whose writer stopped flushing is dropped. Hard item
	// and byte caps live beside it in control_conn.go.
	connQueue = 256

	// connDrainWait bounds the flush of queued lines once a connection stops,
	// so shutdown never hangs on a peer that stopped reading.
	connDrainWait = 250 * time.Millisecond

	// controlNoticeKey keys notices about the socket itself (refusals, verified
	// connects), replacing rather than stacking across connections.
	controlNoticeKey = "control"
)

// controlSocketPath builds <config>/control/<session>.sock, refusing a path
// over the unix socket budget rather than letting the bind truncate it. The
// control directory is created and pinned to 0700; a warning names any wider
// mode found there, like models.json's permission check.
func controlSocketPath(sessionID string) (string, string, error) {
	dir, warn, err := paths.SubDir(paths.ControlDirName)
	if err != nil {
		return "", "", err
	}
	path := filepath.Join(dir, sessionID+".sock")
	if len(path) >= controlSocketMax {
		return "", "", fmt.Errorf(
			"control socket path %s is %d bytes, over the %d-byte unix socket limit; move the ajent home closer to the filesystem root",
			path, len(path), controlSocketMax)
	}
	return path, warn, nil
}

// bindControlListener binds the control socket at path. A file already there
// gets one probe connect: alive means another ajent owns the session and the
// bind refuses naming it, dead (ECONNREFUSED) means stale and the file is
// unlinked before rebinding.
func bindControlListener(path, sessionID string) (net.Listener, error) {
	if err := probeControlSocket(path, sessionID); err != nil {
		return nil, err
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, err
	}
	// same-user access only: the 0700 parent already gates other users, the
	// chmod pins the socket itself for any dir-permission slip
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(path) // never leave a dead socket at the path
		return nil, err
	}
	return ln, nil
}

// probeControlSocket refuses a live socket and clears a dead one. A non-socket
// file at the path is never removed: something else owns it.
func probeControlSocket(path, sessionID string) error {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // clean path
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("control socket path %s exists and is not a socket; move it aside", path)
	}
	c, derr := (&net.Dialer{Timeout: controlStaleDial}).Dial("unix", path)
	if derr == nil {
		_ = c.Close()
		return fmt.Errorf("session %s is already running (control socket %s is live)",
			sessionID, path)
	}
	if !errors.Is(derr, syscall.ECONNREFUSED) && !errors.Is(derr, os.ErrNotExist) {
		return fmt.Errorf("cannot probe control socket %s: %w", path, derr)
	}
	return os.Remove(path) // dead peer: a SIGKILL leftover, safe to rebind
}

// controlServer serves one session's control socket: any number of read-only
// observers, each with an independently bounded outbound stage. Delivery never
// blocks the session; a slow or gone consumer is dropped.
type controlServer struct {
	hub     *sessionHub
	dialogs *interactionBroker
	rec     *sessRec // transcript source for backfill; nil when recording is off
	path    string
	serve   bool // serve mode: the socket is the only front end
	// transcriptPath locates the recorded transcript for hello, empty when
	// recording is off: controllers backfill from this file, not over the wire
	transcriptPath string

	ln         net.Listener
	mu         sync.Mutex
	conns      map[*controlConn]struct{}
	closed     bool
	seat       *controlConn // the connection holding the session's single control seat
	seatHolder string       // its announced name, for refusal errors naming the holder
}

// startControl binds the session's control socket and starts serving it,
// attaching the control observers first so every observable change from here
// on fans out. Returned warnings are startup-level (a tightened directory
// mode), meant for the same surface as a failed start. Connections are served
// only once start runs, after driver wiring completes; Close unlinks the
// socket, callers run it on every return path.
func startControl(hub *sessionHub, dialogs *interactionBroker, rec *sessRec, serve bool) (*controlServer, []string, error) {
	snap := hub.Snapshot()
	if snap.SessionID == "" {
		return nil, nil, errors.New("control needs a recorded session; none is open")
	}
	path, warn, err := controlSocketPath(snap.SessionID)
	if err != nil {
		return nil, nil, err
	}
	ln, err := bindControlListener(path, snap.SessionID)
	if err != nil {
		return nil, nil, err
	}
	srv := &controlServer{
		hub: hub, dialogs: dialogs, rec: rec, path: path, serve: serve,
		ln: ln, conns: make(map[*controlConn]struct{}),
	}
	if rec != nil && rec.w != nil {
		srv.transcriptPath = rec.w.Path()
	}
	var warns []string
	if warn != "" {
		warns = append(warns, warn)
	}

	// observe everything before serving: stream events, rendered surfaces,
	// dialogs and transcript appends, each republished as a seq'd event.
	// The control server owns the emitter set today; a second consumer would
	// compose its own layer on top.
	hub.Register(&ctlStreamSink{hub: hub})
	hub.setEmitters(controlEmitters(hub))
	dialogs.observe(controlDialogHooks(hub))
	if rec != nil {
		rec.onAppend = srv.entryAppended
		rec.rec.WatchAppend(srv.entryAppended)
	}
	return srv, warns, nil
}

// start begins serving connections. Called after driver wiring completes so
// no request reads a half-built hub: the bind happened earlier in
// startControl, and dials since then wait in the accept backlog.
func (s *controlServer) start() {
	go s.acceptLoop()
}

// acceptLoop serves connections until Close retires the listener.
func (s *controlServer) acceptLoop() {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return // closed, or the listener died: nothing left to accept
		}
		if !s.admit(nc) {
			continue
		}
		s.track(nc)
	}
}

// admit verifies one accepted connection before anything is served. The
// peer's credentials come from the kernel's connect-time snapshot, never the
// wire, so a connect from any other uid closes at once with a refused event
// in place of hello.
func (s *controlServer) admit(nc net.Conn) bool {
	peer, perr := peerCredentials(nc)
	if perr == nil && !peer.sameUser() {
		perr = fmt.Errorf("peer uid %d is not this session's user %d", peer.UID, os.Geteuid())
	}
	if perr != nil {
		writeControlRefusal(nc, perr)
		_ = nc.Close()
		s.logControl(tui.LevelWarn, "refused control connection: "+perr.Error())
		return false
	}
	s.logPeer(peer)
	return true
}

// logPeer records the verified peer, the host-side twin of --attach logging
// the host pid it dials.
func (s *controlServer) logPeer(peer peerIdentity) {
	msg := fmt.Sprintf("control connection from uid %d", peer.UID)
	if peer.PID > 0 {
		msg = fmt.Sprintf("control connection from pid %d (uid %d)", peer.PID, peer.UID)
	}
	s.logControl(tui.LevelInfo, msg)
}

// logControl pins one socket-lifecycle line to the local surface: a keyed
// notice in an interactive run, stderr under serve mode where no terminal
// renders. These never enter the control event stream: connect noise would
// consume seqs and replay back to every observer.
func (s *controlServer) logControl(level tui.Level, msg string) {
	if s.hub.ui.UI != nil {
		s.hub.ui.UI.NotifyKeyed(controlNoticeKey, msg, level)
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, "ajent: control: %s\n", msg)
}

// track admits one accepted socket as a connection.
func (s *controlServer) track(nc net.Conn) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = nc.Close()
		return
	}
	c := newControlConn(s, nc)
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	c.start()
}

// remove drops a connection from the serving set, freeing the control seat
// with it: death releases immediately and a new controller attaches at once.
func (s *controlServer) remove(c *controlConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, c)
	if s.seat == c {
		s.seat = nil
		s.seatHolder = ""
	}
}

// Close stops accepting, announces session.exit to every connection, flushes
// it a bounded while, then unlinks the socket. Idempotent.
func (s *controlServer) Close(reason string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	conns := slices.Collect(maps.Keys(s.conns))
	s.conns = nil
	s.seat = nil
	s.seatHolder = ""
	s.mu.Unlock()

	_ = s.ln.Close()
	s.hub.publish(ctl.EventSessionExit, ctl.SessionExitData{Reason: reason})
	for _, c := range conns {
		c.detach(true)
	}
	_ = os.Remove(s.path)
}

// entryAppended announces one transcript entry written after attach. Wired
// onto the recorder wrapper success path, never fired for resume-time reads.
func (s *controlServer) entryAppended(e session.Entry) {
	s.hub.publish(ctl.EventEntryAppend, ctl.EntryData{Entry: ctlEntry(e)})
}
