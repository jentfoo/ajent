package llm

import "encoding/json"

const (
	// minAnswerTokens is the floor an output budget keeps for the answer, so a
	// capped reasoning phase never consumes the whole response.
	minAnswerTokens = 1024
)

// applyThinking dispatches reasoning on or off onto the wire shape pi emits for
// caps.Thinking. It runs after max-tokens fields are set in buildCompatBody so
// the vLLM thinking_token_budget can read them; it writes nothing when reasoning
// is unsupported or this request has none.
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
		// the two core keys carry the level; configured kwargs add on top without
		// overriding them so a user cannot break the toggle
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
			if e, ok := offValue(caps, "none"); ok {
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
			if e, ok := offValue(caps, "none"); ok {
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

// setReasoningEffort writes the top-level reasoning_effort for a level, gated on
// the capability that is never a style override.
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

// applyThinkingTokenBudget emits the vLLM thinking_token_budget for a capped
// reasoning phase, clamped so at least minAnswerTokens stay for the reply. It is
// independent of thinking format: any chat-completions model may be served by vLLM.
func applyThinkingTokenBudget(body *compatRequest, req Request, lvl Level, on bool) {
	if !on || !req.Model.Caps.SupportsThinkingTokenBudget {
		return
	}
	ceiling := req.Model.MaxOutput
	if body.MaxTokens != nil {
		ceiling = *body.MaxTokens
	} else if body.MaxCompletion != nil {
		ceiling = *body.MaxCompletion
	}
	budget := min(req.Model.Caps.Budgets[lvl], max(0, ceiling-minAnswerTokens))
	if budget > 0 {
		body.ThinkingTokenBudget = &budget
	}
}

// chatTemplateValues resolves configured template additions onto the kwargs a
// provider expects. Literal scalars pass through; every object that is not
// $var:"thinking.enabled" routes through the reasoning effort map, mirroring pi.
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
			e, ok := chatTemplateEffort(caps, l)
			if !ok {
				continue // unresolved effort is dropped
			}
			value = e
		}
		out[k] = value
	}
	return out
}

// chatTemplateEffort maps a level onto the provider effort an object routes to,
// mirroring pi: on resolves through LevelMap with the bare level as fallback, off
// only when an explicit off entry exists.
func chatTemplateEffort(caps Capabilities, l Level) (string, bool) {
	if l != LevelOff {
		return levelValue(caps, l)
	}
	return offValue(caps, "")
}

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
	// ThinkingThinkTags sends no request parameter; reasoning is parsed back
	// out of content with inline tags. An ajent extension for local models.
	ThinkingThinkTags
)

var thinkingNames = enumNames[ThinkingFormat]{
	ThinkingNone:             "none",
	ThinkingOpenAI:           "openai",
	ThinkingOpenRouter:       "openrouter",
	ThinkingDeepSeek:         "deepseek",
	ThinkingTogether:         "together",
	ThinkingBaseten:          "baseten",
	ThinkingZAI:              "zai",
	ThinkingQwen:             "qwen",
	ThinkingChatTemplate:     "chat-template",
	ThinkingQwenChatTemplate: "qwen-chat-template",
	ThinkingStringThinking:   "string-thinking",
	ThinkingAntLing:          "ant-ling",
	ThinkingAnthropic:        "anthropic",
	ThinkingThinkTags:        "think-tags",
}

// thinkingAliases maps legacy spellings pi's catalogue once used onto their
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
