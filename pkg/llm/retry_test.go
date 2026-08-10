package llm

import (
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestBackoffDelay(t *testing.T) {
	t.Parallel()

	// no jitter keeps the arithmetic readable
	plain := RetryPolicy{Attempts: 5, Base: Duration(time.Second), Max: Duration(8 * time.Second), Jitter: 1}

	tests := []struct {
		name       string
		policy     RetryPolicy
		attempt    int
		retryAfter time.Duration
		rnd        float64
		expected   time.Duration
		ok         bool
	}{
		{"first_retry_is_base", plain, 1, 0, 0, time.Second, true},
		{"doubles_each_attempt", plain, 2, 0, 0, 2 * time.Second, true},
		{"third_attempt", plain, 3, 0, 0, 4 * time.Second, true},
		{"capped_at_max", plain, 4, 0, 0, 8 * time.Second, true},
		{"jitter_reduces_delay", plain, 1, 0, 0.5, 500 * time.Millisecond, true},
		{"retry_after_wins", plain, 1, 3 * time.Second, 0, 3 * time.Second, true},
		{"retry_after_ignores_jitter", plain, 2, 3 * time.Second, 0.9, 3 * time.Second, true},
		{"absurd_retry_after_gives_up", plain, 1, 24 * time.Hour, 0, 0, false},
		{"attempts_exhausted", plain, 5, 0, 0, 0, false},
		{"defaults_applied", RetryPolicy{Jitter: 1}, 1, 0, 0, defaultBase, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := backoffDelay(tc.policy, tc.attempt, tc.retryAfter, tc.rnd)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.expected, got)
		})
	}
}

func TestShouldRetryStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		status        int
		hasRetryAfter bool
		expected      bool
	}{
		{"request_timeout", http.StatusRequestTimeout, false, true},
		{"too_many_requests", http.StatusTooManyRequests, false, true},
		{"too_early", http.StatusTooEarly, false, true},
		{"internal_error", http.StatusInternalServerError, false, true},
		{"bad_gateway", http.StatusBadGateway, false, true},
		{"unavailable", http.StatusServiceUnavailable, false, true},
		{"gateway_timeout", http.StatusGatewayTimeout, false, true},
		{"conflict_with_retry_after", http.StatusConflict, true, true},
		{"conflict_without_retry_after", http.StatusConflict, false, false},
		{"bad_request", http.StatusBadRequest, false, false},
		{"unauthorized", http.StatusUnauthorized, false, false},
		{"forbidden", http.StatusForbidden, false, false},
		{"not_found", http.StatusNotFound, false, false},
		{"unprocessable", http.StatusUnprocessableEntity, false, false},
		{"payment_required", http.StatusPaymentRequired, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, shouldRetryStatus(tc.status, tc.hasRetryAfter))
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		header   string
		expected time.Duration
	}{
		{"empty", "", 0},
		{"delta_seconds", "5", 5 * time.Second},
		{"zero_seconds", "0", 0},
		{"negative_ignored", "-3", 0},
		{"http_date_future", now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{"http_date_past", now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"garbage", "soon", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, parseRetryAfter(tc.header, now))
		})
	}
}

func TestRetryableConnErr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"nil", nil, false},
		{"net_error", &net.DNSError{IsTimeout: true}, true},
		{"closed", net.ErrClosed, true},
		{"plain", errors.New("boom"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, retryableConnErr(tc.err))
		})
	}
}
