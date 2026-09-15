package llm

import (
	"time"

	"github.com/jentfoo/ajent/pkg/httputil"
)

// Prompt retry defaults. A model call is not an ordinary API request: provider
// endpoints recover slower, so the ladder starts four times later than httputil's
// generic failure timing (500ms) and doubles from there. The max sits above the
// ladder so the doubling is never cut short (2s, 4s, 8s, 16s, 32s, ...).
const (
	promptRetryBase = 2000 * time.Millisecond
	promptRetryMax  = 2 * time.Minute
)

// RetryPolicy bounds automatic retries. Zero fields take the defaults.
type RetryPolicy struct {
	Attempts int      `json:"attempts,omitempty"` // total attempts including the first
	Base     Duration `json:"base,omitzero"`
	Max      Duration `json:"max,omitzero"`
	Jitter   float64  `json:"jitter,omitempty"` // fraction of the delay, 0 to 1
}

// bounds returns the transport form of the configured policy. Unset fields take
// the prompt defaults rather than httputil's generic ones, so model calls get
// four times the room an ordinary API failure gets.
func (p RetryPolicy) bounds() httputil.RetryPolicy {
	base := time.Duration(p.Base)
	if base <= 0 {
		base = promptRetryBase
	}
	max := time.Duration(p.Max)
	if max <= 0 {
		max = promptRetryMax
	}
	return httputil.RetryPolicy{
		Attempts: p.Attempts,
		Base:     base,
		Max:      max,
		Jitter:   p.Jitter,
	}
}

// DefaultPromptPolicy returns the retry policy for a model call with attempts
// total tries. The ladder starts at promptRetryBase and doubles per attempt.
func DefaultPromptPolicy(attempts int) httputil.RetryPolicy {
	return RetryPolicy{Attempts: attempts}.bounds()
}
