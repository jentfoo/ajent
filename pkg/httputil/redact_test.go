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

func TestReadErrorBody(t *testing.T) {
	t.Parallel()

	t.Run("truncates_at_the_limit", func(t *testing.T) {
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(strings.Repeat("a", errBodyLimit*2)))}
		assert.Len(t, readErrorBody(resp), errBodyLimit)
	})
}
