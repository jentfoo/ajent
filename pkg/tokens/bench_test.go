package tokens

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/require"
)

// benchConversation builds a representative working session: a user ask, then
// turn pairs of thinking, prose and parallel tool calls answered by results,
// with an image riding in the first turn. Shaped like pkg/llm's Prepare
// fixture, since the estimator runs over the same transcripts.
func benchConversation() []llm.Message {
	img := llm.ImageBlock{MediaType: "image/jpeg", Data: make([]byte, 2048)}
	msgs := make([]llm.Message, 0, 2*benchTurns+1)
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: llm.BlockList{
		llm.TextBlock{Text: strings.Repeat("audit the parser package and fix the panics. ", 4)},
		img,
	}})
	for i := range benchTurns {
		msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{
			llm.ThinkingBlock{Text: fmt.Sprintf("plan pass %d: isolate the trim boundary first", i),
				Signature: fmt.Sprintf("sig-%03d", i), Encrypted: fmt.Sprintf("enc-%03d", i)},
			llm.TextBlock{Text: fmt.Sprintf("reading the reducer path, pass %d. ", i) +
				strings.Repeat("the window holds one block per turn, so growth is bounded. ", 16)},
			llm.ToolCallBlock{ID: fmt.Sprintf("call_%03d_a", i), Name: "read",
				Input: json.RawMessage(fmt.Sprintf(`{"path":"internal/parser/reduce_%03d.go"}`, i))},
			llm.ToolCallBlock{ID: fmt.Sprintf("call_%03d_b", i), Name: "grep",
				Input: json.RawMessage(`{"pattern":"panic","glob":"*.go"}`)},
		}})
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: llm.BlockList{
			llm.ToolResultBlock{CallID: fmt.Sprintf("call_%03d_a", i), ToolName: "read",
				Content: llm.BlockList{llm.TextBlock{Text: fmt.Sprintf("package parser\n\n// reduce_%03d.go\n", i) +
					strings.Repeat("func step(n int) int { return n %% 97 }\n", 120)}}},
			llm.ToolResultBlock{CallID: fmt.Sprintf("call_%03d_b", i), ToolName: "grep",
				Content: llm.BlockList{llm.TextBlock{
					Text: strings.Repeat(fmt.Sprintf("reduce.go:%d: panic recovered\n", i), 12)}}},
		}})
	}
	return msgs
}

const benchTurns = 48 // turn pairs, ~100 messages

// BenchmarkEstimateText measures the text estimate on the shapes that reach it
// per stream delta and per keystroke: a token fragment, one draft line, a long
// paste, and code/json bodies billed at their own ratios.
func BenchmarkEstimateText(b *testing.B) {
	for _, tc := range []struct {
		name string
		text string
		kind Kind
	}{
		{"delta_fragment", " parser keeps", KindProse},
		{"draft_line", "fix the reducer path so the window holds one block per turn, then re-run the tests", KindProse},
		{"long_paste", strings.Repeat("The parser keeps a 64-line window, so the fix lands there. ", 64), KindProse},
		{"code_body", strings.Repeat("func step(n int) int { return n % 97 }\n", 200), KindCode},
		{"json_args", strings.Repeat(`{"path":"internal/parser/reduce.go","old":"a","new":"b"}`, 40), KindJSON},
	} {
		b.Run(tc.name, func(b *testing.B) {
			require.Positive(b, EstimateText(tc.text, tc.kind))
			b.ReportMetric(float64(len(tc.text)), "text-B/op")

			for b.Loop() {
				_ = EstimateText(tc.text, tc.kind)
			}
		})
	}
}

// benchFixedTools mirrors the offered set a working session carries: four
// immediate tools and two deferred ones, each with a realistic schema.
func benchFixedTools() []llm.ToolSchema {
	schema := []byte(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
	return []llm.ToolSchema{
		{Name: "read", Description: "read a file from the workspace", Parameters: schema},
		{Name: "grep", Description: "search file contents", Parameters: schema},
		{Name: "edit", Description: "edit a single file", Parameters: schema},
		{Name: "bash", Description: "run a shell command", Parameters: schema},
		{Name: "ctx7_docs", Description: "library docs lookup", Deferred: true, Parameters: schema},
		{Name: "web_search", Description: "web search", Deferred: true, Parameters: schema},
	}
}

// BenchmarkEstimateFixed measures the message-independent estimate that runs
// once per model call: the system block plus every tool schema, re-tokenized
// from scratch each time.
func BenchmarkEstimateFixed(b *testing.B) {
	req := llm.Request{
		System: llm.BlockList{llm.TextBlock{Text: strings.Repeat(
			"You are ajent, a terminal coding agent. Prefer the edit tool over shell writes. ", 40)}},
		Tools: benchFixedTools(),
	}
	require.Positive(b, EstimateFixed(req))

	for b.Loop() {
		_ = EstimateFixed(req)
	}
}

// BenchmarkEstimateMessages measures the per-conversation token estimate: the
// block switch, the per-rune prose/code/json scans and the image sizing.
func BenchmarkEstimateMessages(b *testing.B) {
	msgs := benchConversation()
	// ~100k tokens keeps the fixture honest against the ratios; the exact count
	// is not the point, only that every block kind carries weight
	if n := EstimateMessages(msgs); n < 80_000 {
		b.Fatalf("estimate of %d is too small to be representative", n)
	}
	b.ReportMetric(float64(len(msgs)), "msgs/op")

	for b.Loop() {
		_ = EstimateMessages(msgs)
	}
}
