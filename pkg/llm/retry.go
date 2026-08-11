package llm

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"
)

const (
	defaultAttempts = 4
	defaultBase     = 500 * time.Millisecond
	defaultMax      = 30 * time.Second
	defaultJitter   = 0.3
	// cap on an honoured Retry-After; beyond it the request fails
	maxRetryAfter = time.Minute
)

// RetryPolicy bounds automatic retries. Zero fields take the defaults.
type RetryPolicy struct {
	Attempts int      `json:"attempts,omitempty"` // total attempts including the first
	Base     Duration `json:"base,omitzero"`
	Max      Duration `json:"max,omitzero"`
	Jitter   float64  `json:"jitter,omitempty"` // fraction of the delay, 0 to 1
}

// withDefaults returns the policy with unset fields filled in.
func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.Attempts <= 0 {
		p.Attempts = defaultAttempts
	}
	if p.Base <= 0 {
		p.Base = Duration(defaultBase)
	}
	if p.Max <= 0 {
		p.Max = Duration(defaultMax)
	}
	if p.Jitter <= 0 || p.Jitter > 1 {
		p.Jitter = defaultJitter
	}
	return p
}

// backoffDelay returns how long to wait after the given one based attempt
// failed, and whether waiting is worthwhile at all. A server supplied
// retryAfter wins, unless it is longer than we are willing to stall for. rnd is
// a jitter sample in [0,1).
func backoffDelay(p RetryPolicy, attempt int, retryAfter time.Duration, rnd float64) (time.Duration, bool) {
	p = p.withDefaults()
	if attempt >= p.Attempts {
		return 0, false
	} else if retryAfter > 0 {
		if retryAfter > maxRetryAfter {
			return 0, false
		}
		return retryAfter, true
	}

	d := time.Duration(p.Base)
	for range attempt - 1 {
		d *= 2
		if d >= time.Duration(p.Max) {
			d = time.Duration(p.Max)
			break
		}
	}
	return time.Duration(float64(d) * (1 - p.Jitter*rnd)), true
}

// shouldRetryStatus reports whether an HTTP status is worth another attempt.
// 409 only counts when the server asked us to wait, which some gateways use to
// mean the model is still loading.
func shouldRetryStatus(status int, hasRetryAfter bool) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusTooEarly, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	case http.StatusConflict:
		return hasRetryAfter
	default:
		return false
	}
}

// retryableConnErr reports whether a transport error is worth another attempt.
// Only errors from before any response body was read reach here.
func retryableConnErr(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	return errors.Is(err, net.ErrClosed)
}

// parseRetryAfter reads a Retry-After header in either the delta seconds or the
// HTTP date form, returning zero when absent or unparseable.
func parseRetryAfter(h string, now time.Time) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs < 0 {
			return 0
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
