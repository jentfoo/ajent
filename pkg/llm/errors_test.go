package llm

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIErrorError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      APIError
		expected string
	}{
		{"full", APIError{Provider: "anthropic", Status: 400, Code: "invalid_request_error", Message: "prompt is too long"},
			"llm: anthropic http 400 invalid_request_error: prompt is too long"},
		{"no_code", APIError{Provider: "openai", Status: 500, Message: "server error"},
			"llm: openai http 500: server error"},
		{"status_only", APIError{Provider: "lmstudio", Status: 404},
			"llm: lmstudio http 404"},
		{"provider_only", APIError{Provider: "llamacpp"}, "llm: llamacpp"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, tc.err.Error())
		})
	}
}

func TestAPIErrorUnwrap(t *testing.T) {
	t.Parallel()

	t.Run("overflow_matches_sentinel", func(t *testing.T) {
		err := (&APIError{Provider: "anthropic", Status: 400}).Overflow()
		require.ErrorIs(t, err, ErrContextOverflow)
		assert.True(t, IsOverflow(err))
		assert.False(t, err.Retryable) // an overflow is never worth retrying
	})
	t.Run("overflow_through_wrapping", func(t *testing.T) {
		err := fmt.Errorf("building request: %w", (&APIError{Provider: "openai"}).Overflow())
		assert.True(t, IsOverflow(err))
	})
	t.Run("plain_error_is_not_overflow", func(t *testing.T) {
		assert.False(t, IsOverflow(&APIError{Provider: "openai", Status: 429}))
		assert.False(t, IsOverflow(errors.New("boom")))
	})
	t.Run("errors_as_recovers_detail", func(t *testing.T) {
		err := fmt.Errorf("wrapped: %w", &APIError{Provider: "openrouter", Status: 429, RetryAfter: 2 * time.Second})
		var ae *APIError
		require.ErrorAs(t, err, &ae)
		assert.Equal(t, 2*time.Second, ae.RetryAfter)
	})
}

func TestIsRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"retryable_api_error", &APIError{Retryable: true}, true},
		{"non_retryable_api_error", &APIError{}, false},
		{"wrapped_retryable", fmt.Errorf("x: %w", &APIError{Retryable: true}), true},
		{"plain_error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, IsRetryable(tc.err))
		})
	}
}

func TestErrNoAPIKeyError(t *testing.T) {
	t.Parallel()

	t.Run("names_the_variable", func(t *testing.T) {
		err := &ErrNoAPIKey{Provider: "anthropic", EnvVar: "ANTHROPIC_API_KEY"}
		assert.Equal(t, "llm: no api key for anthropic, set ANTHROPIC_API_KEY", err.Error())
	})
	t.Run("without_variable", func(t *testing.T) {
		assert.Equal(t, "llm: no api key for custom", (&ErrNoAPIKey{Provider: "custom"}).Error())
	})
}

func TestErrAmbiguousModelError(t *testing.T) {
	t.Parallel()

	err := &ErrAmbiguousModel{Name: "sonnet", Candidates: []string{"anthropic/a", "openrouter/b"}}
	assert.Equal(t, `llm: "sonnet" matches anthropic/a, openrouter/b`, err.Error())
}
