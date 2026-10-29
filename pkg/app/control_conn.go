package app

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/version"
)

// outgoing is one queued socket line: an event or a request reply, never both.
type outgoing struct {
	event *ctl.Event
	reply *ctl.Reply
}

func (o outgoing) encode(enc *ctl.Encoder) error {
	if o.event != nil {
		return enc.Encode(o.event)
	}
	return enc.Encode(o.reply)
}

// Stage bounds: the soft item count where a stalled writer is checked, and the
// hard item and byte caps where the stage drops outright. A healthy writer
// draining in batches keeps the stage near empty, so a burst that momentarily
// outruns its wake-ups is absorbed, while a peer that stopped reading backs
// the writer up until the soft bound plus the stall window trips.
const (
	stageSoftItems   = connQueue
	stageHardItems   = 4096
	stageHardBytes   = 8 << 20 // 8MB
	controlStallWait = 100 * time.Millisecond
	// controlWriteWait bounds one blocked flush: a peer whose buffers stay
	// full this long is gone, so the write fails and the connection drops.
	controlWriteWait = 5 * time.Second
)

// controlConn serves one observer: a reader goroutine dispatching requests and
// one writer goroutine draining an ordered stage through a buffered writer.
// Every line, event or reply, passes the stage in arrival order, so the socket
// carries one total order matching the hub's seq. A peer that stops reading
// backs the writer up until the stage bound trips and drops the connection;
// nothing on the session side ever blocks on it.
type controlConn struct {
	srv   *controlServer
	conn  net.Conn
	bw    *bufio.Writer
	enc   *ctl.Encoder
	done  chan struct{} // closed on detach: nothing further is accepted
	wrote chan struct{} // closed when the writer loop returns
	once  sync.Once
	lis   *evListener

	stageMu    sync.Mutex
	stage      []outgoing // ordered handoff to the writer
	stageBytes int
	lastWrite  atomic.Int64 // unix nano of the writer's last successful flush
	kick       chan struct{}
}

// newControlConn admits nc: delivery registers first so nothing published in
// between is missed, then the hello line lands before the writer starts, its
// seq the watermark the stream continues from. Callers follow with start.
func newControlConn(srv *controlServer, nc net.Conn) *controlConn {
	bw := bufio.NewWriterSize(nc, 4096)
	c := &controlConn{
		srv: srv, conn: nc, bw: bw, enc: ctl.NewEncoder(bw),
		done: make(chan struct{}), wrote: make(chan struct{}),
		kick: make(chan struct{}, 1),
	}
	var seq uint64
	c.lis, seq = srv.hub.evListen(c.deliver)
	c.lastWrite.Store(time.Now().UnixNano()) // the stall window starts at attach
	snap := srv.hub.Snapshot()
	_ = c.enc.Encode(ctl.NewEvent(ctl.EventHello, seq, ctl.Hello{
		Protocol:  ctl.Protocol,
		Session:   ctl.Session{ID: snap.SessionID, Name: snap.SessionName},
		Workspace: snap.Workspace, Cwd: snap.Cwd,
		Interactive: srv.hub.Interactive(), Serve: srv.serve,
		PID: os.Getpid(), Version: version.Version,
		TranscriptPath: srv.transcriptPath,
	}))
	_ = c.flushHello() // the hello must land before anything can queue behind it
	return c
}

// flushHello lands the first line under the write deadline, so a peer that
// already stalled drops here instead of wedging the writer.
func (c *controlConn) flushHello() error {
	_ = c.conn.SetWriteDeadline(time.Now().Add(controlWriteWait))
	err := c.bw.Flush()
	_ = c.conn.SetWriteDeadline(time.Time{})
	if err == nil {
		c.lastWrite.Store(time.Now().UnixNano())
	}
	return err
}

// start runs the connection's loops.
func (c *controlConn) start() {
	go c.readLoop()
	go c.writeLoop()
}

// deliver stages one event. It runs on the publishing goroutine, under the
// hub's lock, so it never blocks: a full stage drops the connection
// asynchronously.
func (c *controlConn) deliver(ev ctl.Event) {
	if !c.stageOutgoing(outgoing{event: &ev}) {
		go c.detach(false) // off the hub lock: detach unregisters under it
	}
}

// reply stages one request answer, encoding the result first so an
// unencodable value becomes a legible error, never a half-written line.
func (c *controlConn) reply(id string, seq uint64, result any, rerr *ctl.Error) {
	var raw []byte
	if rerr == nil && result != nil {
		b, err := json.Marshal(result)
		if err != nil {
			rerr = ctl.Errf(ctl.CodeUnavailable, "encoding result: %v", err)
		} else {
			raw = b
		}
	}
	rp := ctl.Reply{ID: id, Seq: seq, Result: raw, Error: rerr}
	if !c.stageOutgoing(outgoing{reply: &rp}) {
		go c.detach(false)
	}
}

// stageOutgoing appends one line to the ordered stage and kicks the writer,
// reporting false once the connection is detached or a bound tripped: the
// hard caps always drop, the soft item count drops only when the writer has
// not flushed for controlStallWait, which is the peer-stopped-reading shape.
func (c *controlConn) stageOutgoing(out outgoing) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	c.stageMu.Lock()
	defer c.stageMu.Unlock()
	size := out.size()
	if len(c.stage) >= stageHardItems || c.stageBytes+size > stageHardBytes {
		return false
	}
	if len(c.stage) >= stageSoftItems && c.stalledLocked() {
		return false
	}
	c.stage = append(c.stage, out)
	c.stageBytes += size
	select {
	case c.kick <- struct{}{}:
	default:
	}
	return true
}

// stalledLocked reports whether the writer has stopped making progress.
// Caller holds stageMu.
func (c *controlConn) stalledLocked() bool {
	last := c.lastWrite.Load()
	if last == 0 {
		return false // the writer has not flushed yet, not evidence of a stall
	}
	return time.Since(time.Unix(0, last)) > controlStallWait
}

// size approximates one line's encoded weight for the byte bound.
func (o outgoing) size() int {
	if o.event != nil {
		return len(o.event.Data) + 64
	}
	return len(o.reply.Result) + 64
}

// detach tears the connection down exactly once. flush keeps the socket open
// for a bounded drain of staged lines (session.exit); otherwise the peer is
// gone and the socket closes at once.
func (c *controlConn) detach(flush bool) {
	c.once.Do(func() {
		c.srv.hub.evUnlisten(c.lis)
		c.srv.remove(c)
		close(c.done)
		if !flush {
			_ = c.conn.Close()
			return
		}
		go func() {
			<-c.wrote // writer drained what remained
			_ = c.conn.Close()
		}()
	})
}

// writeControlRefusal sends one refused event in place of hello, so a peer the
// identity check turned away reads why instead of a bare EOF. Best effort: the
// close that follows is the authoritative signal.
func writeControlRefusal(nc net.Conn, reason error) {
	bw := bufio.NewWriterSize(nc, 512)
	_ = ctl.NewEncoder(bw).Encode(ctl.NewEvent(ctl.EventRefused, 0,
		ctl.RefusedData{Reason: reason.Error()}))
	_ = bw.Flush()
}

// readLoop decodes requests until the peer goes away or the line turns to
// garbage, dispatching each on this goroutine.
func (c *controlConn) readLoop() {
	dec := ctl.NewDecoder(c.conn)
	for {
		var req ctl.Request
		if err := dec.Decode(&req); err != nil {
			c.detach(false)
			return
		}
		c.dispatch(req)
	}
}

// writeLoop drains the stage to the socket, one flush per batch, so a burst
// publisher cannot outpace the syscall rate. After detach it flushes what
// remains for at most connDrainWait, then lets the socket close.
func (c *controlConn) writeLoop() {
	defer close(c.wrote)
	for {
		select {
		case <-c.kick:
			if err := c.drain(); err != nil {
				c.detach(false)
				return
			}
		case <-c.done:
			timer := time.NewTimer(connDrainWait)
			defer timer.Stop()
			for {
				select {
				case <-c.kick:
					if err := c.drain(); err != nil {
						return
					}
				case <-timer.C:
					_ = c.flushBuffer()
					return
				default:
					_ = c.flushBuffer()
					return
				}
			}
		}
	}
}

// drain encodes everything staged so far, one batch per pass. The write
// deadline spans the whole batch (encoding into the buffer can flush
// mid-line), and each pass stamps the progress the stall check reads. It
// reports the first write error.
func (c *controlConn) drain() error {
	for {
		c.stageMu.Lock()
		if len(c.stage) == 0 {
			c.stageMu.Unlock()
			return c.flushBuffer()
		}
		batch := c.stage
		c.stage = nil
		c.stageBytes = 0
		c.stageMu.Unlock()

		_ = c.conn.SetWriteDeadline(time.Now().Add(controlWriteWait))
		var err error
		for _, out := range batch {
			if err = out.encode(c.enc); err != nil {
				break
			}
		}
		if err == nil {
			err = c.bw.Flush()
		}
		_ = c.conn.SetWriteDeadline(time.Time{})
		if err != nil {
			return err
		}
		c.lastWrite.Store(time.Now().UnixNano())
	}
}

// flushBuffer flushes what the buffer already holds, under the deadline.
func (c *controlConn) flushBuffer() error {
	_ = c.conn.SetWriteDeadline(time.Now().Add(controlWriteWait))
	err := c.bw.Flush()
	_ = c.conn.SetWriteDeadline(time.Time{})
	if err == nil {
		c.lastWrite.Store(time.Now().UnixNano())
	}
	return err
}

// dispatch answers one request through the method registry. The watermark
// reads before anything runs, so events racing the reply stay above its seq;
// a seat refusal lands after it for the same reason.
func (c *controlConn) dispatch(req ctl.Request) {
	m, ok := controlMethods[req.Method]
	if !ok {
		c.reply(req.ID, c.srv.hub.evWatermark(), nil,
			ctl.Errf(ctl.CodeUnknownMethod, "unknown method %q", req.Method))
		return
	}
	seq := c.srv.hub.evWatermark()
	if m.seat {
		if rerr := c.srv.seatRefusal(c); rerr != nil {
			c.reply(req.ID, seq, nil, rerr)
			return
		}
	}
	result, rerr := m.handle(c, req.Params)
	c.reply(req.ID, seq, result, rerr)
}
