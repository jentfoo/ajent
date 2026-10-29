// Package client connects to a running ajent session's control socket: dial,
// discover live sessions, send requests and receive the event stream. It is
// the out-of-process half of the protocol in pkg/ctl, kept free of terminal
// and agent dependencies so an external controller can import it directly.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/session"
)

// Timeouts for one attach: connecting and reading the hello, then the budget
// any single request may take. The session answers snapshots from memory;
// anything slower is a wedged peer worth reporting rather than waiting on.
const (
	helloWait = 2 * time.Second

	// CallWait is the request budget a caller applies when it has none of its
	// own: ctx := context.WithTimeout(context.Background(), client.CallWait).
	CallWait = 3 * time.Second
)

// Conn is one controller's connection to a session: requests correlated by id,
// events delivered in arrival order to the handler Serve runs, writes
// serialized through the shared line encoder. Safe for concurrent use.
type Conn struct {
	conn  net.Conn
	dec   *ctl.Decoder
	enc   *ctl.Encoder
	hello ctl.Hello // what this session announced at connect

	mu      sync.Mutex
	nextID  int
	waiters map[string]chan ctl.Reply

	once    sync.Once
	done    chan struct{}
	deadErr error
}

// Dial connects to the unix socket at path and reads its hello, refusing a
// peer that speaks an incompatible protocol. The connection starts silent:
// Serve begins event delivery.
func Dial(path string) (*Conn, ctl.Hello, error) {
	conn, err := (&net.Dialer{Timeout: CallWait}).Dial("unix", path)
	if err != nil {
		// a hinted failure is the whole story (stale socket, wrong user); anything
		// else keeps the dial error's own detail
		if hint := dialHint(err); hint != "" {
			return nil, ctl.Hello{}, fmt.Errorf("cannot connect to %s: %s", path, hint)
		}
		return nil, ctl.Hello{}, fmt.Errorf("cannot connect to %s: %w", path, err)
	}
	c := &Conn{
		conn: conn, dec: ctl.NewDecoder(conn), enc: ctl.NewEncoder(conn),
		waiters: make(map[string]chan ctl.Reply), done: make(chan struct{}),
	}
	hello, err := c.readHello()
	if err != nil {
		_ = conn.Close()
		c.finish(err)
		return nil, ctl.Hello{}, err
	}
	if hello.Protocol != ctl.Protocol {
		_ = conn.Close()
		c.finish(nil)
		return nil, ctl.Hello{}, fmt.Errorf(
			"session %s speaks control protocol %d, this build speaks %d; update ajent",
			hello.Session.ID, hello.Protocol, ctl.Protocol)
	}
	c.hello = hello
	return c, hello, nil
}

// readHello reads the first line under a deadline, so an unresponsive peer
// cannot hold the dial open.
func (c *Conn) readHello() (ctl.Hello, error) {
	var hello ctl.Hello
	if err := c.conn.SetDeadline(time.Now().Add(helloWait)); err != nil {
		return hello, err
	}
	defer func() { _ = c.conn.SetDeadline(time.Time{}) }()

	var raw json.RawMessage
	if err := c.dec.Decode(&raw); err != nil {
		return hello, fmt.Errorf("reading control hello: %w", err)
	}
	in, err := ctl.DecodeIncoming(raw)
	if err != nil || in.Event == nil {
		return hello, errors.New("first control line was not a hello event")
	}
	if in.Event.Name == ctl.EventRefused { // identity check failed: the reason says what the kernel saw
		var rd ctl.RefusedData
		_ = in.Event.Decode(&rd)
		return hello, fmt.Errorf("session refused this connection: %s", rd.Reason)
	}
	if in.Event.Name != ctl.EventHello {
		return hello, errors.New("first control line was not a hello event")
	}
	if err := in.Event.Decode(&hello); err != nil {
		return hello, fmt.Errorf("decoding control hello: %w", err)
	}
	return hello, nil
}

// Serve decodes lines until the peer goes away or one turns to garbage,
// routing replies to waiters and events to handle on this goroutine. It must
// be running before any Call can complete: start it in its own goroutine right
// after Dial, before the first request flies.
func (c *Conn) Serve(handle func(ctl.Event)) {
	for {
		var raw json.RawMessage
		if err := c.dec.Decode(&raw); err != nil {
			c.finish(err)
			return
		}
		in, err := ctl.DecodeIncoming(raw)
		if err != nil {
			c.finish(err)
			return
		}
		if in.Reply != nil {
			c.deliver(*in.Reply)
			continue
		}
		if handle != nil {
			handle(*in.Event)
		}
	}
}

// deliver hands one reply to its waiter; a late reply whose caller gave up has
// no waiter and drops.
func (c *Conn) deliver(rp ctl.Reply) {
	c.mu.Lock()
	ch, ok := c.waiters[rp.ID]
	delete(c.waiters, rp.ID)
	c.mu.Unlock()
	if ok {
		ch <- rp
	}
}

// Call sends one request and waits for its reply. A refused request returns
// the session's wire error as *ctl.Error; a transport failure or an expired
// ctx reports through err with no seq. out, when non-nil, decodes the result.
func (c *Conn) Call(ctx context.Context, method string, params, out any) (uint64, error) {
	c.mu.Lock()
	c.nextID++
	id := strconv.Itoa(c.nextID)
	ch := make(chan ctl.Reply, 1)
	c.waiters[id] = ch
	c.mu.Unlock()

	var raw []byte
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			c.drop(id)
			return 0, err
		}
		raw = b
	}
	if err := c.enc.Encode(ctl.Request{ID: id, Method: method, Params: raw}); err != nil {
		c.drop(id)
		return 0, fmt.Errorf("sending %s: %w", method, err)
	}

	select {
	case rp := <-ch:
		if rp.Error != nil {
			return rp.Seq, rp.Error
		}
		if out != nil && len(rp.Result) > 0 {
			if err := json.Unmarshal(rp.Result, out); err != nil {
				return rp.Seq, fmt.Errorf("decoding %s result: %w", method, err)
			}
		}
		return rp.Seq, nil
	case <-ctx.Done():
		c.drop(id)
		return 0, ctx.Err()
	case <-c.done:
		return 0, errors.New("control connection closed")
	}
}

// drop removes one waiter whose caller stopped waiting.
func (c *Conn) drop(id string) {
	c.mu.Lock()
	delete(c.waiters, id)
	c.mu.Unlock()
}

// ClaimSeat claims the control seat: control names whether this connection
// may mutate the session. A held seat errors with code seat-held unless force.
func (c *Conn) ClaimSeat(ctx context.Context, holder string, force bool) (bool, error) {
	var res ctl.AttachResult
	_, err := c.Call(ctx, ctl.MethodAttach, ctl.AttachParams{Control: true, Holder: holder, Force: force}, &res)
	if err != nil {
		return false, err
	}
	return res.Control, nil
}

// State reads one snapshot of everything a controller polls; the second
// return is the watermark it was taken at.
func (c *Conn) State(ctx context.Context) (ctl.State, uint64, error) {
	var st ctl.State
	seq, err := c.Call(ctx, ctl.MethodState, nil, &st)
	return st, seq, err
}

// Hello returns the greeting this session announced at connect: identity,
// workspace, front-end shape and transcript path.
func (c *Conn) Hello() ctl.Hello { return c.hello }

// ReadBranch reads one recorded transcript file and returns its chain under
// head, oldest first. Reading tolerates a torn final line, so reading while
// the host appends is safe; entries landing after the branched head arrive as
// their own events.
func ReadBranch(path, head string) ([]session.Entry, error) {
	entries, _, err := session.Read(path)
	if err != nil {
		return nil, err
	}
	return session.Branch(entries, head), nil
}

// Backfill returns one controller's starting state: the snapshot with its
// watermark plus the transcript branch through the snapshot head, read from
// the file the hello named. Events past the watermark apply on top; an entry
// landing between the snapshot and the read may appear both ways, so
// consumers rendering appends dedupe by entry id.
func (c *Conn) Backfill(ctx context.Context) (ctl.State, []session.Entry, uint64, error) {
	snap, seq, err := c.State(ctx)
	if err != nil {
		return ctl.State{}, nil, 0, err
	}
	if c.hello.TranscriptPath == "" {
		return ctl.State{}, nil, 0, errors.New("session recording is off: no transcript to read")
	}
	branch, err := ReadBranch(c.hello.TranscriptPath, snap.Head)
	if err != nil {
		return ctl.State{}, nil, 0, fmt.Errorf("reading transcript: %w", err)
	}
	return snap, branch, seq, nil
}

// Submit routes one line exactly as typed in the session's editor.
func (c *Conn) Submit(ctx context.Context, line string) error {
	_, err := c.Call(ctx, ctl.MethodSubmit, ctl.SubmitParams{Line: line}, nil)
	return err
}

// Interrupt runs the Esc body remotely and names what stopped: turn, survey,
// shell or empty when nothing ran.
func (c *Conn) Interrupt(ctx context.Context) (string, error) {
	var res ctl.InterruptResult
	_, err := c.Call(ctx, ctl.MethodInterrupt, nil, &res)
	return res.Stopped, err
}

// ModeCycle steps the permission mode like Shift+Tab, backwards when back.
// The returned name is the mode now in effect, empty when no gate exists.
func (c *Conn) ModeCycle(ctx context.Context, back bool) (string, error) {
	var res ctl.ModeResult
	_, err := c.Call(ctx, ctl.MethodModeCycle, ctl.ModeCycleParams{Back: back}, &res)
	return res.Mode, err
}

// Replay requests the retained events past since, in seq order. A since that
// predates the ring answers *ctl.Error with CodeTooOld: re-snapshot instead.
func (c *Conn) Replay(ctx context.Context, since uint64) ([]ctl.Event, error) {
	var res ctl.ReplayResult
	_, err := c.Call(ctx, ctl.MethodReplay, ctl.ReplayParams{Since: since}, &res)
	return res.Events, err
}

// Rewind forks onto an earlier transcript entry and reports the new head.
func (c *Conn) Rewind(ctx context.Context, head string) (string, error) {
	var res ctl.RewindResult
	_, err := c.Call(ctx, ctl.MethodRewind, ctl.RewindParams{Head: head}, &res)
	return res.Head, err
}

// Resolve answers one pending interaction.
func (c *Conn) Resolve(ctx context.Context, params ctl.ResolveParams) error {
	_, err := c.Call(ctx, ctl.MethodResolve, params, nil)
	return err
}

// finish marks the connection dead exactly once.
func (c *Conn) finish(err error) {
	c.once.Do(func() {
		if err != nil {
			c.deadErr = err
		}
		close(c.done)
	})
}

// Done closes when Serve returned.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err names the read failure that ended the stream, nil on a clean close.
func (c *Conn) Err() error {
	c.once.Do(func() {}) // synchronize with finish's write
	return c.deadErr
}

// Close tears the connection down; Serve returns and waiters wake.
func (c *Conn) Close() { _ = c.conn.Close() }

// dialHint maps a refused or missing socket onto what it usually means.
func dialHint(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "no such socket"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "the socket has no live session behind it (stale?)"
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return "permission denied (the session belongs to another user)"
	default:
		return ""
	}
}
