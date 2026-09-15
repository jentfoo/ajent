package llm

import (
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/httputil"
	"github.com/stretchr/testify/assert"
)

func TestRetryPolicyBounds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		policy       RetryPolicy
		expectedBase time.Duration
		expectedMax  time.Duration
	}{
		{"prompt_defaults", RetryPolicy{}, promptRetryBase, promptRetryMax},
		{"explicit_bounds_kept", RetryPolicy{Base: Duration(700 * time.Millisecond), Max: Duration(time.Minute)}, 700 * time.Millisecond, time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.policy.bounds()
			assert.Equal(t, tc.expectedBase, b.Base)
			assert.Equal(t, tc.expectedMax, b.Max)
		})
	}
}

func TestDefaultPromptPolicyLadder(t *testing.T) {
	t.Parallel()

	// 2s -> 4s -> 8s -> 16s -> 32s, the four-times prompt ladder
	p := DefaultPromptPolicy(6)
	p.Jitter = 1

	for attempt := 1; attempt <= 5; attempt++ {
		d, ok := httputil.BackoffDelay(p, attempt, 0, 0)
		assert.True(t, ok)
		assert.Equal(t, promptRetryBase<<(attempt-1), d)
	}

	_, ok := httputil.BackoffDelay(p, 6, 0, 0)
	assert.False(t, ok) // attempts exhausted
}
