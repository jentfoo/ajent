package llm

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// thinkingModel builds a chat-completions model with the given caps, used by
// encoder tests to assert exact wire shape without a server.
func thinkingModel(caps Capabilities) Model {
	if caps.Reasoning && len(caps.Budgets) == 0 {
		caps.Budgets = defaultBudgets(64000)
	}
	return Model{Provider: "p", ID: "m1",
		ContextWindow: 200000, MaxOutput: 8000, Caps: caps}
}

// encodeThinking runs buildCompatBody for a request against the given model and
// reasoning choice, returning the decoded body map.
func encodeThinking(t *testing.T, m Model, level Level) map[string]any {
	t.Helper()
	body, err := buildCompatBody(Request{
		Model:     m,
		System:    BlockList{TextBlock{Text: "sys"}},
		Messages:  []Message{Text(RoleUser, "hi")},
		MaxTokens: 4000,
		Reasoning: ReasoningConfig{Level: level},
	}, compatProfile{})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

// reasoningFragment keeps only the thinking-related keys of a body for shape asserts.
func reasoningFragment(m map[string]any) string {
	out := make(map[string]any)
	for _, k := range []string{"reasoning", "thinking", "enable_thinking",
		"chat_template_kwargs", "chat_template_args", "reasoning_effort",
		"thinking_token_budget"} {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

func TestApplyThinking(t *testing.T) {
	t.Parallel()

	// every format at a reasoning-on and reasoning-off level asserts the exact wire
	// shape pi emits. effort controls whether top-level reasoning_effort is gated on,
	// mirroring each provider's detection default.
	cases := []struct {
		name        string
		format      ThinkingFormat
		effort      bool
		wantOnJSON  string // level high
		wantOffJSON string // LevelOff
	}{
		{"zai", ThinkingZAI, false,
			`{"thinking":{"type":"enabled","clear_thinking":false}}`, `{"thinking":{"type":"disabled"}}`},
		{"qwen", ThinkingQwen, true,
			`{"enable_thinking":true,"reasoning_effort":"high"}`, `{"enable_thinking":false}`},
		{"qwen_chat_template", ThinkingQwenChatTemplate, false,
			`{"chat_template_kwargs":{"enable_thinking":true,"preserve_thinking":true}}`,
			`{"chat_template_kwargs":{"enable_thinking":false,"preserve_thinking":true}}`},
		{"deepseek", ThinkingDeepSeek, true,
			`{"thinking":{"type":"enabled"},"reasoning_effort":"high"}`, `{"thinking":{"type":"disabled"}}`},
		{"openrouter", ThinkingOpenRouter, false,
			`{"reasoning":{"effort":"high"}}`, `{"reasoning":{"effort":"none"}}`},
		{"together", ThinkingTogether, false,
			`{"reasoning":{"enabled":true}}`, `{"reasoning":{"enabled":false}}`},
		{"string_thinking", ThinkingStringThinking, false,
			`{"thinking":"high"}`, `{"thinking":"none"}`},
		// the default case covers none and openai via top-level reasoning_effort
		{"openai_default", ThinkingOpenAI, true,
			`{"reasoning_effort":"high"}`, `{}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caps := Capabilities{Reasoning: true, Thinking: tc.format,
				SupportsReasoningEffort: tc.effort}
			m := thinkingModel(caps)

			assert.JSONEq(t, tc.wantOnJSON, reasoningFragment(encodeThinking(t, m, LevelHigh)))
			assert.JSONEq(t, tc.wantOffJSON, reasoningFragment(encodeThinking(t, m, LevelOff)))
		})
	}
}

func TestApplyThinkingRegressionPins(t *testing.T) {
	t.Parallel()

	t.Run("off_null_omits_thinking_key", func(t *testing.T) {
		caps := Capabilities{Reasoning: true, Thinking: ThinkingDeepSeek}
		v := "on"
		caps.LevelMap = map[Level]*string{LevelHigh: &v, LevelOff: nil}
		m := thinkingModel(caps)
		assert.NotContains(t, encodeThinking(t, m, LevelOff), "thinking")
	})
	t.Run("empty_reasoning_content_when_level_on", func(t *testing.T) {
		// detection sets ReplayReasoning for deepseek; it drives the empty echo
		caps := Capabilities{Reasoning: true, Thinking: ThinkingDeepSeek, ReplayReasoning: true}
		m := thinkingModel(caps)
		body, err := buildCompatBody(Request{
			Model:     m,
			System:    BlockList{TextBlock{Text: "sys"}},
			Messages:  []Message{Text(RoleUser, "q"), Text(RoleAssistant, "reply")},
			Reasoning: ReasoningConfig{Level: LevelHigh, Retain: RetainAll},
		}, compatProfile{})
		require.NoError(t, err)
		assert.Contains(t, string(body), `"reasoning_content":""`)
	})
	t.Run("no_empty_reasoning_when_level_off", func(t *testing.T) {
		caps := Capabilities{Reasoning: true, Thinking: ThinkingDeepSeek}
		m := thinkingModel(caps)
		body, err := buildCompatBody(Request{
			Model:     m,
			System:    BlockList{TextBlock{Text: "sys"}},
			Messages:  []Message{Text(RoleUser, "q"), Text(RoleAssistant, "reply")},
			Reasoning: ReasoningConfig{Retain: RetainAll}, // level defaults to off
		}, compatProfile{})
		require.NoError(t, err)
		assert.NotContains(t, string(body), `reasoning_content`)
	})
	t.Run("no_reasoning_capability_sends_nothing", func(t *testing.T) {
		caps := Capabilities{Thinking: ThinkingDeepSeek} // Reasoning false
		m := thinkingModel(caps)
		assert.JSONEq(t, `{}`, reasoningFragment(encodeThinking(t, m, LevelHigh)))
	})
	t.Run("max_clamps_to_mapped_xhigh_not_bare_high", func(t *testing.T) {
		// a provider that genuinely accepts xhigh gets its mapped value when max
		// is requested; it never falls through to an unmapped bare "high"
		caps := Capabilities{Reasoning: true, Thinking: ThinkingOpenAI,
			SupportsReasoningEffort: true}
		v := "deep"
		caps.LevelMap = map[Level]*string{LevelXHigh: &v}
		m := thinkingModel(caps)
		assert.JSONEq(t, `{"reasoning_effort":"deep"}`,
			reasoningFragment(encodeThinking(t, m, LevelMax)))
	})
}

func TestApplyThinkingBudgetGates(t *testing.T) {
	t.Parallel()

	t.Run("effort_omitted_without_capability", func(t *testing.T) {
		caps := Capabilities{Reasoning: true, Thinking: ThinkingZAI}
		m := thinkingModel(caps)
		assert.NotContains(t, encodeThinking(t, m, LevelHigh), "reasoning_effort")
	})
	t.Run("baseten_sends_effort_while_on", func(t *testing.T) {
		caps := Capabilities{Reasoning: true, Thinking: ThinkingBaseten,
			SupportsReasoningEffort: true}
		m := thinkingModel(caps)
		assert.JSONEq(t, `{"reasoning_effort":"high"}`,
			reasoningFragment(encodeThinking(t, m, LevelHigh)))
	})
	t.Run("baseten_off_sends_no_spurious_effort", func(t *testing.T) {
		// no map entry: off must not emit a bare reasoning_effort:"off"
		caps := Capabilities{Reasoning: true, Thinking: ThinkingBaseten,
			SupportsReasoningEffort: true}
		m := thinkingModel(caps)
		assert.JSONEq(t, `{}`, reasoningFragment(encodeThinking(t, m, LevelOff)))
	})
	t.Run("explicit_empty_off_value_reaches_wire", func(t *testing.T) {
		// pi sends an explicit empty string rather than omitting the key
		v := ""
		caps := Capabilities{Reasoning: true, Thinking: ThinkingOpenAI,
			SupportsReasoningEffort: true}
		caps.LevelMap = map[Level]*string{LevelOff: &v}
		m := thinkingModel(caps)
		assert.JSONEq(t, `{"reasoning_effort":""}`,
			reasoningFragment(encodeThinking(t, m, LevelOff)))
	})
}

func TestChatTemplateValues(t *testing.T) {
	t.Parallel()

	caps := Capabilities{Reasoning: true}
	v := "on"
	caps.LevelMap = map[Level]*string{LevelOff: &v, LevelHigh: ptrOf("high")}

	vals := map[string]json.RawMessage{
		"temperature": json.RawMessage(`0.7`), // literal pass-through
		"thinking_on": json.RawMessage(`{"$var":"thinking.enabled","omitWhenOff":true}`),
		"effort":      json.RawMessage(`{"$var":"thinking.effort"}`),
	}

	t.Run("on_resolves_enabled_and_effort", func(t *testing.T) {
		got := chatTemplateValues(vals, caps, LevelHigh)
		assert.Equal(t, map[string]any{
			"temperature": json.RawMessage(`0.7`),
			"thinking_on": true,
			"effort":      "high",
		}, got)
	})
	t.Run("off_drops_omit_when_off", func(t *testing.T) {
		got := chatTemplateValues(vals, caps, LevelOff)
		assert.NotContains(t, got, "thinking_on")
		// off routes the effort object through the map's off entry
		assert.Equal(t, "on", got["effort"])
		assert.JSONEq(t, `0.7`, string(got["temperature"].(json.RawMessage)))
	})
	t.Run("nil_when_no_values", func(t *testing.T) {
		assert.Nil(t, chatTemplateValues(nil, caps, LevelHigh))
	})
	t.Run("unknown_var_falls_through_to_effort_map", func(t *testing.T) {
		// pi routes any object that is not thinking.enabled through the effort map
		got := chatTemplateValues(map[string]json.RawMessage{
			"custom": json.RawMessage(`{"$var":"reasoning_depth"}`),
		}, caps, LevelHigh)
		assert.Equal(t, map[string]any{"custom": "high"}, got)
	})
	t.Run("object_without_var_falls_through_to_effort_map", func(t *testing.T) {
		got := chatTemplateValues(map[string]json.RawMessage{
			"plain": json.RawMessage(`{"depth": 2}`),
		}, caps, LevelHigh)
		assert.Equal(t, map[string]any{"plain": "high"}, got)
	})
	t.Run("off_without_map_entry_drops_object", func(t *testing.T) {
		// off has no explicit entry here (caps maps Off to a value), so it resolves
		caps := Capabilities{Reasoning: true}
		got := chatTemplateValues(map[string]json.RawMessage{
			"plain": json.RawMessage(`{"depth": 2}`),
		}, caps, LevelOff)
		assert.NotContains(t, got, "plain")
	})
}

func TestQwenChatTemplateConfiguredKwargs(t *testing.T) {
	t.Parallel()

	t.Run("merges_configured_kwargs", func(t *testing.T) {
		caps := Capabilities{Reasoning: true, Thinking: ThinkingQwenChatTemplate,
			ChatTemplateKwargs: map[string]json.RawMessage{
				"extra": json.RawMessage(`true`),
			}}
		m := thinkingModel(caps)

		on := reasoningFragment(encodeThinking(t, m, LevelHigh))
		off := reasoningFragment(encodeThinking(t, m, LevelOff))
		assert.JSONEq(t,
			`{"chat_template_kwargs":{"enable_thinking":true,"preserve_thinking":true,"extra":true}}`, on)
		assert.JSONEq(t,
			`{"chat_template_kwargs":{"enable_thinking":false,"preserve_thinking":true,"extra":true}}`, off)
	})

	t.Run("core_keys_win_over_configured", func(t *testing.T) {
		// a configured enable_thinking must not override the resolved level toggle
		caps := Capabilities{Reasoning: true, Thinking: ThinkingQwenChatTemplate,
			ChatTemplateKwargs: map[string]json.RawMessage{
				"enable_thinking": json.RawMessage(`false`),
			}}
		m := thinkingModel(caps)

		on := reasoningFragment(encodeThinking(t, m, LevelHigh))
		assert.JSONEq(t,
			`{"chat_template_kwargs":{"enable_thinking":true,"preserve_thinking":true}}`, on)
	})
}

func TestThinkingTokenBudget(t *testing.T) {
	t.Parallel()

	base := func() Request {
		m := compatModel(func(c *Capabilities) {
			c.Reasoning = true
			c.Thinking = ThinkingOpenAI
			c.SupportsReasoningEffort = true
			c.ThinkingBudgetField = fieldThinkingTokenBudget
			c.Budgets = map[Level]int{LevelHigh: 30000, LevelMedium: 8000}
		})
		return Request{
			Model:     m,
			System:    BlockList{TextBlock{Text: "sys"}},
			Messages:  []Message{Text(RoleUser, "hi")},
			MaxTokens: 20000,
			Reasoning: ReasoningConfig{Level: LevelHigh},
		}
	}

	t.Run("clamped_to_the_answer_ceiling", func(t *testing.T) {
		body, err := buildCompatBody(base(), compatProfile{})
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(body, &m))
		assert.InDelta(t, 18976, m["thinking_token_budget"], 0.001) // min(30000, maxTokens-1024)
	})
	t.Run("omitted_when_ceiling_too_small", func(t *testing.T) {
		req := base()
		req.MaxTokens = 500 // ceiling - floor <= 0
		assert.NotContains(t,
			decodeThinkingBudget(t, req), "thinking_token_budget")
	})
	t.Run("absent_without_the_capability", func(t *testing.T) {
		req := base()
		req.Model.Caps.ThinkingBudgetField = ""
		assert.NotContains(t,
			decodeThinkingBudget(t, req), "thinking_token_budget")
	})
	t.Run("absent_at_level_off", func(t *testing.T) {
		req := base()
		req.Reasoning.Level = LevelOff
		assert.NotContains(t,
			decodeThinkingBudget(t, req), "thinking_token_budget")
	})
}

// decodeThinkingBudget runs buildCompatBody and returns the decoded body map.
func decodeThinkingBudget(t *testing.T, req Request) map[string]any {
	t.Helper()
	body, err := buildCompatBody(req, compatProfile{})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(body, &m))
	return m
}
