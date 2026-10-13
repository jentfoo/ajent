package llm

import (
	"net/http"
	"testing"

	"github.com/jentfoo/ajent/pkg/httputil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompatClassifier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		flavor   Flavor
		status   int
		body     string
		overflow bool
	}{
		{"lmstudio_context_length", FlavorLMStudio, 400,
			`{"error":{"message":"the context length is exceeded"}}`, true},
		{"llamacpp_500_n_ctx", FlavorLlamaCpp, 500,
			`{"error":{"message":"the request exceeds n_ctx"}}`, true},
		{"openai_code", FlavorOpenAI, 400,
			`{"error":{"message":"too big","code":"context_length_exceeded"}}`, true},
		{"openrouter_metadata", FlavorOpenRouter, 400,
			`{"error":{"message":"upstream","metadata":{"raw":"maximum context length"}}}`, true},
		{"unrelated_400_is_not_overflow", FlavorLMStudio, 400,
			`{"error":{"message":"unknown model"}}`, false},
		{"llamacpp_500_without_ctx_is_not_overflow", FlavorLlamaCpp, 500,
			`{"error":{"message":"slot unavailable"}}`, false},
		{"429_is_not_overflow", FlavorOpenRouter, 429, `{"error":{"message":"slow down"}}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := compatClassifier("p", tc.flavor)(tc.status, []byte(tc.body))
			assert.Equal(t, tc.overflow, IsOverflow(err))
		})
	}

	t.Run("message_extracted_from_the_envelope", func(t *testing.T) {
		err := compatClassifier("p", FlavorOpenAI)(400, []byte(`{"error":{"message":"bad thing"}}`))
		assert.Contains(t, err.Error(), "bad thing")
	})

	t.Run("plain_body_is_kept", func(t *testing.T) {
		err := compatClassifier("p", FlavorOpenAI)(500, []byte(`internal failure`))
		assert.Contains(t, err.Error(), "internal failure")
	})
}

func TestFrameStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code string
		want int
		ok   bool
	}{
		{"overloaded_error", "overloaded_error", httputil.StatusOverloaded, true},
		{"rate_limit_error", "rate_limit_error", http.StatusTooManyRequests, true},
		{"responses_server_error", "server_error", http.StatusInternalServerError, true},
		{"numeric_gateway", " 502 ", http.StatusBadGateway, true},
		{"unknown_app_code", "internal", 0, false},
		{"below_client_range", "302", 0, false},
		{"above_server_range", "600", 0, false},
		{"empty", "", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, ok := frameStatus(tc.code)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, status)
		})
	}
}

func TestFrameAPIError(t *testing.T) {
	t.Parallel()

	t.Run("overloaded_frame_is_recoverable", func(t *testing.T) {
		err := frameAPIError("anthropic", "overloaded_error", "Overloaded", FlavorAnthropic)
		var ae *APIError
		require.ErrorAs(t, err, &ae)
		assert.True(t, ae.Retryable)
		assert.Equal(t, httputil.StatusOverloaded, ae.Status)
	})

	t.Run("numeric_frame_is_recoverable", func(t *testing.T) {
		err := frameAPIError("gateway", "502", "bad gateway upstream", FlavorGeneric)
		var ae *APIError
		require.ErrorAs(t, err, &ae)
		assert.True(t, ae.Retryable)
		assert.Equal(t, http.StatusBadGateway, ae.Status)
	})

	t.Run("opaque_frame_stays_unrecoverable", func(t *testing.T) {
		err := frameAPIError("gateway", "internal", "upstream exploded", FlavorGeneric)
		var ae *APIError
		require.ErrorAs(t, err, &ae)
		assert.False(t, ae.Retryable)
		assert.Zero(t, ae.Status)
	})

	t.Run("overflow_frame_wins_over_the_table", func(t *testing.T) {
		err := frameAPIError("llamacpp", "server_error", "prompt exceeds n_ctx", FlavorLlamaCpp)
		assert.True(t, IsOverflow(err))
		assert.False(t, err.Retryable)
	})
}
