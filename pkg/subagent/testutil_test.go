package subagent

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// fakeTool is a minimal Tool used to prove the structural filter.
type fakeTool struct {
	name   string
	result string
	err    error
	mode   agent.ExecutionMode
}

func (t *fakeTool) Name() string                { return t.name }
func (t *fakeTool) Label(agent.ToolCall) string { return t.name + ": ..." }
func (t *fakeTool) Description() string         { return "test tool" }
func (t *fakeTool) Schema() llm.ToolSchema      { return llm.ToolSchema{Name: t.name} }
func (t *fakeTool) Mode() agent.ExecutionMode {
	if t.mode == 0 {
		return agent.ModeParallel
	}
	return t.mode
}

// Execute returns a canned result or error.
func (t *fakeTool) Execute(ctx context.Context, _ agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	if t.err != nil {
		return agent.ToolResult{Content: llm.BlockList{llm.TextBlock{Text: t.err.Error()}}, IsError: true}, t.err
	}
	return agent.ToolResult{Content: llm.BlockList{llm.TextBlock{Text: t.result}}}, nil
}

// fakeSource is a ToolSource with explicit read-only marks.
type fakeSource struct {
	tools    []agent.Tool
	readOnly map[string]bool // default false
}

func (s *fakeSource) All() []agent.Tool         { return s.tools }
func (s *fakeSource) ReadOnly(name string) bool { return s.readOnly[name] }

// roTool builds a fake tool marked read-only in the source.
func roTool(name string) agent.Tool { return &fakeTool{name: name} }

// scripted returns a Provider func over one ScriptedProvider and that provider.
func scripted(turns []llm.ScriptedTurn) (func(llm.Model) (llm.Provider, error), *llm.ScriptedProvider) {
	p := &llm.ScriptedProvider{Turns: turns}
	return func(llm.Model) (llm.Provider, error) { return p, nil }, p
}

// blockingProvider blocks every Stream until ctx is cancelled; used for abort and
// shutdown tests where the turn must be pinned in flight.
type blockingProvider struct{}

func (*blockingProvider) Name() string { return "scripted" }
func (b *blockingProvider) Stream(ctx context.Context, _ llm.Request) (llm.Stream, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// gatedProvider holds each stream open until releaseAll; it counts how many runs at once.
type gatedProvider struct {
	mu      sync.Mutex
	gates   []chan struct{}
	active  atomic.Int32  // currently in-flight streams
	peak    atomic.Int32  // high-water mark of active
	release chan struct{} // closed to let every stream proceed
}

func (g *gatedProvider) Name() string { return "scripted" }

// Stream blocks until the shared release is closed, then returns a canned summary.
func (g *gatedProvider) Stream(ctx context.Context, _ llm.Request) (llm.Stream, error) {
	n := g.active.Add(1)
	for { // high-water mark without racing on a plain int
		if cur := g.peak.Load(); n > cur && !g.peak.CompareAndSwap(cur, n) {
			continue
		}
		break
	}
	defer g.active.Add(-1)

	rel := make(chan struct{})
	g.mu.Lock()
	if g.release != nil { // a previous release closed the shared gate; proceed now
		close(rel)
	} else {
		g.gates = append(g.gates, rel)
	}
	g.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-rel:
	}
	return &llm.SliceStream{Events: summaryTurn("done", llm.Usage{})}, nil
}

// releaseAll closes every held gate so all in-flight streams proceed.
func (g *gatedProvider) releaseAll() {
	g.mu.Lock()
	if g.release != nil { // already released once; nothing new to do
		g.mu.Unlock()
		return
	}
	g.release = make(chan struct{})
	close(g.release)
	for _, ch := range g.gates {
		close(ch)
	}
	g.gates = nil
	g.mu.Unlock()
}

// delayedProvider holds the first stream until release, then serves a canned turn.
type delayedProvider struct {
	release chan struct{}
	turn    []llm.Event
}

func (d *delayedProvider) Name() string { return "scripted" }
func (d *delayedProvider) Stream(ctx context.Context, _ llm.Request) (llm.Stream, error) {
	if d.release != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-d.release:
		}
	}
	return &llm.SliceStream{Events: d.turn}, nil
}

// summaryTurn frames one assistant text turn ending with StopEndTurn.
func summaryTurn(text string, usage llm.Usage) []llm.Event {
	out := []llm.Event{
		{Type: llm.EventTextStart, Index: 0},
		{Type: llm.EventTextDelta, Index: 0, Text: text},
		{Type: llm.EventTextEnd, Index: 0, Block: llm.TextBlock{Text: text}},
	}
	if usage != (llm.Usage{}) {
		out = append(out, llm.Event{Type: llm.EventUsage, Usage: usage})
	}
	return append(out, doneEvent())
}

// thinkingOnlyTurn frames an assistant turn whose only content is reasoning.
func thinkingOnlyTurn() []llm.Event {
	return []llm.Event{
		{Type: llm.EventThinkingStart, Index: 0},
		{Type: llm.EventThinkingDelta, Index: 0, Text: "thinking…"},
		{Type: llm.EventThinkingEnd, Index: 0, Block: llm.ThinkingBlock{Text: "thinking…"}},
		doneEvent(),
	}
}

func doneEvent() llm.Event { return llm.Event{Type: llm.EventDone, StopReason: llm.StopEndTurn} }

// capture is a goroutine-safe recorder for activity/notice/deliver.
type capture struct {
	mu       sync.Mutex
	rows     []string // "key|text" pairs from Activity
	notices  []string
	delivers []agent.Input // inputs offered to Deliver; Delivered run by caller
}

func newCapture() *capture { return &capture{} }

// recordRow appends an activity publish as key|text.
func (c *capture) recordRow(key, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows = append(c.rows, key+"|"+text)
}

// rowText returns the last published text for a key, or "" when none/empty.
func (c *capture) rowText(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.rows) - 1; i >= 0; i-- {
		if strings.HasPrefix(c.rows[i], key+"|") {
			return strings.TrimPrefix(c.rows[i], key+"|")
		}
	}
	return ""
}

// deliveredTexts returns the steer texts offered so far.
func (c *capture) deliveredTexts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.delivers))
	for i, in := range c.delivers {
		out[i] = in.Text
	}
	return out
}

// noticeCount returns how many notices arrived.
func (c *capture) noticeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.notices)
}

// noticeTexts returns every notice sent so far.
func (c *capture) noticeTexts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.notices)
}

// lastNotice returns the most recent notice, or "" when none.
func (c *capture) lastNotice() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.notices) == 0 {
		return ""
	}
	return c.notices[len(c.notices)-1]
}

var _ agent.Tool = (*fakeTool)(nil)
