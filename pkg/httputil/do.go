package httputil

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// ErrorFunc builds the caller's error for a non-2xx response, and reports
// whether the attempt should be retried.
type ErrorFunc func(status int, body []byte, retryAfter time.Duration) (error, bool)

// Request describes one call. Headers win over the User-Agent and JSON
// Content-Type Do sets, so a caller can override either.
type Request struct {
	Method    string
	URL       string
	Body      []byte
	Headers   map[string]string
	Name      string // caller supplied label for log events
	UserAgent string
	Error     ErrorFunc // nil builds a plain *Error

	Timeouts Timeouts
	Retry    RetryPolicy
	Log      func(LogEvent)

	hooks callHooks // injection seams, replaced in tests
}

// callHooks holds the test-injectable seams Do otherwise defaults.
type callHooks struct {
	now       func() time.Time
	sleep     func(ctx context.Context, d time.Duration) error
	afterFunc func(d time.Duration, f func()) stopper
	rand      func() float64
}

// Error is the non-2xx response a caller that supplied no ErrorFunc gets back.
type Error struct {
	Status     int
	Body       []byte
	RetryAfter time.Duration
}

func (e *Error) Error() string { return "httputil: http status " + strconv.Itoa(e.Status) }

// retryableError marks an attempt failure the retry loop should repeat.
type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

func isRetryableAttempt(err error) bool {
	var re *retryableError
	return errors.As(err, &re)
}

// unwrapAttempt strips the retry marker so callers see the provider error.
func unwrapAttempt(err error) error {
	var re *retryableError
	if errors.As(err, &re) {
		return re.err
	}
	return err
}

// Do performs one request with retries against hc, returning a response whose
// headers have arrived and whose status is 2xx, or 304 for a conditional get.
//
// Every retry happens here, before any body byte is read, so no caller can
// re-emit deltas that were already delivered.
func Do(ctx context.Context, hc *http.Client, r Request) (*http.Response, error) {
	h := r.hooks
	if h.now == nil {
		h.now = time.Now
	}
	if h.sleep == nil {
		h.sleep = sleepContext
	}
	if h.afterFunc == nil {
		h.afterFunc = func(d time.Duration, f func()) stopper { return time.AfterFunc(d, f) }
	}
	if h.rand == nil {
		h.rand = rand.Float64
	}

	// a total timeout has to outlive this call, since the stream is read after
	// it returns, so its cancel hangs off the response body instead
	cancel := func() {}
	if total := durOr(r.Timeouts.Total, 0); total > 0 {
		ctx, cancel = context.WithTimeout(ctx, total)
	}

	for attemptNum := 1; ; attemptNum++ {
		resp, retryAfter, err := doAttempt(ctx, hc, r, h, attemptNum)
		if err == nil {
			resp.Body = &cancelReader{ReadCloser: resp.Body, cancel: cancel}
			return resp, nil
		}
		if ctx.Err() != nil {
			cancel()
			return nil, ctx.Err()
		} else if !isRetryableAttempt(err) {
			cancel()
			return nil, unwrapAttempt(err)
		}
		delay, ok := backoffDelay(r.Retry, attemptNum, retryAfter, h.rand())
		if !ok {
			cancel()
			return nil, unwrapAttempt(err)
		} else if serr := h.sleep(ctx, delay); serr != nil {
			cancel()
			return nil, serr
		}
	}
}

// doAttempt performs a single request, reporting any Retry-After the server sent.
func doAttempt(ctx context.Context, hc *http.Client, r Request, h callHooks, attemptNum int) (*http.Response, time.Duration, error) {
	req, err := newRequest(ctx, r)
	if err != nil {
		return nil, 0, err
	}

	start := h.now()
	resp, err := hc.Do(req)
	if err != nil {
		emit(r.Log, LogEvent{Name: r.Name, Method: r.Method, URL: redactURL(req.URL),
			Header: redactHeaders(req.Header), Attempt: attemptNum,
			Duration: h.now().Sub(start), Err: err})
		if retryableConnErr(err) {
			return nil, 0, &retryableError{err: err}
		}
		return nil, 0, err
	}

	emit(r.Log, LogEvent{Name: r.Name, Method: r.Method, URL: redactURL(req.URL),
		Header: redactHeaders(req.Header), Status: resp.StatusCode, Attempt: attemptNum,
		Duration: h.now().Sub(start)})

	// 304 counts as success so a conditional discovery refetch is not an error
	if (resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == http.StatusNotModified {
		resp.Body = wrapIdle(resp.Body, r.Timeouts.Idle, h.afterFunc)
		return resp, 0, nil
	}

	errBody := readErrorBody(resp)
	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), h.now())
	var errOut error
	var retryable bool
	if r.Error == nil {
		errOut = &Error{Status: resp.StatusCode, Body: errBody, RetryAfter: retryAfter}
		retryable = ShouldRetryStatus(resp.StatusCode, retryAfter > 0)
	} else {
		e, ret := r.Error(resp.StatusCode, errBody, retryAfter)
		if e == nil {
			e = &Error{Status: resp.StatusCode, Body: errBody, RetryAfter: retryAfter}
		}
		errOut, retryable = e, ret
	}
	if retryable {
		return nil, retryAfter, &retryableError{err: errOut}
	}
	return nil, retryAfter, errOut
}

// wrapIdle applies the idle timeout to a response body when one is configured.
func wrapIdle(rc io.ReadCloser, idle *time.Duration, afterFunc func(time.Duration, func()) stopper) io.ReadCloser {
	d := durOr(idle, defaultIdleTimeout)
	if d <= 0 {
		return rc // explicitly disabled
	}
	return &idleReader{rc: rc, d: d, afterFunc: afterFunc}
}

func newRequest(ctx context.Context, r Request) (*http.Request, error) {
	var rdr io.Reader
	if r.Body != nil {
		rdr = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, r.URL, rdr)
	if err != nil {
		return nil, err
	}
	if r.UserAgent != "" {
		req.Header.Set("User-Agent", r.UserAgent)
	}
	if r.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

func emit(log func(LogEvent), ev LogEvent) {
	if log != nil {
		log(ev)
	}
}
