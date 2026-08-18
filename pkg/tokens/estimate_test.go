package tokens

import (
	"encoding/json"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
)

func TestEstimateText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		kind Kind
		want int // reference count recorded from a real tokenizer, within tolerance
		tol  float64
	}{
		{"prose_sentence", "the quick brown fox jumps over the lazy dog", KindProse, 10, 0.5},
		{"short_prose", "hello world", KindProse, 3, 1},
		{"go_source", "package main\nfunc f(x int) (int, error) {\n\treturn x * 2, nil\n}\n", KindCode, 20, 6},
		{"json_args", `{"file":"a.go","content":"line one\n"}`, KindJSON, 16, 5},
		{"minified_json", `[{"name":"x"},{"name":"y"}]`, KindJSON, 12, 4},
		// non-ASCII runes count exactly (one each), so tol < 1 enforces equality.
		{"cjk_one_token_per_rune", "\u4f60\u597d\u4e16\u754c\uff0c\u4eca\u5929\u6c14\u5f88\u597d\u3002", KindProse, 11, 0.1},
		// astral (emoji) pairs cost two; the trailing U+2728 is BMP and costs one.
		{"astral_pairs_cost_two", "🎉🚀🔥✨", KindProse, 7, 0.5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EstimateText(tc.text, tc.kind)
			assert.InDelta(t, float64(tc.want), float64(got), tc.tol)
		})
	}
}

func TestEstimateBlocksToolResultIsCode(t *testing.T) {
	t.Parallel()

	// a read result is a file; its text uses the denser code ratio (3.2 bytes/token),
	// so identical prose costs more tokens inside a tool result than as prose.
	txt := "the quick brown fox jumps over the lazy dog"
	inResult := estimateBlocks(llm.BlockList{llm.ToolResultBlock{
		Content: llm.BlockList{llm.TextBlock{Text: txt}},
	}})
	plain := EstimateText(txt, KindProse)
	code := EstimateText(txt, KindCode)
	assert.Equal(t, code, inResult)    // uses the code ratio
	assert.Greater(t, inResult, plain) // and that costs more tokens than prose
}

func TestEstimateRequest(t *testing.T) {
	t.Parallel()

	t.Run("excludes_unretained_thinking", func(t *testing.T) {
		// a content-field provider replays inline thinking only when retention keeps it.
		caps := llm.Capabilities{Dialect: llm.DialectOpenAICompletions, Reasoning: true,
			Thinking: llm.ThinkingDeepSeek, ReplayReasoning: true}
		newReq := func() llm.Request {
			return llm.Request{
				Model:  llm.Model{Caps: caps},
				System: llm.BlockList{llm.TextBlock{Text: "you are a helper"}},
				Messages: []llm.Message{
					{Role: llm.RoleUser, Content: llm.BlockList{llm.TextBlock{Text: "hello there friend"}}},
					{Role: llm.RoleAssistant, Content: llm.BlockList{
						llm.ThinkingBlock{Text: "a long chain of private reasoning that must not count"},
						llm.TextBlock{Text: "the answer"},
					}},
				},
			}
		}

		dropped := EstimateRequest(newReq()) // RetainNone strips the thinking block

		retained := newReq()
		retained.Reasoning.Retain = llm.RetainAll // keeps it, adding its tokens back
		assert.Greater(t, EstimateRequest(retained), dropped)
	})

	t.Run("counts_system_and_tools_plus_messages", func(t *testing.T) {
		req := llm.Request{
			Model:  llm.Model{ID: "m", Caps: llm.Capabilities{Dialect: llm.DialectOpenAICompletions}},
			System: llm.BlockList{llm.TextBlock{Text: "you are a helper"}},
			Tools: []llm.ToolSchema{
				{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)},
			},
			Messages: []llm.Message{
				{Role: llm.RoleUser, Content: llm.BlockList{llm.TextBlock{Text: "hello there friend"}}},
			},
		}

		sysTools := EstimateFixed(req)    // system + tool schemas only
		full := EstimateRequest(req)      // includes the message on top
		assert.Greater(t, full, sysTools) // messages add tokens beyond the fixed part
		// a bare request with no tools or system contributes nothing fixed
		assert.Zero(t, EstimateFixed(llm.Request{}))
	})

	t.Run("matches_prepared_messages", func(t *testing.T) {
		// a text-only model with an orphaned tool call: Prepare downgrades the image
		// and synthesizes a result, so the estimate must reflect what is really sent.
		m := llm.Model{Provider: "p", ID: "m", Caps: llm.Capabilities{Dialect: llm.DialectOpenAICompletions}}
		req := llm.Request{
			Model: m,
			Messages: []llm.Message{
				{Role: llm.RoleUser, Content: llm.BlockList{
					llm.TextBlock{Text: "look at this"},
					llm.ImageBlock{MediaType: "image/png", Data: make([]byte, 1024)},
				}},
				{Role: llm.RoleAssistant, Content: llm.BlockList{
					llm.ToolCallBlock{ID: "c1", Name: "read", Input: []byte(`{}`)},
				}},
			},
		}

		estimated := EstimateRequest(req) // normalizes through Prepare internally
		prepared := llm.Prepare(req).Messages
		manual := 0
		for _, msg := range prepared {
			manual += messageOverhead + estimateBlocks(msg.Content)
		}
		assert.Equal(t, manual, estimated,
			"the estimator must count exactly the messages Prepare will send")
	})
}
