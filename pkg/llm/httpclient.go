package llm

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jentfoo/ajent/pkg/httputil"
	"github.com/jentfoo/ajent/pkg/version"
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

// bounds returns the transport form of the configured timeouts.
func (t Timeouts) bounds() httputil.Timeouts {
	return httputil.Timeouts{
		Connect: stdDur(t.Connect),
		TLS:     stdDur(t.TLS),
		Header:  stdDur(t.Header),
		Idle:    stdDur(t.Idle),
		Total:   stdDur(t.Total),
	}
}

// HTTPLogEvent is one request attempt, with credentials already removed.
type HTTPLogEvent = httputil.LogEvent

// httpClient is the shared transport every provider adapter runs on.
type httpClient struct {
	provider string
	base     *url.URL
	headers  map[string]string
	timeouts httputil.Timeouts
	retry    httputil.RetryPolicy
	hc       *http.Client
	log      func(HTTPLogEvent)
}

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
	bounds := opts.timeouts.bounds()
	return &httpClient{
		provider: opts.provider,
		base:     base,
		headers:  opts.headers,
		timeouts: bounds,
		retry:    opts.retry.bounds(),
		hc:       httputil.New(httputil.Options{Timeouts: bounds, Transport: opts.transport}),
		log:      opts.log,
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
func (c *httpClient) do(ctx context.Context, r httpReq) (*http.Response, error) {
	headers := c.headers
	if len(r.headers) > 0 {
		headers = make(map[string]string, len(c.headers)+len(r.headers))
		maps.Copy(headers, c.headers)
		maps.Copy(headers, r.headers)
	}
	return httputil.Do(ctx, c.hc, httputil.Request{
		Method:    r.method,
		URL:       c.base.String() + r.path,
		Body:      r.body,
		Headers:   headers,
		Name:      c.provider,
		UserAgent: version.UserAgent(),
		Error:     c.errorFunc(r.classify),
		Timeouts:  c.timeouts,
		Retry:     c.retry,
		Log:       c.log,
	})
}

// errorFunc adapts apiError to the seam the transport calls on a non 2xx status.
func (c *httpClient) errorFunc(classify func(int, []byte) error) httputil.ErrorFunc {
	return func(status int, body []byte, retryAfter time.Duration) (error, bool) {
		e := c.apiError(status, body, retryAfter, classify)
		return e, e.Retryable
	}
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
		Retryable:  httputil.ShouldRetryStatus(status, retryAfter > 0),
		RetryAfter: retryAfter,
		Body:       body,
	}
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
