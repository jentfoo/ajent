package tools

import (
	"context"

	"github.com/jentfoo/ajent/pkg/agent"
)

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

// Guard vets a tool call before it runs. Phase 14 registers the barrier; core
// registers none by default so an agent runs unguarded unless configured.
type Guard func(ctx context.Context, call agent.ToolCall) Decision

// Allow is a guard that always permits its calls.
func Allow(agent.ToolCall) Decision { return Decision{Action: ActionAllow} }

// Deny builds a denying decision with the given reason.
func Deny(reason string) Decision { return Decision{Action: ActionDeny, Reason: reason} }
