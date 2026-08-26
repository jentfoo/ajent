package tokens

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		// 43 bytes sits on a half-token boundary at the prose ratio, so rounding decides
		// it; one token either way is within what a ratio heuristic can promise.
		{"prose_sentence", "the quick brown fox jumps over the lazy dog", KindProse, 10, 1},
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

func TestEstimateBytes(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 250, EstimateBytes(1000, KindProse))
	assert.Equal(t, 313, EstimateBytes(1000, KindCode)) // 312.5 rounds up, never truncates
	assert.Equal(t, 357, EstimateBytes(1000, KindJSON))
	assert.Equal(t, 278, EstimateBytes(1000, KindOpaque))
	assert.Equal(t, 250, EstimateBytes(1000, Kind(99))) // unknown kind reads as prose
	assert.Zero(t, EstimateBytes(0, KindCode))
	assert.Zero(t, EstimateBytes(-5, KindCode))
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
	// the result carries its own tool_use_id framing on top of its content
	assert.Equal(t, code+toolResultOverhead, inResult)
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

// TestEstimateBlocksCoversEveryType walks llm.BlockTypes so a block type added to
// pkg/llm cannot fall through to the unknown-block constant unnoticed. The probe
// is content sensitivity: a real arm charges more for more content, while the
// default constant is flat whatever the block holds.
func TestEstimateBlocksCoversEveryType(t *testing.T) {
	t.Parallel()

	short, long := "package main", strings.Repeat("package main\n", 64)
	samples := map[llm.BlockType][2]llm.Block{
		llm.BlockText:     {llm.TextBlock{Text: short}, llm.TextBlock{Text: long}},
		llm.BlockThinking: {llm.ThinkingBlock{Text: short}, llm.ThinkingBlock{Text: long}},
		llm.BlockToolCall: {
			llm.ToolCallBlock{ID: "toolu_01", Name: "read", Input: json.RawMessage(`{"a":1}`)},
			llm.ToolCallBlock{ID: "toolu_01", Name: "read", Input: json.RawMessage(`{"a":"` + long + `"}`)},
		},
		llm.BlockToolResult: {
			llm.ToolResultBlock{CallID: "toolu_01", Content: llm.BlockList{llm.TextBlock{Text: short}}},
			llm.ToolResultBlock{CallID: "toolu_01", Content: llm.BlockList{llm.TextBlock{Text: long}}},
		},
		llm.BlockImage: {
			llm.ImageBlock{MediaType: "image/png", Data: make([]byte, 4096)},
			llm.ImageBlock{MediaType: "image/png", Data: make([]byte, 1<<20)},
		},
	}
	for _, bt := range llm.BlockTypes {
		t.Run(string(bt), func(t *testing.T) {
			pair, ok := samples[bt]
			require.True(t, ok, "add a sample pair for this type, and an estimateBlocks arm")
			assert.Greater(t, estimateBlocks(llm.BlockList{pair[1]}),
				estimateBlocks(llm.BlockList{pair[0]}))
		})
	}
}

func TestEstimateFixedCountsDescriptions(t *testing.T) {
	t.Parallel()

	// a description is often the larger half of a schema, so dropping it under-counts
	// the tool block that rides in every request
	bare := llm.Request{Tools: []llm.ToolSchema{
		{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)},
	}}
	described := llm.Request{Tools: []llm.ToolSchema{
		{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`),
			Description: "Read the contents of a file. Returns line-numbered text."},
	}}
	assert.Greater(t, EstimateFixed(described), EstimateFixed(bare))
}

func TestEstimateThinking(t *testing.T) {
	t.Parallel()

	text := "reasoning about the problem at some length"
	tests := []struct {
		name  string
		block llm.ThinkingBlock
		want  int
	}{
		{"text_only", llm.ThinkingBlock{Text: text}, EstimateText(text, KindProse)},
		// a serialized responses item already embeds the text and encrypted content,
		// so it replaces them rather than adding to them
		{"item_supersedes", llm.ThinkingBlock{Text: text, Encrypted: "abcdef",
			Item: json.RawMessage(`{"type":"reasoning","id":"rs_1"}`)},
			EstimateText(`{"type":"reasoning","id":"rs_1"}`, KindOpaque)},
		// anthropic sends redacted data in place of thinking, never alongside it
		{"redacted_supersedes", llm.ThinkingBlock{Text: text, Redacted: "QUJDREVG"},
			EstimateText("QUJDREVG", KindOpaque)},
		{"signature_counted", llm.ThinkingBlock{Text: text, Signature: "sig_abcdefgh"},
			EstimateText(text, KindProse) + EstimateText("sig_abcdefgh", KindOpaque)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, estimateThinking(tc.block))
		})
	}
}

func TestEstimateToolResultExcludesTranscriptFields(t *testing.T) {
	t.Parallel()

	// Display, Details and AddedToolNames never reach the wire, so they must not
	// occupy context
	base := llm.ToolResultBlock{CallID: "toolu_01", ToolName: "read",
		Content: llm.BlockList{llm.TextBlock{Text: "package main"}}}
	loaded := base
	loaded.Display = "a much longer rendering shown only in the transcript history"
	loaded.Details = map[string]string{"key": "value"}
	loaded.AddedToolNames = []string{"grep", "find"}
	assert.Equal(t, estimateToolResult(base), estimateToolResult(loaded))
}

func TestImageDims(t *testing.T) {
	t.Parallel()

	png := append(append([]byte{}, pngMagic...), // IHDR length + tag, then 640x480
		0, 0, 0, 13, 'I', 'H', 'D', 'R', 0, 0, 2, 128, 0, 0, 1, 224)
	jpeg := []byte{0xFF, 0xD8, // SOI, then APP0 skipped by length, then SOF0 320x200
		0xFF, 0xE0, 0, 4, 0, 0,
		0xFF, 0xC0, 0, 17, 8, 0, 200, 1, 64, 3, 0, 0, 0, 0, 0, 0, 0, 0}

	t.Run("png", func(t *testing.T) {
		w, h, ok := imageDims(png)
		require.True(t, ok)
		assert.Equal(t, 640, w)
		assert.Equal(t, 480, h)
	})
	t.Run("jpeg", func(t *testing.T) {
		w, h, ok := imageDims(jpeg)
		require.True(t, ok)
		assert.Equal(t, 320, w)
		assert.Equal(t, 200, h)
	})
	t.Run("unknown_format", func(t *testing.T) {
		_, _, ok := imageDims([]byte("GIF89a and then some bytes to pad it out"))
		assert.False(t, ok)
	})
	t.Run("dimensions_beat_byte_size", func(t *testing.T) {
		// the same pixels compress differently; cost must follow area, not bytes
		small := imageTokens(llm.ImageBlock{Data: png})
		padded := imageTokens(llm.ImageBlock{Data: append(append([]byte{}, png...), make([]byte, 1<<20)...)})
		assert.Equal(t, small, padded)
		assert.Equal(t, 640*480/imagePixelsPerToken, small)
	})
	t.Run("unparsed_falls_back_to_bytes", func(t *testing.T) {
		assert.Equal(t, imageBaseTokens, imageTokens(llm.ImageBlock{Data: []byte("GIF89a")}))
	})
}

func TestEstimateForZeroModel(t *testing.T) {
	t.Parallel()

	// with no capabilities to normalize against, Prepare would call every message
	// foreign and downgrade every image, so a zero model must skip it entirely
	msgs := []llm.Message{
		llm.Text(llm.RoleUser, "hello"),
		{Role: llm.RoleAssistant, Content: llm.BlockList{
			llm.ThinkingBlock{Text: "some reasoning", Signature: "sig_1"},
			llm.TextBlock{Text: "hi"},
		}},
	}
	assert.Equal(t, EstimateMessages(msgs), EstimateFor(llm.Model{}, llm.RetainAll, msgs))
}

func TestEstimateToolPairMatchesLandedPair(t *testing.T) {
	t.Parallel()

	// the reserve a caller books before running an injected read must match what
	// EstimateMessages bills once the pair lands, or the bar steps as it arrives
	body := "package main\n\nfunc main() {}\n"
	call := llm.ToolCallBlock{ID: "ref-1-main.go", Name: "read",
		Input: json.RawMessage(`{"path":"main.go"}`)}
	// shaped as agent.InjectPair builds it, which sets no ToolName on the result
	// (pkg/agent imports this package, so the pair is rebuilt here rather than called)
	landed := EstimateMessages([]llm.Message{
		{Role: llm.RoleAssistant, Content: llm.BlockList{call}},
		{Role: llm.RoleUser, Content: llm.BlockList{llm.ToolResultBlock{
			CallID: call.ID, Content: llm.BlockList{llm.TextBlock{Text: body}},
		}}},
	})
	reserved := EstimateToolPair(call, int64(len(body)), KindCode)
	assert.Equal(t, landed, reserved)
}
