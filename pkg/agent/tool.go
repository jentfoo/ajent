package agent

import (
	"context"
	"io"

	"github.com/jentfoo/ajent/pkg/llm"
)

// ExecutionMode is whether a tool may run alongside others in one batch.
type ExecutionMode uint8

const (
	ModeSerial ExecutionMode = iota
	ModeParallel
)

// Tool is one thing the model may ask for. The interface keeps the loop
// decoupled from any concrete tool implementation, so it stays fully testable.
type Tool interface {
	Name() string
	Label(call ToolCall) string // header line text, falls back to Name on bad input
	Description() string
	Schema() llm.ToolSchema
	Mode() ExecutionMode
	Execute(ctx context.Context, call ToolCall, out Output) (ToolResult, error)
}

// Output is a running tool's display channel. Writes stream to the UI as they
// arrive; Diff commits a rendered file change.
type Output interface {
	io.Writer
	Diff(path, before, after string)
}

// ToolSet is a read-only view of the tools declared for a turn.
type ToolSet interface {
	Get(name string) (Tool, bool)
	Schemas() []llm.ToolSchema
	Names() []string // enabled names in declaration order
}

// Serializer optionally reports whether a batch must run one at a time because
// one or more calls would open an approval dialog. Parallel dispatch races the
// dialogs against each other, so prompt order could differ from submission
// order; a ToolSet whose guard chain may ask implements this to keep them in lockstep.
type Serializer interface {
	MustSerialize(calls []ToolCall) bool
}
