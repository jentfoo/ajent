package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
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

	t.Run("retryable_status_is_marked", func(t *testing.T) {
		err := compatClassifier("p", FlavorOpenRouter)(503, []byte(`{"error":{"message":"down"}}`))
		assert.True(t, IsRetryable(err))
	})
	t.Run("message_extracted_from_the_envelope", func(t *testing.T) {
		err := compatClassifier("p", FlavorOpenAI)(400, []byte(`{"error":{"message":"bad thing"}}`))
		assert.Contains(t, err.Error(), "bad thing")
	})
	t.Run("plain_body_is_kept", func(t *testing.T) {
		err := compatClassifier("p", FlavorOpenAI)(500, []byte(`internal failure`))
		assert.Contains(t, err.Error(), "internal failure")
	})
}
