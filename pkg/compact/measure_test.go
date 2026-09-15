package compact

import (
	"encoding/json"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/stretchr/testify/assert"
)

// reasoningModel replays thinking back to the provider, so Prepare keeps a
// same-origin ThinkingBlock at its full size instead of degrading it.
func reasoningModel() llm.Model {
	return llm.Model{Provider: "anthropic", ID: "claude-opus-4-5",
		Caps: llm.Capabilities{Reasoning: true, Dialect: llm.DialectAnthropic}}
}

func TestMeasureOriginStampsThinking(t *testing.T) {
	t.Parallel()

	model := reasoningModel()
	resolve := func(string) (llm.Model, error) { return model, nil }

	branch := []session.Entry{
		{ID: "s0", Type: session.TypeSession, Data: mustMarshal(session.SessionData{Model: "anthropic/claude-opus-4-5"})},
	}
	for i := 1; i <= 6; i++ {
		n := string(rune('a' + i))
		branch = append(branch,
			msg("u"+n, llm.Text(llm.RoleUser, "step "+n)),
			msg("a"+n, llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{
				llm.ThinkingBlock{Text: "reasoning about " + n, Signature: "sig-" + n},
				llm.TextBlock{Text: "answer " + n},
			}}),
		)
	}

	stamped := tokensFor(branch, session.CompactionData{}, model, llm.RetainAll, 0, resolve)
	nilr := tokensFor(branch, session.CompactionData{}, model, llm.RetainAll, 1, nil)

	assert.Greater(t, stamped, nilr)

	// the resolver stamps each assistant message with its producing origin; a
	// measurement that ran without it would see every one as foreign.
	msgs, _ := session.ContextMessages(branch, session.CompactionData{}, resolve)
	for _, m := range msgs {
		if m.Role == llm.RoleAssistant {
			assert.NotNil(t, m.Origin)
			assert.Equal(t, model.ID, m.Origin.Model)
		}
	}
}

func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
