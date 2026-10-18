package llm

import (
	"encoding/json"
	"strconv"
)

const (
	// minAnswerTokens is the floor an output budget keeps for the answer, so a
	// capped reasoning phase never consumes the whole response.
	minAnswerTokens = 1024
)

// applyThinking writes reasoning onto body in the wire shape caps.Thinking
// expects: an effort level or budget when on, explicit off markers where the
// format carries them. No-op when the model cannot reason.
func applyThinking(body *compatRequest, req Request) {
	caps := req.Model.Caps
	if !caps.Reasoning {
		return
	}
	lvl := clampLevel(caps, req.Reasoning.Level)
	on := lvl != LevelOff

	switch caps.Thinking {
	case ThinkingZAI:
		if on {
			body.Thinking = compatThinking{Type: "enabled", ClearThinking: ptrOf(false)}
			setReasoningEffort(body, caps, lvl)
		} else {
			body.Thinking = compatThinking{Type: "disabled"}
		}
	case ThinkingQwen:
		if on {
			body.EnableThinking = ptrOf(true)
			setReasoningEffort(body, caps, lvl)
		} else {
			body.EnableThinking = ptrOf(false)
		}
	case ThinkingQwenChatTemplate:
		// the two core keys carry the level. Configured kwargs add on top without
		// overriding them so a user cannot break the toggle.
		kw := map[string]any{"enable_thinking": on, "preserve_thinking": true}
		for k, v := range chatTemplateValues(caps.ChatTemplateKwargs, caps, lvl) {
			if _, core := kw[k]; !core {
				kw[k] = v
			}
		}
		body.ChatTemplateKwarg = kw
	case ThinkingChatTemplate:
		if vals := chatTemplateValues(caps.ChatTemplateKwargs, caps, lvl); len(vals) > 0 {
			body.ChatTemplateKwarg = vals
		}
	case ThinkingBaseten:
		if vals := chatTemplateValues(caps.ChatTemplateArgs, caps, lvl); len(vals) > 0 {
			body.ChatTemplateArgs = vals
		}
		// baseten is the one format that sends effort while thinking is off
		if on {
			setReasoningEffort(body, caps, lvl)
		} else if e, ok := offValue(caps, ""); ok && caps.SupportsReasoningEffort {
			body.ReasoningEffort = &e
		}
	case ThinkingDeepSeek:
		if on {
			body.Thinking = compatThinking{Type: "enabled"}
			setReasoningEffort(body, caps, lvl)
		} else if !offSuppressed(caps) {
			body.Thinking = compatThinking{Type: "disabled"}
		}
	case ThinkingOpenRouter:
		if on {
			if req.Reasoning.Budget > 0 {
				body.Reasoning = &compatReasoning{MaxTokens: req.Reasoning.Budget}
			} else if e, ok := levelValue(caps, lvl); ok {
				body.Reasoning = &compatReasoning{Effort: &e}
			}
		} else if !offSuppressed(caps) {
			if e, ok := offValue(caps, valueNone); ok {
				body.Reasoning = &compatReasoning{Effort: &e}
			}
		}
	case ThinkingAntLing:
		// ant-ling sends effort only when the map holds a non-null string
		if on {
			if v, ok := caps.LevelMap[lvl]; ok && v != nil {
				body.Reasoning = &compatReasoning{Effort: ptrOf(*v)}
			}
		}
	case ThinkingTogether:
		if on {
			body.Reasoning = &compatReasoning{Enabled: ptrOf(true)}
			setReasoningEffort(body, caps, lvl)
		} else {
			body.Reasoning = &compatReasoning{Enabled: ptrOf(false)}
		}
	case ThinkingStringThinking:
		if on {
			if e, ok := levelValue(caps, lvl); ok {
				body.Thinking = e
			}
		} else if !offSuppressed(caps) {
			if e, ok := offValue(caps, valueNone); ok {
				body.Thinking = e
			}
		}
	default: // none, openai, think-tags and anthropic all use top-level reasoning_effort
		if on {
			setReasoningEffort(body, caps, lvl)
		} else if e, ok := offValue(caps, ""); ok && caps.SupportsReasoningEffort {
			body.ReasoningEffort = &e
		}
	}

	applyThinkingTokenBudget(body, req, lvl, on)
}

// setReasoningEffort writes the top-level reasoning_effort for l, resolved
// through caps.LevelMap with the level name as fallback. No-op unless the model
// declares SupportsReasoningEffort.
func setReasoningEffort(body *compatRequest, caps Capabilities, l Level) {
	if !caps.SupportsReasoningEffort {
		return
	}
	e, ok := levelValue(caps, l)
	if !ok || e == "" {
		return
	}
	body.ReasoningEffort = &e
}

// applyThinkingTokenBudget caps the reasoning phase under
// caps.ThinkingBudgetField, keeping minAnswerTokens for the reply. The ceiling
// is model-fixed rather than the live request cap, so the budget cannot drift
// and bust the engine's prefix cache. Independent of thinking format: any
// chat-completions model may be served by vLLM.
func applyThinkingTokenBudget(body *compatRequest, req Request, lvl Level, on bool) {
	field := req.Model.Caps.ThinkingBudgetField
	if !on || field == "" {
		return
	}
	budget := min(req.Model.Caps.Budgets[lvl],
		max(0, MaxOutputFor(req.Model, 0)-minAnswerTokens))
	if budget > 0 {
		if body.extra == nil { // records one configuration named body key
			body.extra = make(map[string]json.RawMessage, 1)
		}
		body.extra[field] = json.RawMessage(strconv.Itoa(budget))
	}
}

// chatTemplateValues resolves configured template additions onto the kwargs a
// provider expects. Literal scalars pass through, and every object that is not
// $var:"thinking.enabled" routes through the reasoning effort map.
// omitWhenOff drops an object while reasoning is off. It returns nil when nothing survives.
func chatTemplateValues(vals map[string]json.RawMessage, caps Capabilities, l Level) map[string]any {
	if len(vals) == 0 {
		return nil
	}
	on := l != LevelOff
	out := make(map[string]any, len(vals))
	for k, raw := range vals {
		var obj struct {
			Var         string `json:"$var"`
			OmitWhenOff *bool  `json:"omitWhenOff"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			out[k] = raw // not an object: pass through verbatim
			continue
		}
		if obj.OmitWhenOff != nil && *obj.OmitWhenOff && !on {
			continue
		}
		var value any = on
		if obj.Var != "thinking.enabled" {
			// on resolves through the level map with the bare level as fallback,
			// off only when an explicit off entry exists
			e, ok := levelValue(caps, l)
			if l == LevelOff {
				e, ok = offValue(caps, "")
			}
			if !ok {
				continue // unresolved effort is dropped
			}
			value = e
		}
		out[k] = value
	}
	return out
}

// ThinkingFormat values name the wire encoding a provider expects.
type ThinkingFormat uint8

const (
	ThinkingNone ThinkingFormat = iota
	ThinkingOpenAI
	ThinkingOpenRouter
	ThinkingDeepSeek
	ThinkingTogether
	ThinkingBaseten
	ThinkingZAI
	ThinkingQwen
	ThinkingChatTemplate
	ThinkingQwenChatTemplate
	ThinkingStringThinking
	ThinkingAntLing
	// ThinkingAnthropic is the Messages budget shape, an ajent extension.
	ThinkingAnthropic
	// ThinkingThinkTags sends no request parameter, since reasoning is parsed back
	// out of content with inline tags. An ajent extension for local models.
	ThinkingThinkTags
)

var thinkingNames = enumNames[ThinkingFormat]{
	ThinkingNone:             valueNone,
	ThinkingOpenAI:           providerOpenAI,
	ThinkingOpenRouter:       openRouterAffinityFormat,
	ThinkingDeepSeek:         "deepseek",
	ThinkingTogether:         "together",
	ThinkingBaseten:          "baseten",
	ThinkingZAI:              "zai",
	ThinkingQwen:             "qwen",
	ThinkingChatTemplate:     "chat-template",
	ThinkingQwenChatTemplate: "qwen-chat-template",
	ThinkingStringThinking:   "string-thinking",
	ThinkingAntLing:          "ant-ling",
	ThinkingAnthropic:        providerAnthropic,
	ThinkingThinkTags:        "think-tags",
}

// thinkingAliases maps legacy spellings once used by a model catalogue onto their
// canonical format, consulted after the enum lookup fails.
var thinkingAliases = map[string]ThinkingFormat{
	"reasoning_effort":  ThinkingOpenAI,
	"openai-responses":  ThinkingOpenAI,
	"reasoning_content": ThinkingDeepSeek,
}

// parseThinkingFormat resolves a configured name to its format, accepting the
// canonical enum names and legacy aliases. Unknown values report false so a
// caller can warn rather than silently pick a default.
func parseThinkingFormat(s string) (ThinkingFormat, bool) {
	if v, ok := thinkingNames.lookup(s); ok {
		return v, true
	}
	v, ok := thinkingAliases[s]
	return v, ok
}

// String returns the configuration name of the format.
func (f ThinkingFormat) String() string { return thinkingNames.name(f) }

// MarshalText encodes the format as its configuration name.
func (f ThinkingFormat) MarshalText() ([]byte, error) { return thinkingNames.marshalText(f) }

// UnmarshalText decodes the configuration name.
func (f *ThinkingFormat) UnmarshalText(data []byte) error {
	return thinkingNames.unmarshalText(data, f, "thinking format")
}
