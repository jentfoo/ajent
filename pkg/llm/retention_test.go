package llm

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReplayable(t *testing.T) {
	t.Parallel()

	anthropic := Capabilities{Dialect: DialectAnthropic}
	responses := Capabilities{Dialect: DialectOpenAIResponses}
	openrouter := Capabilities{Dialect: DialectOpenAICompletions, Thinking: ThinkingOpenRouter}

	tests := []struct {
		name     string
		block    ThinkingBlock
		caps     Capabilities
		expected bool
	}{
		{"anthropic_signature", ThinkingBlock{Signature: "sig"}, anthropic, true},
		{"anthropic_redacted", ThinkingBlock{Redacted: "cmVk"}, anthropic, true},
		{"anthropic_empty_allowed", ThinkingBlock{Text: "t"}, Capabilities{
			Dialect: DialectAnthropic, AllowEmptySignature: true}, true},
		{"anthropic_unsigned_dropped", ThinkingBlock{Text: "t"}, anthropic, false},

		{"responses_verbatim_item", ThinkingBlock{Item: json.RawMessage(`{}`)}, responses, true},
		{"responses_id_and_encrypted", ThinkingBlock{ItemID: "rs_1", Encrypted: "enc"}, responses, true},
		// store is pinned false, so an id alone references nothing server-side
		{"responses_id_only_dropped", ThinkingBlock{ItemID: "rs_1"}, responses, false},
		{"responses_encrypted_without_id_dropped", ThinkingBlock{Encrypted: "enc"}, responses, false},
		{"responses_plain_text_dropped", ThinkingBlock{Text: "t"}, responses, false},

		{"openrouter_details", ThinkingBlock{Details: []byte(`[{}]`)}, openrouter, true},
		{"openrouter_plain_text_dropped", ThinkingBlock{Text: "t"}, openrouter, false},
		{"compat_source_field", ThinkingBlock{Field: fieldReasoning}, openrouter, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, replayable(tc.block, tc.caps))
		})
	}
}
