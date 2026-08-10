package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAPIKey = "sk-secret-do-not-leak"

// fakeTimer is a stopper whose callback the test fires by hand.
type fakeTimer struct {
	fn      func()
	stopped bool
}

func (t *fakeTimer) Stop() bool { t.stopped = true; return true }

// testClient builds a client against srv with deterministic clock and jitter.
func testClient(t *testing.T, url string, opts ...func(*clientOptions)) (*httpClient, *[]time.Duration, *[]HTTPLogEvent) {
	t.Helper()

	var logs []HTTPLogEvent
	var mu sync.Mutex
	o := clientOptions{
		provider: "testprov",
		baseURL:  url,
		headers:  map[string]string{"Authorization": "Bearer " + testAPIKey},
		retry:    RetryPolicy{Attempts: 4, Base: Duration(time.Second), Max: Duration(30 * time.Second), Jitter: 1},
		log: func(ev HTTPLogEvent) {
			mu.Lock()
			defer mu.Unlock()
			logs = append(logs, ev)
		},
	}
	for _, fn := range opts {
		fn(&o)
	}
	c, err := newHTTPClient(o)
	require.NoError(t, err)

	var slept []time.Duration
	c.rand = func() float64 { return 0 }
	c.now = func() time.Time { return time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC) }
	c.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	return c, &slept, &logs
}

func TestHTTPClientDo(t *testing.T) {
	t.Parallel()

	t.Run("succeeds_without_retry", func(t *testing.T) {
		var hits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits++
			_, _ = io.WriteString(w, "ok")
		}))
		t.Cleanup(srv.Close)

		c, slept, _ := testClient(t, srv.URL)
		resp, err := c.do(t.Context(), httpReq{method: http.MethodPost, path: "/v1/x", body: []byte(`{}`)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })

		assert.Equal(t, 1, hits)
		assert.Empty(t, *slept)
	})
	t.Run("honours_retry_after_header", func(t *testing.T) {
		var hits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits++
			if hits == 1 {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = io.WriteString(w, "ok")
		}))
		t.Cleanup(srv.Close)

		c, slept, _ := testClient(t, srv.URL)
		resp, err := c.do(t.Context(), httpReq{method: http.MethodPost, path: "/v1/x", body: []byte(`{}`)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })

		assert.Equal(t, 2, hits)
		assert.Equal(t, []time.Duration{2 * time.Second}, *slept)
	})
	t.Run("exponential_backoff_until_attempts_exhausted", func(t *testing.T) {
		var hits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits++
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)

		c, slept, _ := testClient(t, srv.URL)
		_, err := c.do(t.Context(), httpReq{method: http.MethodPost, path: "/v1/x", body: []byte(`{}`)})

		var ae *APIError
		require.ErrorAs(t, err, &ae)
		assert.Equal(t, http.StatusInternalServerError, ae.Status)
		assert.Equal(t, 4, hits)
		assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}, *slept)
	})
	t.Run("client_error_is_not_retried", func(t *testing.T) {
		var hits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits++
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"bad"}`)
		}))
		t.Cleanup(srv.Close)

		c, slept, _ := testClient(t, srv.URL)
		_, err := c.do(t.Context(), httpReq{method: http.MethodPost, path: "/v1/x", body: []byte(`{}`)})

		var ae *APIError
		require.ErrorAs(t, err, &ae)
		assert.Equal(t, 1, hits)
		assert.Empty(t, *slept)
		assert.Contains(t, ae.Message, "bad")
	})
	t.Run("absurd_retry_after_fails_immediately", func(t *testing.T) {
		var hits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits++
			w.Header().Set("Retry-After", "86400")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		t.Cleanup(srv.Close)

		c, slept, _ := testClient(t, srv.URL)
		_, err := c.do(t.Context(), httpReq{method: http.MethodPost, path: "/v1/x", body: nil})

		require.Error(t, err)
		assert.Equal(t, 1, hits)
		assert.Empty(t, *slept)
	})
	t.Run("classifier_maps_overflow", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "prompt is too long")
		}))
		t.Cleanup(srv.Close)

		classify := func(status int, body []byte) error {
			if status == http.StatusBadRequest && strings.Contains(string(body), "too long") {
				return (&APIError{Provider: "testprov", Status: status}).Overflow()
			}
			return nil
		}
		c, _, _ := testClient(t, srv.URL)
		_, err := c.do(t.Context(), httpReq{method: http.MethodPost, path: "/v1/x", body: nil, classify: classify})
		assert.True(t, IsOverflow(err))
	})
	t.Run("cancelled_context_stops_retrying", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(srv.Close)

		ctx, cancel := context.WithCancel(t.Context())
		c, _, _ := testClient(t, srv.URL)
		c.sleep = func(context.Context, time.Duration) error {
			cancel()
			return context.Canceled
		}
		_, err := c.do(ctx, httpReq{method: http.MethodPost, path: "/v1/x"})
		assert.ErrorIs(t, err, context.Canceled)
	})
	t.Run("sets_json_content_type_with_body", func(t *testing.T) {
		var got string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Get("Content-Type")
			_, _ = io.WriteString(w, "ok")
		}))
		t.Cleanup(srv.Close)

		c, _, _ := testClient(t, srv.URL)
		resp, err := c.do(t.Context(), httpReq{method: http.MethodPost, path: "/v1/x", body: []byte(`{}`)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		assert.Equal(t, "application/json", got)
	})
}

func TestHTTPClientRedaction(t *testing.T) {
	t.Parallel()

	t.Run("key_never_reaches_the_log_or_error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"bad request"}`)
		}))
		t.Cleanup(srv.Close)

		c, _, logs := testClient(t, srv.URL)
		_, err := c.do(t.Context(), httpReq{method: http.MethodPost, path: "/v1/x?api_key=" + testAPIKey})
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
}

func TestRedactHeaders(t *testing.T) {
	t.Parallel()

	t.Run("masks_every_credential_header", func(t *testing.T) {
		h := http.Header{}
		for _, k := range redactedHeaders {
			h.Set(k, testAPIKey)
		}
		h.Set("Content-Type", "application/json")

		got := redactHeaders(h)
		for _, k := range redactedHeaders {
			assert.Equal(t, redactedMask, got.Get(k), k)
		}
		assert.Equal(t, "application/json", got.Get("Content-Type"))
	})
	t.Run("does_not_mutate_the_original", func(t *testing.T) {
		h := http.Header{}
		h.Set("Authorization", testAPIKey)
		redactHeaders(h)
		assert.Equal(t, testAPIKey, h.Get("Authorization"))
	})
	t.Run("absent_header_not_added", func(t *testing.T) {
		got := redactHeaders(http.Header{})
		assert.Empty(t, got.Get("Authorization"))
	})
}

func TestRedactURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		raw      string
		contains string
	}{
		{"api_key_param", "https://x.test/v1?api_key=" + testAPIKey, redactedQuery},
		{"key_param", "https://x.test/v1?key=" + testAPIKey, redactedQuery},
		{"access_token_param", "https://x.test/v1?access_token=" + testAPIKey, redactedQuery},
		{"unrelated_param_kept", "https://x.test/v1?model=opus", "model=opus"},
		{"no_query", "https://x.test/v1", "https://x.test/v1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.raw)
			require.NoError(t, err)

			got := redactURL(u)
			assert.Contains(t, got, tc.contains)
			assert.NotContains(t, got, testAPIKey)
		})
	}
}

func TestResolveKey(t *testing.T) {
	t.Parallel()

	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	t.Run("configured_env_var_wins", func(t *testing.T) {
		got, err := resolveKey("p", "literal", "MY_KEY", "DEFAULT_KEY",
			env(map[string]string{"MY_KEY": "from-env", "DEFAULT_KEY": "wrong"}))
		require.NoError(t, err)
		assert.Equal(t, "from-env", got)
	})
	t.Run("literal_when_env_empty", func(t *testing.T) {
		got, err := resolveKey("p", "literal", "MY_KEY", "DEFAULT_KEY", env(nil))
		require.NoError(t, err)
		assert.Equal(t, "literal", got)
	})
	t.Run("dialect_default_env_last", func(t *testing.T) {
		got, err := resolveKey("p", "", "", "DEFAULT_KEY",
			env(map[string]string{"DEFAULT_KEY": "conventional"}))
		require.NoError(t, err)
		assert.Equal(t, "conventional", got)
	})
	t.Run("missing_names_the_variable", func(t *testing.T) {
		_, err := resolveKey("anthropic", "", "", "ANTHROPIC_API_KEY", env(nil))

		var ne *ErrNoAPIKey
		require.ErrorAs(t, err, &ne)
		assert.Equal(t, "ANTHROPIC_API_KEY", ne.EnvVar)
		assert.Contains(t, err.Error(), "ANTHROPIC_API_KEY")
	})
}

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

func TestHTTPClientWrapIdle(t *testing.T) {
	t.Parallel()

	body := func() io.ReadCloser { return io.NopCloser(strings.NewReader("x")) }

	t.Run("unset_uses_the_default", func(t *testing.T) {
		c := &httpClient{afterFunc: func(time.Duration, func()) stopper { return &fakeTimer{} }}
		assert.IsType(t, &idleReader{}, c.wrapIdle(body()))
	})
	t.Run("explicit_zero_disables", func(t *testing.T) {
		c := &httpClient{timeouts: Timeouts{Idle: dur(0)}}
		_, wrapped := c.wrapIdle(body()).(*idleReader)
		assert.False(t, wrapped)
	})
	t.Run("explicit_value_wraps", func(t *testing.T) {
		c := &httpClient{timeouts: Timeouts{Idle: dur(time.Second)},
			afterFunc: func(time.Duration, func()) stopper { return &fakeTimer{} }}
		assert.IsType(t, &idleReader{}, c.wrapIdle(body()))
	})
}
