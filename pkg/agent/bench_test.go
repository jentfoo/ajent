package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
)

// benchStreamEvents shapes one streamed response the way providers deliver it:
// thinking and text deltas plus three tool calls whose arguments arrive in
// 8-byte fragments, so forward runs its per-delta paths on every event.
func benchStreamEvents() []llm.Event {
	const args = `{"path":"internal/parser/parse.go","oldText":"return buf.trim()","newText":"return buf.trimRight()"}`
	evs := make([]llm.Event, 0, 128)

	var think strings.Builder
	evs = append(evs, llm.Event{Type: llm.EventThinkingStart, Index: 0})
	for i := range 24 {
		d := fmt.Sprintf(" step %d: weigh the parse order against buffer reuse", i)
		think.WriteString(d)
		evs = append(evs, llm.Event{Type: llm.EventThinkingDelta, Index: 0, Text: d})
	}
	evs = append(evs, llm.Event{Type: llm.EventThinkingEnd, Index: 0,
		Block: llm.ThinkingBlock{Text: think.String(), Signature: "sig-bench"}})

	var text strings.Builder
	evs = append(evs, llm.Event{Type: llm.EventTextStart, Index: 1})
	for i := range 32 {
		d := fmt.Sprintf(" The parser keeps a %d-line window, so the fix lands there. ", i)
		text.WriteString(d)
		evs = append(evs, llm.Event{Type: llm.EventTextDelta, Index: 1, Text: d})
	}
	evs = append(evs, llm.Event{Type: llm.EventTextEnd, Index: 1, Block: llm.TextBlock{Text: text.String()}})

	for c, name := range []string{"read", "edit", "bash"} {
		idx := 2 + c
		id := fmt.Sprintf("toolu_bench_%02d", c)
		evs = append(evs, llm.Event{Type: llm.EventToolCallStart, Index: idx, ToolCallID: id, ToolName: name})
		for i := 0; i < len(args); i += 8 {
			evs = append(evs, llm.Event{Type: llm.EventToolCallDelta, Index: idx, ToolCallID: id,
				Text: args[i:min(i+8, len(args))]})
		}
		evs = append(evs, llm.Event{Type: llm.EventToolCallEnd, Index: idx, ToolCallID: id, ToolName: name,
			Block: llm.ToolCallBlock{ID: id, Name: name, Input: json.RawMessage(args)}})
	}
	return append(evs, llm.Event{Type: llm.EventUsage,
		Usage: llm.Usage{Input: 48231, Output: 812, CacheRead: 39000, CacheWrite: 1200}})
}

// benchState builds one turn-loop state sized like a working session: ~100
// messages of tool traffic under a reasoning-capable model.
func benchState() *State {
	m := llm.Model{ID: "bench-model", ContextWindow: 200000, MaxOutput: 64000}
	own := &llm.Origin{Provider: "bench", Dialect: m.Caps.Dialect, Model: m.ID}
	msgs := make([]llm.Message, 0, 2*benchStateTurns+1)
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Origin: own, Content: llm.BlockList{
		llm.TextBlock{Text: "audit the parser package and fix the panics. "},
	}})
	for i := range benchStateTurns {
		msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Origin: own, Stop: llm.StopToolUse, Content: llm.BlockList{
			llm.ThinkingBlock{Text: fmt.Sprintf("plan pass %d: isolate the trim boundary", i)},
			llm.TextBlock{Text: fmt.Sprintf("reading the reducer path, pass %d. ", i)},
			llm.ToolCallBlock{ID: fmt.Sprintf("call_%03d_a", i), Name: "read",
				Input: json.RawMessage(fmt.Sprintf(`{"path":"internal/parser/reduce_%03d.go"}`, i))},
			llm.ToolCallBlock{ID: fmt.Sprintf("call_%03d_b", i), Name: "grep",
				Input: json.RawMessage(`{"pattern":"panic","glob":"*.go"}`)},
		}})
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Origin: own, Content: llm.BlockList{
			llm.ToolResultBlock{CallID: fmt.Sprintf("call_%03d_a", i), ToolName: "read",
				Content: llm.BlockList{llm.TextBlock{Text: strings.Repeat("func step(n int) int { return n % 97 }\n", 24)}}},
			llm.ToolResultBlock{CallID: fmt.Sprintf("call_%03d_b", i), ToolName: "grep",
				Content: llm.BlockList{llm.TextBlock{Text: strings.Repeat("reduce.go: panic recovered\n", 6)}}},
		}})
	}
	return &State{Messages: msgs, Model: m, Tokens: tokens.New(m)}
}

const benchStateTurns = 48 // turn pairs, ~100 messages

// BenchmarkForward measures the per-event stream path: sink emissions, the
// spend-ledger text estimates, tool-argument progress and the throttled context
// sync. One op is one full event sequence through a fresh progress tracker.
func BenchmarkForward(b *testing.B) {
	a := newTestAgent(benchState(), &llm.ScriptedProvider{}, NopSink{})
	events := benchStreamEvents()
	b.ReportMetric(float64(len(events)), "events/op")

	for b.Loop() {
		var prog toolProgress
		for _, ev := range events {
			a.forward(a.sink, true, &prog, ev)
		}
	}
}

// BenchmarkAppend measures recording one message in a working session's state:
// the model stamp, the tool-input repair walk, the append itself and the ledger
// estimate every unreported message needs. One op is an assistant call plus its
// tool-result batch, then the state rewinds so size stays steady.
func BenchmarkAppend(b *testing.B) {
	a := newTestAgent(benchState(), &llm.ScriptedProvider{}, NopSink{})
	call := llm.ToolCallBlock{ID: "call_new", Name: "read",
		Input: json.RawMessage(`{"path":"internal/parser/reduce.go"}`)}
	b.ReportMetric(float64(len(a.state.Messages)), "state-msgs")

	for b.Loop() {
		n := len(a.state.Messages)
		a.append(MessageInfo{Message: llm.Message{Role: llm.RoleAssistant,
			Content: llm.BlockList{llm.TextBlock{Text: "reading the reducer. "}, call}}, Stop: llm.StopToolUse})
		result := llm.ToolResultBlock{CallID: call.ID, ToolName: "read",
			Content: llm.BlockList{llm.TextBlock{
				Text: strings.Repeat("func step(n int) int { return n % 97 }\n", 24)}}}
		a.appendToolResults(llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{call}},
			[]llm.ToolResultBlock{result}, "turn aborted")
		a.state.Messages = a.state.Messages[:n]
	}
}

// BenchmarkBuildRequest measures per model-call assembly: the pure message
// assembly, the system block rebuild (AGENTS.md body included) and the tool
// schema fetch, everything the estimator and the wire both consume.
func BenchmarkBuildRequest(b *testing.B) {
	a := newTestAgent(benchState(), &llm.ScriptedProvider{}, nil)
	a.opts.Tools = &mapSet{tools: map[string]Tool{
		"read":  fakeRead{},
		"grep":  fakeRead{},
		"edit":  fakeRead{},
		"bash":  fakeRead{},
		"write": fakeRead{},
	}}
	a.opts.ProjectInstructions = []ProjectInstruction{{
		Path: "/repo/AGENTS.md",
		Body: strings.Repeat("Prefer the edit tool over shell writes. Keep functions small. ", 24),
	}}
	req := a.buildRequest()
	require.NotEmpty(b, req.Messages)
	require.NotEmpty(b, req.System)

	for b.Loop() {
		_ = a.buildRequest()
	}
}
