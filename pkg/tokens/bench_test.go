package tokens

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
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
