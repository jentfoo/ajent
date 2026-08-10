package llm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultConnectTimeout = 10 * time.Second
	defaultTLSTimeout     = 10 * time.Second
	defaultHeaderTimeout  = 60 * time.Second
	defaultIdleTimeout    = 5 * time.Minute
	// errBodyLimit bounds how much of an error body is kept for the debug log.
	errBodyLimit = 2 << 10
	redactedMask = "[redacted]"
	// redactedQuery avoids the brackets, which url encoding would mangle into
	// something unreadable in a debug log
	redactedQuery = "redacted"
)

// Timeouts bound one request. An unset field takes the dialect default; an
// explicit "0s" disables that bound, which is what a lm-studio endpoint needs
// while it loads a model just in time.
type Timeouts struct {
	Connect *Duration `json:"connect,omitempty"`
	TLS     *Duration `json:"tls,omitempty"`
	Header  *Duration `json:"header,omitempty"` // connect to the first response header
	Idle    *Duration `json:"idle,omitempty"`   // gap between response body reads
	Total   *Duration `json:"total,omitempty"`  // whole call including the stream
}

// HTTPLogEvent is one request attempt, with credentials already removed.
type HTTPLogEvent struct {
	Provider string
	Method   string
	URL      string
	Header   http.Header
	Status   int
	Attempt  int
	Duration time.Duration
	Err      error
}

// httpClient is the shared transport every provider adapter runs on.
type httpClient struct {
	provider string
	base     *url.URL
	headers  map[string]string
	timeouts Timeouts
	retry    RetryPolicy
	hc       *http.Client

	// injection seams, replaced in tests
	now       func() time.Time
	sleep     func(ctx context.Context, d time.Duration) error
	afterFunc func(d time.Duration, f func()) stopper
	rand      func() float64
	log       func(HTTPLogEvent)
}

// stopper cancels a pending timer.
type stopper interface{ Stop() bool }

// clientOptions configures an httpClient.
type clientOptions struct {
	provider  string
	baseURL   string
	headers   map[string]string
	timeouts  Timeouts
	retry     RetryPolicy
	transport http.RoundTripper
	log       func(HTTPLogEvent)
}

// newHTTPClient builds a client for one provider endpoint.
func newHTTPClient(opts clientOptions) (*httpClient, error) {
	base, err := url.Parse(strings.TrimSuffix(opts.baseURL, "/"))
	if err != nil {
		return nil, err
	}

	tr := opts.transport
	if tr == nil {
		tr = &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: durOr(opts.timeouts.Connect, defaultConnectTimeout),
			}).DialContext,
			TLSHandshakeTimeout:   durOr(opts.timeouts.TLS, defaultTLSTimeout),
			ResponseHeaderTimeout: durOr(opts.timeouts.Header, defaultHeaderTimeout),
			ForceAttemptHTTP2:     true,
		}
	}
	return &httpClient{
		provider:  opts.provider,
		base:      base,
		headers:   opts.headers,
		timeouts:  opts.timeouts,
		retry:     opts.retry,
		hc:        &http.Client{Transport: tr}, // no Timeout, it would kill a long stream
		now:       time.Now,
		sleep:     sleepContext,
		afterFunc: func(d time.Duration, f func()) stopper { return time.AfterFunc(d, f) },
		rand:      rand.Float64,
		log:       opts.log,
	}, nil
}

// httpReq describes one call. Per call headers merge over the client's, so a
// conditional get never mutates shared state.
type httpReq struct {
	method   string
	path     string
	body     []byte
	headers  map[string]string
	classify func(status int, body []byte) error
}

// do performs one request with retries, returning a response whose headers have
// arrived and whose status is 2xx.
//
// Every retry happens here, before any body byte is read, so no caller can
// re-emit deltas that were already delivered.
func (c *httpClient) do(ctx context.Context, r httpReq) (*http.Response, error) {
	// a total timeout has to outlive this call, since the stream is read after
	// it returns, so its cancel hangs off the response body instead
	cancel := func() {}
	if total := durOr(c.timeouts.Total, 0); total > 0 {
		ctx, cancel = context.WithTimeout(ctx, total)
	}

	for attempt := 1; ; attempt++ {
		resp, retryAfter, err := c.attempt(ctx, r, attempt)
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
		delay, ok := backoffDelay(c.retry, attempt, retryAfter, c.rand())
		if !ok {
			cancel()
			return nil, unwrapAttempt(err)
		} else if serr := c.sleep(ctx, delay); serr != nil {
			cancel()
			return nil, serr
		}
	}
}

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

// attempt performs a single request, reporting any Retry-After the server sent.
func (c *httpClient) attempt(ctx context.Context, r httpReq, attempt int) (*http.Response, time.Duration, error) {
	req, err := c.newRequest(ctx, r)
	if err != nil {
		return nil, 0, err
	}

	start := c.now()
	resp, err := c.hc.Do(req)
	if err != nil {
		c.emit(HTTPLogEvent{Provider: c.provider, Method: r.method, URL: redactURL(req.URL),
			Header: redactHeaders(req.Header), Attempt: attempt, Duration: c.now().Sub(start), Err: err})
		if retryableConnErr(err) {
			return nil, 0, &retryableError{err: err}
		}
		return nil, 0, err
	}

	c.emit(HTTPLogEvent{Provider: c.provider, Method: r.method, URL: redactURL(req.URL),
		Header: redactHeaders(req.Header), Status: resp.StatusCode, Attempt: attempt,
		Duration: c.now().Sub(start)})

	// 304 counts as success so a conditional discovery refetch is not an error
	if (resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == http.StatusNotModified {
		resp.Body = c.wrapIdle(resp.Body)
		return resp, 0, nil
	}

	errBody := readErrorBody(resp)
	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), c.now())
	apiErr := c.apiError(resp.StatusCode, errBody, retryAfter, r.classify)
	if apiErr.Retryable {
		return nil, retryAfter, &retryableError{err: apiErr}
	}
	return nil, retryAfter, apiErr
}

// apiError builds the error for a non 2xx response, letting the adapter's
// classifier refine it.
func (c *httpClient) apiError(status int, body []byte, retryAfter time.Duration, classify func(int, []byte) error) *APIError {
	if classify != nil {
		if err := classify(status, body); err != nil {
			var ae *APIError
			if errors.As(err, &ae) {
				return ae
			}
			return &APIError{Provider: c.provider, Status: status, Message: err.Error(), Body: body}
		}
	}
	return &APIError{
		Provider:   c.provider,
		Status:     status,
		Message:    strings.TrimSpace(string(body)),
		Retryable:  shouldRetryStatus(status, retryAfter > 0),
		RetryAfter: retryAfter,
		Body:       body,
	}
}

// newRequest builds a request against the client's base URL.
func (c *httpClient) newRequest(ctx context.Context, r httpReq) (*http.Request, error) {
	var rdr io.Reader
	if r.body != nil {
		rdr = bytes.NewReader(r.body)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, c.base.String()+r.path, rdr)
	if err != nil {
		return nil, err
	}
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// wrapIdle applies the idle timeout to a response body when one is configured.
func (c *httpClient) wrapIdle(rc io.ReadCloser) io.ReadCloser {
	d := durOr(c.timeouts.Idle, defaultIdleTimeout)
	if d <= 0 {
		return rc // explicitly disabled
	}
	return &idleReader{rc: rc, d: d, afterFunc: c.afterFunc}
}

func (c *httpClient) emit(ev HTTPLogEvent) {
	if c.log != nil {
		c.log(ev)
	}
}

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

// readErrorBody reads a bounded, scrubbed copy of an error response body.
func readErrorBody(resp *http.Response) []byte {
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
	return body
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

// credential headers, matched case insensitively by http.Header canonicalization
var redactedHeaders = []string{
	"Authorization", "X-Api-Key", "Api-Key", "Proxy-Authorization",
	"Cookie", "Set-Cookie", "X-Goog-Api-Key", "Openai-Organization",
}

// redactHeaders returns a copy with credential headers masked.
func redactHeaders(h http.Header) http.Header {
	out := h.Clone()
	if out == nil {
		return nil
	}
	for _, k := range redactedHeaders {
		if out.Get(k) != "" {
			out.Set(k, redactedMask)
		}
	}
	return out
}

// redactURL returns u with credential query parameters masked.
func redactURL(u *url.URL) string {
	q := u.Query()
	var dirty bool
	for _, k := range []string{"key", "api_key", "access_token"} {
		if q.Has(k) {
			q.Set(k, redactedQuery)
			dirty = true
		}
	}
	if !dirty {
		return u.String()
	}
	c := *u
	c.RawQuery = q.Encode()
	return c.String()
}

// resolveKey returns the API key for a provider. The configured environment
// variable wins, then a literal key, then the dialect's conventional variable.
func resolveKey(provider, literal, envVar, defaultEnv string, env func(string) string) (string, error) {
	if envVar != "" {
		if v := env(envVar); v != "" {
			return v, nil
		}
	}
	if literal != "" {
		return literal, nil
	}
	if defaultEnv != "" {
		if v := env(defaultEnv); v != "" {
			return v, nil
		}
	}
	name := envVar
	if name == "" {
		name = defaultEnv
	}
	return "", &ErrNoAPIKey{Provider: provider, EnvVar: name}
}
