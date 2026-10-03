package httputil

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestRedactURLVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		raw      string
		contains string
	}{
		{"upper_case", "https://x.test/v1?API_KEY=" + testAPIKey, "API_KEY=redacted"},
		{"mixed_case", "https://x.test/v1?ApiKey=" + testAPIKey, "ApiKey=redacted"},
		{"no_separator", "https://x.test/v1?apikey=" + testAPIKey, "apikey=redacted"},
		{"dash_separator", "https://x.test/v1?api-key=" + testAPIKey, "api-key=redacted"},
		{"token_param", "https://x.test/v1?token=" + testAPIKey, "token=redacted"},
		{"auth_param", "https://x.test/v1?auth=" + testAPIKey, "auth=redacted"},
		{"secret_param", "https://x.test/v1?secret=" + testAPIKey, "secret=redacted"},
		{"userinfo_masked", "https://user:pw@" + "x.test/v1", "x.test/v1"},
		{"unrelated_param_kept", "https://x.test/v1?model=opus", "model=opus"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.raw)
			require.NoError(t, err)

			got := redactURL(u)
			assert.Contains(t, got, tc.contains)
			assert.NotContains(t, got, testAPIKey)
			assert.NotContains(t, got, "user:pw")
		})
	}
}

func TestCredentialParam(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
		want bool
	}{
		{"exact_key", "key", true},
		{"lowercase", "api_key", true},
		{"upper_case", "API_KEY", true},
		{"mixed_case", "ApiKey", true},
		{"dash_separator", "access-token", true},
		{"unrelated", "model", false},
		{"prefix_match_not_enough", "keys", false},
		{"empty", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, credentialParam(tc.key))
		})
	}
}

func TestRedactErrURL(t *testing.T) {
	t.Parallel()

	u, err := url.Parse("https://x.test/v1?api_key=" + testAPIKey)
	require.NoError(t, err)

	t.Run("rewrites_the_url_error", func(t *testing.T) {
		uerr := &url.Error{Op: "Post", URL: u.String(), Err: io.EOF}

		redactErrURL(uerr, u)
		assert.Equal(t, "Post \"https://x.test/v1?api_key=redacted\": "+io.EOF.Error(), uerr.Error())
	})

	t.Run("wrapped_url_error_reached", func(t *testing.T) {
		var redacted *url.Error
		wrapped := &retryableError{err: &url.Error{Op: "Post", URL: u.String(), Err: io.EOF}}

		redactErrURL(wrapped, u)
		require.ErrorAs(t, wrapped, &redacted)
		assert.NotContains(t, redacted.Error(), testAPIKey)
	})

	t.Run("other_errors_untouched", func(t *testing.T) {
		plain := io.EOF
		redactErrURL(plain, u)
		assert.Equal(t, io.EOF, plain)
	})
}

func TestScrubSecrets(t *testing.T) {
	t.Parallel()

	t.Run("replaces_known_values", func(t *testing.T) {
		body := []byte(`{"error":"invalid key ` + testAPIKey + ` provided"}`)

		got := string(scrubSecrets(body, []string{testAPIKey}))
		assert.JSONEq(t, `{"error":"invalid key [redacted] provided"}`, got)
	})

	t.Run("replaces_bearer_token_without_prefix", func(t *testing.T) {
		body := []byte(`error: token ` + strings.TrimPrefix("Bearer "+testAPIKey, "Bearer ") + ` rejected`)

		got := string(scrubSecrets(body, secretValues(http.Header{"Authorization": {"Bearer " + testAPIKey}})))
		assert.NotContains(t, got, testAPIKey)
	})

	t.Run("masks_key_shaped_strings", func(t *testing.T) {
		tests := []struct {
			name string
			body string
		}{
			{"openai_shape", `{"message":"bad key sk-1234567890abcdefghijklmnop"}`},
			{"anthropic_shape", `{"message":"bad key sk-ant-1234567890abcdefghijklmnop"}`},
			{"google_shape", `{"message":"bad key AIza1234567890abcdefghijklmnopqrstuvwxy"}`},
			{"bearer_prefix", `{"message":"bad key Bearer 1234567890abcdefghijklmnop"}`},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				assert.NotContains(t, string(scrubSecrets([]byte(tc.body), nil)), "1234567890")
			})
		}
	})

	t.Run("leaves_normal_text_alone", func(t *testing.T) {
		body := []byte(`{"error":"prompt is too long: 128000 > 200000"}`)

		assert.Equal(t, string(body), string(scrubSecrets(body, nil)))
	})

	t.Run("empty_secrets_ignored", func(t *testing.T) {
		body := []byte("no credentials here")

		assert.Equal(t, string(body), string(scrubSecrets(body, []string{""})))
	})
}

func TestSecretValues(t *testing.T) {
	t.Parallel()

	t.Run("collects_credential_headers", func(t *testing.T) {
		h := http.Header{}
		h.Set("Authorization", "Bearer "+testAPIKey)
		h.Set("X-Api-Key", testAPIKey)
		h.Set("Content-Type", "application/json")

		got := secretValues(h)
		assert.ElementsMatch(t, []string{"Bearer " + testAPIKey, testAPIKey, testAPIKey}, got)
	})

	t.Run("no_bearer_split_without_prefix", func(t *testing.T) {
		h := http.Header{}
		h.Set("Authorization", "Basic abc")

		assert.Equal(t, []string{"Basic abc"}, secretValues(h))
	})

	t.Run("empty_headers", func(t *testing.T) {
		assert.Empty(t, secretValues(http.Header{}))
	})
}

func TestReadErrorBody(t *testing.T) {
	t.Parallel()

	t.Run("truncates_at_the_limit", func(t *testing.T) {
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(strings.Repeat("a", errBodyLimit*2)))}
		assert.Len(t, readErrorBody(resp, nil), errBodyLimit)
	})

	t.Run("scrubs_echoed_key", func(t *testing.T) {
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(`{"error":"key ` + testAPIKey + ` invalid"}`))}

		got := readErrorBody(resp, []string{testAPIKey})
		assert.NotContains(t, string(got), testAPIKey)
		assert.Contains(t, string(got), redactedMask)
	})
}
