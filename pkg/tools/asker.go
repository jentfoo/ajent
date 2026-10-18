package tools

import (
	"context"

	"github.com/jentfoo/ajent/pkg/agent"
)

// Asker resolves a guard's ActionAsk into a final allow or deny. InstallGate
// installs it together with the guard chain, so an ask always has somewhere to
// go.
type Asker func(ctx context.Context, call agent.ToolCall, d Decision) Decision
