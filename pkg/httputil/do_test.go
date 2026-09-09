package httputil

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAPIKey = "sk-secret-do-not-leak"

// testRequest builds a call with a deterministic clock, jitter and recorded
// sleeps. The returned slices are filled as Do runs.
func testRequest(t *testing.T, srvURL string) (Request, *http.Client, *[]time.Duration, *[]LogEvent) {
	t.Helper()

	var logs []LogEvent
	var mu sync.Mutex
	var slept []time.Duration

	r := Request{
		Method:  http.MethodPost,
		URL:     srvURL + "/v1/x",
		Body:    []byte(`{}`),
		Name:    "testprov",
		Headers: map[string]string{"Authorization": "Bearer " + testAPIKey},
		Retry:   RetryPolicy{Attempts: 4, Base: time.Second, Max: 30 * time.Second, Jitter: 1},
		Log: func(ev LogEvent) {
			mu.Lock()
			defer mu.Unlock()
			logs = append(logs, ev)
		},
	}
	r.hooks.rand = func() float64 { return 0 }
	r.hooks.now = func() time.Time { return time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC) }
	r.hooks.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	return r, New(Options{}), &slept, &logs
}

// countingServer answers with handler and counts the requests it received.
func countingServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		handler(w, req)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestDo(t *testing.T) {
	t.Parallel()

	t.Run("succeeds_without_retry", func(t *testing.T) {
		srv, hits := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "ok")
		})

		r, hc, slept, logs := testRequest(t, srv.URL)
		resp, err := Do(t.Context(), hc, r)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })

		assert.Equal(t, int64(1), hits.Load())
		assert.Empty(t, *slept)
		assert.Len(t, *logs, 1)
	})
	t.Run("honours_retry_after_header", func(t *testing.T) {
		var served atomic.Int64
		srv, hits := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
			if served.Add(1) == 1 {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = io.WriteString(w, "ok")
		})

		r, hc, slept, _ := testRequest(t, srv.URL)
		resp, err := Do(t.Context(), hc, r)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })

		assert.Equal(t, int64(2), hits.Load())
		assert.Equal(t, []time.Duration{2 * time.Second}, *slept)
	})
	t.Run("exponential_backoff_until_attempts_exhausted", func(t *testing.T) {
		srv, hits := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})

		r, hc, slept, _ := testRequest(t, srv.URL)
		_, err := Do(t.Context(), hc, r)

		var he *Error
		require.ErrorAs(t, err, &he)
		assert.Equal(t, http.StatusInternalServerError, he.Status)
		assert.Equal(t, int64(4), hits.Load())
		assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}, *slept)
	})
	t.Run("client_error_is_not_retried", func(t *testing.T) {
		srv, hits := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"bad"}`)
		})

		r, hc, slept, _ := testRequest(t, srv.URL)
		_, err := Do(t.Context(), hc, r)

		var he *Error
		require.ErrorAs(t, err, &he)
		assert.Equal(t, int64(1), hits.Load())
		assert.Empty(t, *slept)
		assert.Contains(t, string(he.Body), "bad")
	})
	t.Run("absurd_retry_after_fails_immediately", func(t *testing.T) {
		srv, hits := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "86400")
			w.WriteHeader(http.StatusTooManyRequests)
		})

		r, hc, slept, _ := testRequest(t, srv.URL)
		_, err := Do(t.Context(), hc, r)

		require.Error(t, err)
		assert.Equal(t, int64(1), hits.Load())
		assert.Empty(t, *slept)
	})
	t.Run("error_func_replaces_the_error", func(t *testing.T) {
		srv, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "prompt is too long")
		})

		sentinel := context.Canceled // any caller error stands in for a classified one
		r, hc, _, _ := testRequest(t, srv.URL)
		r.Error = func(status int, body []byte, _ time.Duration) (error, bool) {
			assert.Equal(t, http.StatusBadRequest, status)
			assert.Contains(t, string(body), "too long")
			return sentinel, false
		}
		_, err := Do(t.Context(), hc, r)
		assert.ErrorIs(t, err, sentinel)
	})
	t.Run("error_func_drives_retry", func(t *testing.T) {
		srv, hits := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest) // not retryable by status alone
		})

		r, hc, slept, _ := testRequest(t, srv.URL)
		r.Error = func(int, []byte, time.Duration) (error, bool) { return context.Canceled, true }
		_, err := Do(t.Context(), hc, r)

		require.Error(t, err)
		assert.Equal(t, int64(4), hits.Load())
		assert.Len(t, *slept, 3)
	})
	t.Run("cancelled_context_stops_retrying", func(t *testing.T) {
		srv, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		})

		ctx, cancel := context.WithCancel(t.Context())
		r, hc, _, _ := testRequest(t, srv.URL)
		r.hooks.sleep = func(context.Context, time.Duration) error {
			cancel()
			return context.Canceled
		}
		_, err := Do(ctx, hc, r)
		assert.ErrorIs(t, err, context.Canceled)
	})
	t.Run("sets_json_content_type_with_body", func(t *testing.T) {
		var got string
		srv, _ := countingServer(t, func(w http.ResponseWriter, req *http.Request) {
			got = req.Header.Get("Content-Type")
			_, _ = io.WriteString(w, "ok")
		})

		r, hc, _, _ := testRequest(t, srv.URL)
		resp, err := Do(t.Context(), hc, r)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		assert.Equal(t, "application/json", got)
	})
	t.Run("not_modified_counts_as_success", func(t *testing.T) {
		srv, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotModified)
		})

		r, hc, _, _ := testRequest(t, srv.URL)
		r.Method, r.Body = http.MethodGet, nil
		resp, err := Do(t.Context(), hc, r)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		assert.Equal(t, http.StatusNotModified, resp.StatusCode)
	})
	t.Run("user_agent_sent_on_every_attempt", func(t *testing.T) {
		var mu sync.Mutex
		var got []string
		srv, _ := countingServer(t, func(w http.ResponseWriter, req *http.Request) {
			mu.Lock()
			got = append(got, req.Header.Get("User-Agent"))
			mu.Unlock()
			w.WriteHeader(http.StatusBadGateway)
		})

		r, hc, _, _ := testRequest(t, srv.URL)
		r.UserAgent = "ajent/v0.4.2"
		_, err := Do(t.Context(), hc, r)
		require.Error(t, err)

		require.Len(t, got, 4)
		for _, ua := range got {
			assert.Equal(t, "ajent/v0.4.2", ua)
		}
	})
	t.Run("request_header_overrides_user_agent", func(t *testing.T) {
		var got string
		srv, _ := countingServer(t, func(w http.ResponseWriter, req *http.Request) {
			got = req.Header.Get("User-Agent")
			_, _ = io.WriteString(w, "ok")
		})

		r, hc, _, _ := testRequest(t, srv.URL)
		r.UserAgent = "ajent/v0.4.2"
		r.Headers["User-Agent"] = "custom/1.0"
		resp, err := Do(t.Context(), hc, r)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		assert.Equal(t, "custom/1.0", got)
	})
	t.Run("redirect_is_not_followed", func(t *testing.T) {
		var target atomic.Int64
		mux := http.NewServeMux()
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		mux.HandleFunc("/target", func(w http.ResponseWriter, _ *http.Request) {
			target.Add(1)
			_, _ = io.WriteString(w, "ok")
		})
		mux.HandleFunc("/v1/x", func(w http.ResponseWriter, _ *http.Request) {
			http.Redirect(w, &http.Request{}, srv.URL+"/target", http.StatusFound)
		})

		r, hc, _, _ := testRequest(t, srv.URL)
		_, err := Do(t.Context(), hc, r)

		var he *Error
		require.ErrorAs(t, err, &he)
		assert.Equal(t, http.StatusFound, he.Status)
		assert.Zero(t, target.Load()) // a credential bearing request is never replayed
	})
	t.Run("credentials_never_reach_the_log_or_error", func(t *testing.T) {
		srv, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"bad request"}`)
		})

		r, hc, _, logs := testRequest(t, srv.URL)
		r.URL = srv.URL + "/v1/x?api_key=" + testAPIKey
		_, err := Do(t.Context(), hc, r)
		require.Error(t, err)

		assert.NotContains(t, err.Error(), testAPIKey)
		require.NotEmpty(t, *logs)
		for _, ev := range *logs {
			assert.NotContains(t, ev.URL, testAPIKey)
			assert.Equal(t, redactedMask, ev.Header.Get("Authorization"))
			for k, vs := range ev.Header {
				for _, v := range vs {
					assert.NotContains(t, v, testAPIKey, k)
				}
			}
		}
	})
	t.Run("total_timeout_outlives_the_call", func(t *testing.T) {
		srv, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "streamed")
		})

		r, hc, _, _ := testRequest(t, srv.URL)
		r.Timeouts.Total = dur(time.Minute)
		resp, err := Do(t.Context(), hc, r)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })

		body, err := io.ReadAll(resp.Body) // the cancel hangs off the body, not Do
		require.NoError(t, err)
		assert.Equal(t, "streamed", string(body))
	})
}

func TestWrapIdle(t *testing.T) {
	t.Parallel()

	body := func() io.ReadCloser { return io.NopCloser(strings.NewReader("x")) }
	timer := func(time.Duration, func()) stopper { return &fakeTimer{} }

	t.Run("unset_uses_the_default", func(t *testing.T) {
		assert.IsType(t, &idleReader{}, wrapIdle(body(), nil, timer))
	})
	t.Run("explicit_zero_disables", func(t *testing.T) {
		_, wrapped := wrapIdle(body(), dur(0), timer).(*idleReader)
		assert.False(t, wrapped)
	})
	t.Run("explicit_value_wraps", func(t *testing.T) {
		assert.IsType(t, &idleReader{}, wrapIdle(body(), dur(time.Second), timer))
	})
}
