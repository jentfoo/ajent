package app

import (
	"context"
	"encoding/json"
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

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/session"
)

const (
	// controlDirName holds session sockets under the ajent config dir, apart
	// from sessions/ so listing and deletion tooling never sweeps one mid-run
	// and a stale file is never mistaken for a transcript.
	controlDirName = "control"

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
)

// controlSocketPath builds <config>/control/<session>.sock, refusing a path
// over the unix socket budget rather than letting the bind truncate it.
func controlSocketPath(sessionID string) (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, controlDirName, sessionID+".sock")
	if len(path) >= controlSocketMax {
		return "", fmt.Errorf(
			"control socket path %s is %d bytes, over the %d-byte unix socket limit; move the ajent home closer to the filesystem root",
			path, len(path), controlSocketMax)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return path, nil
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

	ln         net.Listener
	mu         sync.Mutex
	conns      map[*controlConn]struct{}
	closed     bool
	seat       *controlConn // the connection holding the session's single control seat
	seatHolder string       // its announced name, for refusal errors naming the holder
}

// startControl binds the session's control socket and starts serving it,
// attaching the control observers first so every observable change from here
// on fans out. Connections are served only once start runs, after driver
// wiring completes; Close unlinks the socket, callers run it on every return
// path.
func startControl(hub *sessionHub, dialogs *interactionBroker, rec *sessRec, serve bool) (*controlServer, error) {
	snap := hub.Snapshot()
	if snap.SessionID == "" {
		return nil, errors.New("control needs a recorded session; none is open")
	}
	path, err := controlSocketPath(snap.SessionID)
	if err != nil {
		return nil, err
	}
	ln, err := bindControlListener(path, snap.SessionID)
	if err != nil {
		return nil, err
	}
	srv := &controlServer{
		hub: hub, dialogs: dialogs, rec: rec, path: path, serve: serve,
		ln: ln, conns: make(map[*controlConn]struct{}),
	}

	// observe everything before serving: stream events, rendered surfaces,
	// dialogs and transcript appends, each republished as a seq'd event
	hub.Register(&ctlStreamSink{hub: hub})
	hub.setEmitters(chainEmitters(hub.emitting(), controlEmitters(hub)))
	dialogs.observe(controlDialogHooks(hub))
	if rec != nil {
		rec.onAppend = srv.entryAppended
		rec.rec.WatchAppend(srv.entryAppended)
	}
	return srv, nil
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
		s.track(nc)
	}
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
	s.hub.noteHead(e.ID) // appends extend the head; only a switch moves it loudly
	s.hub.publish(ctl.EventEntryAppend, ctl.EntryData{Entry: ctlEntry(e)})
}

// transcript answers a transcript.entries request: the current branch, oldest
// first, bounded by an optional entry id cursor.
func (s *controlServer) transcript(params []byte) (ctl.TranscriptResult, *ctl.Error) {
	var p ctl.TranscriptParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return ctl.TranscriptResult{}, ctl.Errf(ctl.CodeBadRequest, "params: %v", err)
		}
	}
	if s.rec == nil || s.rec.w == nil {
		return ctl.TranscriptResult{}, ctl.Errf(ctl.CodeUnavailable, "session recording is off")
	}
	entries, _, err := session.Read(s.rec.w.Path()) // read-only: never Open, which repairs
	if err != nil {
		return ctl.TranscriptResult{}, ctl.Errf(ctl.CodeUnavailable, "reading transcript: %v", err)
	}
	head := s.rec.w.Head()
	branch := session.Branch(entries, head)
	if p.After != "" {
		i := slices.IndexFunc(branch, func(e session.Entry) bool { return e.ID == p.After })
		if i < 0 {
			return ctl.TranscriptResult{}, ctl.Errf(ctl.CodeBadRequest,
				"after %q is not on the current branch; re-read the transcript", p.After)
		}
		branch = branch[i+1:]
	}
	return ctl.TranscriptResult{Head: head, Entries: ctlEntries(branch)}, nil
}
