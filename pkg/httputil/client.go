// Package httputil holds the hardened HTTP client every outbound call runs on.
package httputil

import (
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	defaultConnectTimeout = 10 * time.Second
	defaultTLSTimeout     = 10 * time.Second
	defaultHeaderTimeout  = 60 * time.Second
	// defaultIdleTimeout bounds the gap between response body reads
	defaultIdleTimeout = 10 * time.Minute
	// pooledConnTimeout drops an unused keep-alive connection from the pool
	pooledConnTimeout          = 90 * time.Second
	defaultMaxIdleConns        = 64
	defaultMaxIdleConnsPerHost = 8
)

// Timeouts bound one request. A nil field takes the package default; an
// explicit zero disables that bound.
type Timeouts struct {
	Connect, TLS, Header, Idle, Total *time.Duration
}

// LogEvent is one request attempt, with credentials already removed.
type LogEvent struct {
	Name     string // caller supplied label
	Method   string
	URL      string
	Header   http.Header
	Status   int
	Attempt  int
	Duration time.Duration
	Err      error
}

// Options configures the client New returns. Only bounds the transport itself
// enforces belong here; the rest of a call is described by Request.
type Options struct {
	Timeouts  Timeouts
	Transport http.RoundTripper // test seam; bypasses the shared client cache
}

var clientCache sync.Map // map[transportKey]*http.Client

// transportKey is the resolved bounds that decide whether a pool can be shared.
// A dialer cannot carry two timeouts, so any provider configuring its own splits
// off a transport; the sharing is real for the defaults every cloud provider takes.
type transportKey struct {
	connect, tls, header time.Duration
}

// New returns a hardened *http.Client for one set of timeout bounds. Pass it to
// Do, which carries the per-call URL, headers and user agent.
func New(opts Options) *http.Client {
	if opts.Transport != nil {
		return newClient(opts.Transport)
	}
	k := transportKey{
		connect: durOr(opts.Timeouts.Connect, defaultConnectTimeout),
		tls:     durOr(opts.Timeouts.TLS, defaultTLSTimeout),
		header:  durOr(opts.Timeouts.Header, defaultHeaderTimeout),
	}
	if v, ok := clientCache.Load(k); ok {
		return v.(*http.Client)
	}
	v, _ := clientCache.LoadOrStore(k, newClient(buildTransport(k)))
	return v.(*http.Client)
}

func newClient(tr http.RoundTripper) *http.Client {
	return &http.Client{
		Transport: tr,
		// an API client never needs a redirect, and following one would replay
		// a credential bearing request against a host we did not choose
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	} // no Timeout, it would kill a long stream
}

func buildTransport(k transportKey) *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment, // honours HTTP_PROXY/HTTPS_PROXY/NO_PROXY
		DialContext:           (&net.Dialer{Timeout: k.connect}).DialContext,
		TLSHandshakeTimeout:   k.tls,
		ResponseHeaderTimeout: k.header,
		ForceAttemptHTTP2:     true,
		IdleConnTimeout:       pooledConnTimeout,
		MaxIdleConns:          defaultMaxIdleConns,
		MaxIdleConnsPerHost:   defaultMaxIdleConnsPerHost,
	}
}

// durOr returns d as a duration, or alt when unset. An explicit zero is
// honoured so a caller can disable a bound the default would enable.
func durOr(d *time.Duration, alt time.Duration) time.Duration {
	if d == nil {
		return alt
	}
	return *d
}
