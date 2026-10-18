package httputil

import (
	"errors"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	defaultAttempts = 4
	defaultBase     = 500 * time.Millisecond
	defaultMax      = 30 * time.Second
	defaultJitter   = 0.3
	// cap on an honoured Retry-After, beyond it the request fails
	maxRetryAfter = time.Minute
)

// StatusOverloaded is Anthropic's non-standard overload status, also used by
// some gateways.
const StatusOverloaded = 529

// RetryPolicy bounds automatic retries. Zero fields take the defaults.
type RetryPolicy struct {
	Attempts int
	Base     time.Duration
	Max      time.Duration
	Jitter   float64 // fraction of the delay, 0 to 1
}

// BackoffDelay returns how long to wait after the given one based attempt failed, and whether
// waiting is worthwhile at all. A server supplied retryAfter wins, unless it is longer than we
// are willing to stall for. rnd is a jitter sample in [0,1).
func BackoffDelay(p RetryPolicy, attempt int, retryAfter time.Duration, rnd float64) (time.Duration, bool) {
	if p.Attempts <= 0 {
		p.Attempts = defaultAttempts
	}
	if p.Base <= 0 {
		p.Base = defaultBase
	}
	if p.Max <= 0 {
		p.Max = defaultMax
	}
	if p.Jitter <= 0 || p.Jitter > 1 {
		p.Jitter = defaultJitter
	}
	if attempt >= p.Attempts {
		return 0, false
	} else if retryAfter > 0 {
		if retryAfter > maxRetryAfter {
			return 0, false
		}
		return retryAfter, true
	}

	d := p.Base
	for range attempt - 1 {
		d *= 2
		if d >= p.Max {
			d = p.Max
			break
		}
	}
	return time.Duration(float64(d) * (1 - p.Jitter*rnd)), true
}

// ShouldRetryStatus reports whether an HTTP status is worth another attempt.
// 409 only counts when the server asked us to wait, which some gateways use to
// mean the model is still loading. Callers building their own error from an
// ErrorFunc use this to decide the retryable flag, as do mid-stream error
// frames once their code resolves to a status.
func ShouldRetryStatus(status int, hasRetryAfter bool) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusTooEarly, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout,
		StatusOverloaded:
		return true
	case http.StatusConflict:
		return hasRetryAfter
	default:
		return false
	}
}

// ShouldRetryTransport reports whether a request level transport error is worth
// another attempt. Only errors from before any response body was read reach here.
// Timeouts and broken connections retry; unsupported schemes or TLS failures fail
// fast.
func ShouldRetryTransport(err error) bool {
	if err == nil {
		return false
	}
	var ue *url.Error
	// peel url.Error layers before matching; the wrapper satisfies net.Error,
	// so left in place it always matches whatever its cause is
	for errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, net.ErrClosed)
}

// maxDeltaSeconds is the largest seconds count that still scales into a duration.
const maxDeltaSeconds = math.MaxInt64 / int64(time.Second)

// retryAfterForever saturates any larger wait: positive so presence checks stay
// truthful, past every cap BackoffDelay honours.
const retryAfterForever = time.Duration(math.MaxInt64)

// parseRetryAfter reads a Retry-After header in either the delta seconds or the
// HTTP date form, returning zero when absent or unparseable. A wait too large
// to represent saturates to retryAfterForever rather than wrapping negative.
func parseRetryAfter(h string, now time.Time) time.Duration {
	if h == "" {
		return 0
	}
	// explicit width parses huge deltas identically on every platform, where Atoi
	// would reject them. An out-of-range literal arrives clamped to the int64
	// bounds and folds into the saturate branch below.
	if secs, err := strconv.ParseInt(h, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		if secs <= 0 {
			return 0
		}
		if secs > maxDeltaSeconds {
			return retryAfterForever
		}
		return time.Duration(secs) * time.Second
	}
	t, err := http.ParseTime(h)
	if err != nil {
		return 0
	}
	if d := t.Sub(now); d > 0 {
		return d
	}
	return 0
}
