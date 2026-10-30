package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/jentfoo/ajent/pkg/paths"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tui"
)

const (
	// controlSocketMax is the sun_path budget the enable path guards: linux
	// allows 108 bytes, darwin 104, and 100 leaves room for the terminator.
	controlSocketMax = 100

	// tokenPrefixLen is how much of a writer token names one instance's files:
	// short enough for the socket budget, taken from the ULID's random tail so
	// tokens minted in the same millisecond still name different instances.
	tokenPrefixLen = 8

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

	// peerNoticeKey keys the shared-session join and leave notices, so repeated
	// joins replace rather than stack.
	peerNoticeKey = "peers"

	// gcStaleRegistry is how old a registry file must be before its socket is
	// dial-probed for collection, so a starting instance's files are never
	// judged by a racer.
	gcStaleRegistry = time.Minute

	// gcDial bounds one liveness probe during collection.
	gcDial = 250 * time.Millisecond

	// acceptBackoff is the retry delay after a temporary accept failure, so a
	// transient fd exhaustion cannot become a live-looking zombie.
	acceptBackoff = 25 * time.Millisecond
)

// controlLayout derives one instance's files under the control directory:
// <control>/<sessionID>/<token8>.sock plus its <token8>.json registry entry.
// The token is the writer slot token, minted before any entry could append, so
// every serving instance has one and a process only ever removes paths
// carrying its own token. The control directory is created and pinned to 0700,
// with a warning naming any wider mode found there.
func controlLayout(sessionID, token string) (dir, sock, reg string, warn string, err error) {
	base, warn, err := paths.SubDir(paths.ControlDirName)
	if err != nil {
		return "", "", "", "", err
	}
	dir = filepath.Join(base, sessionID)
	if err := os.MkdirAll(dir, paths.DirPerm); err != nil {
		return "", "", "", "", err
	}
	prefix := tokenSuffix(token)
	sock = filepath.Join(dir, prefix+".sock")
	reg = filepath.Join(dir, prefix+".json")
	if len(sock) >= controlSocketMax {
		return "", "", "", "", fmt.Errorf(
			"control socket path %s is %d bytes, over the %d-byte unix socket limit; move the ajent home closer to the filesystem root",
			sock, len(sock), controlSocketMax)
	}
	return dir, sock, reg, warn, nil
}

// tokenSuffix takes a token's trailing chars, the random half of its ULID, so
// simultaneous writers never share one instance's file names.
func tokenSuffix(token string) string {
	if len(token) > tokenPrefixLen {
		return token[len(token)-tokenPrefixLen:]
	}
	return token
}

// bindControlListener binds one instance's socket. There is no probe: sockets
// are per instance, so a file already at the path is a dead leftover of this
// token's own previous life and is unlinked before rebinding. A non-socket
// file is never removed.
func bindControlListener(ctx context.Context, path string) (net.Listener, error) {
	if fi, err := os.Stat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("control socket path %s exists and is not a socket; move it aside", path)
		}
		_ = os.Remove(path) // this token's own dead socket from a SIGKILL: safe to rebind
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
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

// controlServer serves one session's control socket: any number of read-only
// observers, each with an independently bounded outbound stage. Delivery never
// blocks the session; a slow or gone consumer is dropped. Sockets are per
// instance and named for this process's writer token, so two ajent processes
// serving one session coexist and no path removal can cross instances.
type controlServer struct {
	hub     *sessionHub
	dialogs *interactionBroker
	rec     *sessRec // transcript source for backfill; nil when recording is off
	path    string
	serve   bool // serve mode: the socket is the only front end
	// transcriptPath locates the recorded transcript for hello, empty when
	// recording is off: controllers backfill from this file, not over the wire
	transcriptPath string
	// instance identity: this writer's token and the files it owns. Removal is
	// always by these paths, so a newer instance's socket is untouchable. started
	// is stamped once, so registry rewrites never drift the announced start time.
	token   string
	regPath string
	started int64 // unix millis

	ln         net.Listener
	mu         sync.Mutex
	conns      map[*controlConn]struct{}
	closed     bool
	seat       *controlConn     // the connection holding the session's single control seat
	seatHolder string           // its announced name, for refusal errors naming the holder
	exec       *controlExecutor // the lane every mutating verb runs on
}

// controlRegistry is the discovery record beside the socket, written after
// listen so a reader never sees a socket with no entry. Rewritten by atomic
// rename on seat changes and session renames, so pickers stay current without
// dialing. The shape is ctl's shared wire contract; additive fields only.
type controlRegistry = ctl.Instance

// startControl binds the session's control socket and starts serving it,
// attaching the control observers first so every observable change from here
// on fans out. Returned warnings are startup-level (a tightened directory
// mode), meant for the same surface as a failed start. Connections are served
// only once start runs, after driver wiring completes; Close unlinks this
// instance's files, callers run it on every return path.
func startControl(ctx context.Context, hub *sessionHub, dialogs *interactionBroker, rec *sessRec, serve bool) (*controlServer, []string, error) {
	snap := hub.Snapshot()
	if snap.SessionID == "" || rec == nil || rec.w == nil {
		return nil, nil, errors.New("control needs a recorded session; none is open")
	}
	token := rec.w.Token()
	_, sock, reg, warn, err := controlLayout(snap.SessionID, token)
	if err != nil {
		return nil, nil, err
	}
	ln, err := bindControlListener(ctx, sock)
	if errors.Is(err, syscall.ENOENT) {
		// the GC removed the session directory between our mkdir and the bind,
		// seeing it empty: recreate it and retry once
		if mkErr := os.MkdirAll(filepath.Dir(sock), paths.DirPerm); mkErr == nil {
			ln, err = bindControlListener(ctx, sock)
		}
	}
	if err != nil {
		return nil, nil, err
	}
	srv := &controlServer{
		hub: hub, dialogs: dialogs, rec: rec, path: sock, serve: serve,
		ln: ln, conns: make(map[*controlConn]struct{}),
		token: token, regPath: reg, started: time.Now().UnixMilli(),
		exec: newControlExecutor(ctx),
	}
	if rec != nil && rec.w != nil {
		srv.transcriptPath = rec.w.Path()
	}
	srv.writeRegistry()
	// registry rewrites ride the hub's rename seam, so pickers learn renames
	// without dialing
	hub.renameObs = srv.writeRegistry
	var warns []string
	if warn != "" {
		warns = append(warns, warn)
	}
	// one passive sweep: this start collects peers that died since the last
	// one, async so startup never waits on foreign dial timeouts
	go srv.collectStale()

	// observe everything before serving: stream events, rendered surfaces,
	// dialogs and transcript appends, each republished as a seq'd event.
	// The control server owns the emitter set today; a second consumer would
	// compose its own layer on top.
	hub.Register(&ctlStreamSink{hub: hub})
	dialogs.observe(controlDialogHooks(hub))
	if rec != nil {
		rec.onAppend = srv.entryAppended
		rec.rec.WatchAppend(srv.entryAppended)
	}
	return srv, warns, nil
}

// writeRegistry publishes this instance's discovery record: who serves, where,
// and whether the seat is held. Best effort: a failed write only hides this
// instance from pickers, the socket still serves.
func (s *controlServer) writeRegistry() {
	snap := s.hub.Snapshot()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return // Close removed the files: a late rewrite would resurrect them
	}
	held := s.seat != nil
	started := s.started
	s.mu.Unlock()
	reg := controlRegistry{
		SessionID:   snap.SessionID,
		SessionName: snap.SessionName,
		Workspace:   snap.Workspace,
		Model:       snap.Model.Key,
		StartedAt:   started,
		PID:         os.Getpid(),
		Token:       s.token,
		Socket:      s.path,
		SeatHeld:    held,
	}
	b, err := json.Marshal(reg)
	if err != nil {
		return
	}
	_ = config.WriteFileAtomic(s.regPath, b, 0o600)
}

// collectStale removes registry and socket files whose instance died: the
// socket refuses a connect and the registry is old enough that its writer had
// time to finish starting. Empty per-session directories go with them. The
// sweep runs once per start, skipping own records since probing this
// instance's own socket would ring its accept log for nothing.
func (s *controlServer) collectStale() {
	base, _, err := paths.SubDir(paths.ControlDirName)
	if err != nil {
		return
	}
	sessions, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, dir := range sessions {
		if !dir.IsDir() {
			continue
		}
		dirPath := filepath.Join(base, dir.Name())
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
				continue
			}
			regPath := filepath.Join(base, dir.Name(), e.Name())
			fi, err := os.Stat(regPath)
			if err != nil || time.Since(fi.ModTime()) < gcStaleRegistry {
				continue // fresh: its writer may still be binding
			}
			var reg controlRegistry
			b, rerr := os.ReadFile(regPath)
			if rerr != nil || json.Unmarshal(b, &reg) != nil || reg.Socket == "" {
				continue
			}
			if reg.Token == s.token {
				continue // own record: known alive, and probing would ring our accept log
			}
			c, derr := (&net.Dialer{Timeout: gcDial}).Dial("unix", reg.Socket)
			if derr == nil {
				_ = c.Close()
				continue // alive: another instance is serving
			}
			if !deadDial(derr) {
				continue // inconclusive (fd exhaustion, a full backlog): judge it later
			}
			_ = os.Remove(regPath)
			_ = os.Remove(reg.Socket)
		}
		// an empty session directory is clutter once every registry died off
		if left, lerr := os.ReadDir(dirPath); lerr == nil && len(left) == 0 {
			_ = os.Remove(dirPath)
		}
	}
}

// deadDial reports whether a failed probe proves the socket dead: refused, or
// the file already gone. Anything else (a timeout, fd exhaustion) leaves the
// files for a later sweep rather than unlinking a live peer on weak evidence.
func deadDial(err error) bool {
	var oe *net.OpError
	if !errors.As(err, &oe) {
		return false
	}
	return errors.Is(oe.Err, syscall.ECONNREFUSED) || errors.Is(oe.Err, os.ErrNotExist)
}

// start begins serving connections and running the executor lane. Called after
// driver wiring completes so no request reads a half-built hub: the bind
// happened earlier in startControl, and dials since then wait in the accept
// backlog.
func (s *controlServer) start() {
	s.exec.start()
	go s.acceptLoop()
}

// acceptLoop serves connections until Close retires the listener. Temporary
// failures (fd exhaustion, aborted connects) log and back off instead of
// killing the loop, so a transient storm cannot produce a live-looking zombie.
func (s *controlServer) acceptLoop() {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			// temporary failures leave the listener alive: per-process or
			// per-system fd exhaustion, and a peer that vanished between connect
			// and accept (errors.Is unwraps net.OpError)
			if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) ||
				errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.ENOBUFS) {
				s.logControl(tui.LevelWarn, "accept retry: "+err.Error())
				time.Sleep(acceptBackoff)
				continue
			}
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
		msg = fmt.Sprintf("control connection from pid %d", peer.PID)
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

// track admits one accepted socket as a connection. The greeting flush runs
// before the serving set is touched: its 5s write bound must never delay
// Close, seat operations, or other accepts.
func (s *controlServer) track(nc net.Conn) {
	c := newControlConn(s, nc)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		c.detach(false)
		return
	}
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	c.start()
}

// remove drops a connection from the serving set, freeing the control seat
// with it: death releases immediately and a new controller attaches at once.
// The registry follows, so pickers see the seat open without dialing.
func (s *controlServer) remove(c *controlConn) {
	s.mu.Lock()
	delete(s.conns, c)
	wasSeat := s.seat == c
	if wasSeat {
		s.seat = nil
		s.seatHolder = ""
	}
	s.mu.Unlock()
	if wasSeat {
		s.writeRegistry()
	}
}

// Close stops accepting, announces session.exit to every connection, flushes
// it a bounded while, then unlinks this instance's own socket and registry.
// Only paths carrying this token are ever removed, so a newer instance's
// files are untouchable. Idempotent.
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
	sock := s.path
	reg := s.regPath
	s.mu.Unlock()

	_ = s.ln.Close()
	s.hub.publish(ctl.EventSessionExit, ctl.SessionExitData{Reason: reason})
	for _, c := range conns {
		c.detach(true)
	}
	_ = os.Remove(sock)
	_ = os.Remove(reg)
}

// entryAppended announces one transcript entry written after attach. Wired
// onto the recorder wrapper success path, never fired for resume-time reads.
// The head note rides along so a later rewind's branch.changed carries the real
// pre-switch head.
func (s *controlServer) entryAppended(e session.Entry) {
	s.hub.noteAppend(e.ID)
	s.hub.publish(ctl.EventEntryAppend, ctl.EntryData{Entry: ctlEntry(e)})
}
