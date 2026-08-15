package tools

import (
	"context"

	"github.com/jentfoo/ajent/pkg/agent"
)

// userInitiatedKey marks a context whose tool calls come from the human's own
// shell (a `!` line), not the model, so the permission gate exempts them.
type userInitiatedKey struct{}

// WithUserInitiated returns ctx marked as carrying a user-initiated call. The
// stager marks its context; the permit guard reads it to skip gating.
func WithUserInitiated(ctx context.Context) context.Context {
	return context.WithValue(ctx, userInitiatedKey{}, true)
}

// IsUserInitiated reports whether ctx was marked by WithUserInitiated.
func IsUserInitiated(ctx context.Context) bool {
	v, _ := ctx.Value(userInitiatedKey{}).(bool)
	return v
}

// Action is a guard's verdict on one call.
type Action uint8

const (
	ActionAllow Action = iota
	ActionDeny
	ActionAsk
)

// Decision is a guard's verdict on one call, with the reason that motivates it.
type Decision struct {
	Action Action
	Reason string
}

// Guard vets a tool call before it runs. The permission layer registers the
// barrier; core registers none by default so an agent runs unguarded unless
// configured.
type Guard func(ctx context.Context, call agent.ToolCall) Decision

// Allow is a guard that always permits its calls.
func Allow(agent.ToolCall) Decision { return Decision{Action: ActionAllow} }

// Deny builds a denying decision with the given reason.
func Deny(reason string) Decision { return Decision{Action: ActionDeny, Reason: reason} }
