package llm

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBlockListMarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		blocks BlockList
	}{
		{"empty", BlockList{}},
		{"text", BlockList{TextBlock{Text: "hi"}}},
		{
			"thinking_all_tokens",
			BlockList{ThinkingBlock{
				Text:      "pondering",
				Signature: "sig",
				Redacted:  "cmVkYWN0ZWQ=",
				ItemID:    "rs_1",
				Encrypted: "enc",
				Details:   []byte(`[{"type":"reasoning.text"}]`),
			}},
		},
		{"tool_call", BlockList{ToolCallBlock{ID: "t1", Name: "read", Input: json.RawMessage(`{"p":1}`)}}},
		{
			"tool_result_nested",
			BlockList{ToolResultBlock{
				CallID:  "t1",
				Content: BlockList{TextBlock{Text: "out"}, ImageBlock{MediaType: "image/png", Data: []byte{1, 2}}},
				IsError: true,
			}},
		},
		{"image", BlockList{ImageBlock{MediaType: "image/png", Data: []byte{0, 1, 2}}}},
		{
			"mixed",
			BlockList{
				ThinkingBlock{Text: "t", Signature: "s"},
				TextBlock{Text: "answer"},
				ToolCallBlock{ID: "t1", Name: "bash", Input: json.RawMessage(`{}`)},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.blocks)
			require.NoError(t, err)

			var got BlockList
			require.NoError(t, json.Unmarshal(data, &got))
			assert.Equal(t, tc.blocks, got)
		})
	}

	t.Run("unknown_type_errors", func(t *testing.T) {
		var got BlockList
		err := json.Unmarshal([]byte(`[{"type":"bogus","data":{}}]`), &got)
		assert.ErrorContains(t, err, "unknown block type")
	})
}

// TestMessageRebuildFieldsAreNotSerialized pins that the rebuild-only fields
// (Origin, Stop) never reach the transcript while additive block fields survive it.
func TestMessageRebuildFieldsRoundTrip(t *testing.T) {
	t.Parallel()

	m := Message{
		Role: RoleAssistant,
		Content: BlockList{
			ThinkingBlock{Text: "hmm", Field: "reasoning_content"},
			TextBlock{Text: "hello", Signature: "msg_1.phase"},
			ToolResultBlock{CallID: "t1", Content: BlockList{TextBlock{Text: "out"}},
				AddedToolNames: []string{"read"}},
		},
		Origin: &Origin{Provider: "anthropic", Dialect: DialectAnthropic, Model: "claude"},
		Stop:   StopEndTurn,
	}

	data, err := json.Marshal(m)
	require.NoError(t, err)
	// rebuild identity must stay out of the transcript format
	assert.NotContains(t, string(data), `"origin"`)
	assert.NotContains(t, string(data), `"stop"`)
	assert.NotContains(t, string(data), "anthropic")

	var got Message
	require.NoError(t, json.Unmarshal(data, &got))
	// additive block fields survive the round trip
	think := got.Content[0].(ThinkingBlock)
	assert.Equal(t, "reasoning_content", think.Field)
	text := got.Content[1].(TextBlock)
	assert.Equal(t, "msg_1.phase", text.Signature)
	tr := got.Content[2].(ToolResultBlock)
	assert.Equal(t, []string{"read"}, tr.AddedToolNames)
}

func TestMessageMarshalJSON(t *testing.T) {
	t.Parallel()

	t.Run("round_trips_blocks", func(t *testing.T) {
		m := Message{
			Role: RoleAssistant,
			Content: BlockList{
				ThinkingBlock{Text: "hmm", Signature: "sig"},
				TextBlock{Text: "hello"},
			},
		}
		data, err := json.Marshal(m)
		require.NoError(t, err)

		var got Message
		require.NoError(t, json.Unmarshal(data, &got))
		assert.Equal(t, m, got)
	})
	t.Run("role_is_plain_string", func(t *testing.T) {
		data, err := json.Marshal(Text(RoleUser, "hi"))
		require.NoError(t, err)
		assert.Contains(t, string(data), `"role":"user"`)
	})
}

func TestText(t *testing.T) {
	t.Parallel()

	m := Text(RoleUser, "hi")
	assert.Equal(t, RoleUser, m.Role)
	require.Len(t, m.Content, 1)
	assert.Equal(t, TextBlock{Text: "hi"}, m.Content[0])
}
