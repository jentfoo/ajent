package httputil

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTimer is a stopper whose callback the test fires by hand.
type fakeTimer struct {
	fn      func()
	stopped bool
}

func (t *fakeTimer) Stop() bool { t.stopped = true; return true }

func TestIdleReaderRead(t *testing.T) {
	t.Parallel()

	t.Run("reports_idle_timeout_when_it_fires", func(t *testing.T) {
		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pw.Close() })

		// the channel hands the armed timer to the firing goroutine, so the
		// expiry is ordered against Read with no polling
		armed := make(chan *fakeTimer, 1)
		r := &idleReader{rc: pr, d: time.Minute, afterFunc: func(_ time.Duration, fn func()) stopper {
			ft := &fakeTimer{fn: fn}
			armed <- ft
			return ft
		}}

		go func() {
			(<-armed).fn() // expire, which closes the body and unblocks the read
		}()

		_, err := r.Read(make([]byte, 8))
		assert.ErrorIs(t, err, ErrIdleTimeout)
	})
	t.Run("progress_stops_the_timer", func(t *testing.T) {
		var timer *fakeTimer
		r := &idleReader{rc: io.NopCloser(strings.NewReader("hello")), d: time.Minute,
			afterFunc: func(_ time.Duration, fn func()) stopper {
				timer = &fakeTimer{fn: fn}
				return timer
			}}

		n, err := r.Read(make([]byte, 8))
		require.NoError(t, err)
		assert.Equal(t, 5, n)
		assert.True(t, timer.stopped)
	})
	t.Run("normal_eof_is_not_an_idle_timeout", func(t *testing.T) {
		r := &idleReader{rc: io.NopCloser(strings.NewReader("")), d: time.Minute,
			afterFunc: func(_ time.Duration, fn func()) stopper { return &fakeTimer{fn: fn} }}

		_, err := r.Read(make([]byte, 8))
		assert.ErrorIs(t, err, io.EOF)
	})
}

func TestCancelReaderClose(t *testing.T) {
	t.Parallel()

	t.Run("close_cancels_once", func(t *testing.T) {
		var calls int
		r := &cancelReader{
			ReadCloser: io.NopCloser(strings.NewReader("x")),
			cancel:     func() { calls++ },
		}
		require.NoError(t, r.Close())
		require.NoError(t, r.Close())
		assert.Equal(t, 1, calls)
	})
}

func TestSleepContext(t *testing.T) {
	t.Parallel()

	t.Run("zero_returns_immediately", func(t *testing.T) {
		assert.NoError(t, sleepContext(t.Context(), 0))
	})
	t.Run("cancelled_context_reports_the_cause", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		assert.ErrorIs(t, sleepContext(ctx, time.Hour), context.Canceled)
	})
}
