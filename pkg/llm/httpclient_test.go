package llm

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/version"
)

const testAPIKey = "sk-secret-do-not-leak"

// testClient builds a client against url with a recording log hook.
func testClient(t *testing.T, url string) *httpClient {
	t.Helper()

	c, err := newHTTPClient(clientOptions{
		provider: "testprov",
		baseURL:  url,
		headers:  map[string]string{"Authorization": "Bearer " + testAPIKey},
	})
	require.NoError(t, err)
	return c
}

func TestHTTPClientAPIError(t *testing.T) {
	t.Parallel()

	c := &httpClient{provider: "testprov"}

	t.Run("status_drives_retryable", func(t *testing.T) {
		e := c.apiError(http.StatusServiceUnavailable, []byte("down"), 0, nil)
		assert.Equal(t, "testprov", e.Provider)
		assert.Equal(t, "down", e.Message)
		assert.True(t, e.Retryable)
	})
	t.Run("client_error_is_not_retryable", func(t *testing.T) {
		e := c.apiError(http.StatusBadRequest, []byte("nope"), 0, nil)
		assert.False(t, e.Retryable)
	})
	t.Run("retry_after_makes_conflict_retryable", func(t *testing.T) {
		e := c.apiError(http.StatusConflict, nil, 2*time.Second, nil)
		assert.True(t, e.Retryable)
		assert.Equal(t, 2*time.Second, e.RetryAfter)
	})
	t.Run("classifier_result_wins", func(t *testing.T) {
		classify := func(status int, _ []byte) error {
			return (&APIError{Provider: "testprov", Status: status}).Overflow()
		}
		e := c.apiError(http.StatusBadRequest, []byte("prompt is too long"), 0, classify)
		assert.True(t, IsOverflow(e))
	})
	t.Run("plain_classifier_error_is_wrapped", func(t *testing.T) {
		classify := func(int, []byte) error { return errors.New("boom") }
		e := c.apiError(http.StatusBadRequest, []byte("body"), 0, classify)
		assert.Equal(t, "boom", e.Message)
		assert.False(t, e.Retryable)
	})
	t.Run("nil_classifier_result_falls_through", func(t *testing.T) {
		classify := func(int, []byte) error { return nil }
		e := c.apiError(http.StatusInternalServerError, []byte("oops"), 0, classify)
		assert.Equal(t, "oops", e.Message)
		assert.True(t, e.Retryable)
	})
}

func TestHTTPClientErrorFunc(t *testing.T) {
	t.Parallel()

	c := &httpClient{provider: "testprov"}

	t.Run("reports_retryable_to_the_transport", func(t *testing.T) {
		err, retryable := c.errorFunc(nil)(http.StatusBadGateway, []byte("up"), 0)
		require.Error(t, err)
		assert.True(t, retryable)
	})
	t.Run("classified_overflow_is_not_retried", func(t *testing.T) {
		classify := func(status int, _ []byte) error {
			return (&APIError{Provider: "testprov", Status: status}).Overflow()
		}
		err, retryable := c.errorFunc(classify)(http.StatusBadRequest, nil, 0)
		assert.True(t, IsOverflow(err))
		assert.False(t, retryable)
	})
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

func TestHTTPClientDo(t *testing.T) {
	t.Parallel()

	t.Run("sends_the_ajent_user_agent", func(t *testing.T) {
		var ua, auth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ua, auth = r.Header.Get("User-Agent"), r.Header.Get("Authorization")
			_, _ = io.WriteString(w, "ok")
		}))
		t.Cleanup(srv.Close)

		c := testClient(t, srv.URL)
		resp, err := c.do(t.Context(), httpReq{method: http.MethodGet, path: "/v1/x"})
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })

		assert.Equal(t, version.UserAgent(), ua)
		assert.Contains(t, ua, "ajent/")
		assert.Equal(t, "Bearer "+testAPIKey, auth) // client headers still apply
	})
	t.Run("request_headers_override_the_client", func(t *testing.T) {
		var auth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth = r.Header.Get("Authorization")
			_, _ = io.WriteString(w, "ok")
		}))
		t.Cleanup(srv.Close)

		c := testClient(t, srv.URL)
		resp, err := c.do(t.Context(), httpReq{method: http.MethodGet, path: "/v1/x",
			headers: map[string]string{"Authorization": "Bearer other"}})
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })

		assert.Equal(t, "Bearer other", auth)
		assert.Equal(t, "Bearer "+testAPIKey, c.headers["Authorization"]) // shared map untouched
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
		c := testClient(t, srv.URL)
		_, err := c.do(t.Context(), httpReq{method: http.MethodPost, path: "/v1/x", classify: classify})
		assert.True(t, IsOverflow(err))
	})
}
