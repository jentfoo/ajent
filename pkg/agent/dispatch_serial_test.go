package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/require"
)

// serializeSet wraps mapSet with a fixed MustSerialize answer, so dispatch's
// Serializer path can be driven directly.
type serializeSet struct {
	*mapSet
	must bool
}

func (s *serializeSet) MustSerialize([]ToolCall) bool { return s.must }

// blockStub blocks its Execute until released; aStarted closes once it is running,
// so the test knows when the first call is in flight.
type blockStub struct {
	stubTool
	mu        sync.Mutex
	aStarted  chan<- struct{}
	sentStart bool
	release   <-chan struct{}
}

func (t *blockStub) Execute(ctx context.Context, call ToolCall, out Output) (ToolResult, error) {
	t.mu.Lock()
	if !t.sentStart {
		t.sentStart = true
		close(t.aStarted)
	}
	t.mu.Unlock()
	select {
	case <-t.release:
	case <-ctx.Done():
		return ToolResult{}, ctx.Err()
	}
	return t.stubTool.Execute(ctx, call, out)
}

// TestDispatchHonorsSerializer asserts a ToolSet that reports it must serialize is
// dispatched one call at a time even when every tool is ModeParallel and the model
// supports parallel tools. The second call cannot start while the first is in
// flight; only serial dispatch satisfies this, which keeps approval dialogs open in
// submission order.
func TestDispatchHonorsSerializer(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	aStarted := make(chan struct{})
	bStub := &stubTool{name: "b", result: "rb", parallel: true}
	set := &serializeSet{
		mapSet: &mapSet{tools: map[string]Tool{
			"a": &blockStub{aStarted: aStarted, release: release,
				stubTool: stubTool{name: "a", result: "ra", parallel: true}},
			"b": bStub,
		}},
		must: true,
	}
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: append(twoToolCalls("1", "a", "2", "b"), doneEvent())},
		{Events: textOnly("done")},
	}}
	a := newTestAgent(nil, p, nil)
	a.opts.Tools = set
	a.state.Model.Caps.ParallelTools = true

	errCh := make(chan error, 1)
	go func() { errCh <- a.Prompt(t.Context(), Input{Text: "x"}) }()

	<-aStarted // the first call is in flight and blocked
	require.Equal(t, 0, bStub.callCount(), "second call started before the first finished")
	close(release)

	require.NoError(t, <-errCh)
}
