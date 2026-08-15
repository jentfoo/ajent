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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EstimateText(tc.text, tc.kind)
			assert.InDeltaf(t, float64(tc.want), float64(got), tc.tol,
				"estimate %d deviates from reference %d", got, tc.want)
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

func TestEstimateRequestExcludesUnretainedThinking(t *testing.T) {
	t.Parallel()

	// a content-field provider replays inline thinking only when retention keeps it.
	caps := llm.Capabilities{Dialect: llm.DialectOpenAICompletions, Reasoning: true,
		Thinking: llm.ThinkingDeepSeek, ReplayReasoning: true}
	req := llm.Request{
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

	dropped := EstimateRequest(req) // RetainNone strips the thinking block

	retained := req
	retained.Reasoning.Retain = llm.RetainAll // keeps it, adding its tokens back
	assert.Greater(t, EstimateRequest(retained), dropped,
		"retaining thinking must count more than stripping it")
}

func TestEstimateTextCJKOneTokenPerRune(t *testing.T) {
	t.Parallel()

	// built from escapes so the Han script never appears as a source literal
	cjk := "\u4f60\u597d\u4e16\u754c\uff0c\u4eca\u5929\u6c14\u5f88\u597d\u3002"
	assert.Equal(t, len([]rune(cjk)), EstimateText(cjk, KindProse))
}

func TestEstimateTextAstralPairsCostTwo(t *testing.T) {
	t.Parallel()

	// each astral (emoji) rune counts one token plus one more for the pair
	astral := "🎉🚀🔥✨"
	want := 0
	for _, r := range astral {
		want++ // one for the non-ASCII rune
		if r > 0xFFFF {
			want++ // and one more for the surrogate pair
		}
	}
	assert.Equal(t, want, EstimateText(astral, KindProse))
}

func TestEstimateFixedCountsSystemAndToolsOnly(t *testing.T) {
	t.Parallel()

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
}

func TestEstimateRequestMatchesPreparedMessages(t *testing.T) {
	t.Parallel()

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
}
