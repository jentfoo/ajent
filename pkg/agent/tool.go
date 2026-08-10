package agent

import (
	"context"
	"io"

	"github.com/jentfoo/ajent/pkg/llm"
)

// Tool is one thing the model may ask for. The interface keeps the loop
// decoupled from any concrete tool implementation, so it stays fully testable.
type Tool interface {
	Schema() llm.ToolSchema
	Parallel() bool // a future ExecutionMode could widen this
	Execute(ctx context.Context, call ToolCall, out io.Writer) (ToolResult, error)
}

// ToolSet is a read-only view of the tools declared for a turn.
type ToolSet interface {
	Get(name string) (Tool, bool)
	Schemas() []llm.ToolSchema
}
