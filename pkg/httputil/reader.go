package httputil

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

// ErrIdleTimeout is returned when a response stalled past the idle timeout.
var ErrIdleTimeout = errors.New("httputil: stream idle timeout")

// stopper cancels a pending timer.
type stopper interface{ Stop() bool }

// cancelReader releases the request context once the stream is closed.
type cancelReader struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (r *cancelReader) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.cancel)
	return err
}

// idleReader fails a stream that stops producing bytes for d.
type idleReader struct {
	rc        io.ReadCloser
	d         time.Duration
	afterFunc func(time.Duration, func()) stopper

	mu    sync.Mutex
	timer stopper
	fired bool
}

// Read resets the idle timer on progress, and reports ErrIdleTimeout when the
// stream stalled rather than the transport error closing it produced.
func (r *idleReader) Read(p []byte) (int, error) {
	r.arm()
	n, err := r.rc.Read(p)
	r.disarm()

	if n > 0 {
		return n, err
	} else if err != nil && r.didFire() {
		return n, ErrIdleTimeout
	}
	return n, err
}

// Close stops the timer and closes the underlying body.
func (r *idleReader) Close() error {
	r.disarm()
	return r.rc.Close()
}

func (r *idleReader) arm() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		return
	}
	r.timer = r.afterFunc(r.d, r.expire)
}

func (r *idleReader) disarm() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
}

// expire closes the body, which unblocks the pending Read.
func (r *idleReader) expire() {
	r.mu.Lock()
	r.fired = true
	r.mu.Unlock()
	_ = r.rc.Close()
}

func (r *idleReader) didFire() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fired
}

// sleepContext waits for d, or until ctx is done.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
