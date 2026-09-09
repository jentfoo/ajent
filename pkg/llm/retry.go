package llm

import (
	"time"

	"github.com/jentfoo/ajent/pkg/httputil"
)

// RetryPolicy bounds automatic retries. Zero fields take the defaults.
type RetryPolicy struct {
	Attempts int      `json:"attempts,omitempty"` // total attempts including the first
	Base     Duration `json:"base,omitzero"`
	Max      Duration `json:"max,omitzero"`
	Jitter   float64  `json:"jitter,omitempty"` // fraction of the delay, 0 to 1
}

// bounds returns the transport form of the configured policy.
func (p RetryPolicy) bounds() httputil.RetryPolicy {
	return httputil.RetryPolicy{
		Attempts: p.Attempts,
		Base:     time.Duration(p.Base),
		Max:      time.Duration(p.Max),
		Jitter:   p.Jitter,
	}
}
