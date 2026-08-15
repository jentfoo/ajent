package tools

import (
	"context"

	"github.com/jentfoo/ajent/pkg/agent"
)

// Asker resolves a guard's ActionAsk into a final allow or deny. The permission
// layer registers one; with none registered an ask denies.
type Asker func(ctx context.Context, call agent.ToolCall, d Decision) Decision

// SetAsker registers the asker consulted when a guard returns ActionAsk. With
// no asker registered an ask still refuses the call.
func (r *Registry) SetAsker(a Asker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asker = a
}
