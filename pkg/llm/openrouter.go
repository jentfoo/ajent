package llm

import (
	"encoding/json"
	"slices"
)

// decorateOpenRouter adds the upstream routing preference and usage reporting
// that openrouter accepts beyond the shared dialect. The reasoning parameter is
// owned by applyThinking.
func decorateOpenRouter(routing *Routing) func(*compatRequest, Request) {
	return func(body *compatRequest, req Request) {
		body.Usage = &compatUsageOption{Include: true}
		if r := req.Model.Caps.OpenRouterRouting; len(r) > 0 {
			body.Provider = r // verbatim when configuration set it
		} else if routing != nil {
			body.Provider = compatRoutingFrom(routing)
		}
	}
}

// compatRoutingFrom maps the configured routing onto its wire form.
func compatRoutingFrom(r *Routing) *compatRouting {
	return &compatRouting{
		Order:               slices.Clone(r.Order),
		AllowFallbacks:      r.AllowFallbacks,
		DataCollection:      r.DataCollection,
		RequireParameters:   r.RequireParams,
		Only:                slices.Clone(r.Only),
		Ignore:              slices.Clone(r.Ignore),
		Zdr:                 r.Zdr,
		EnforceDistillable:  r.EnforceDistillable,
		Quantizations:       slices.Clone(r.Quantizations),
		Sort:                r.Sort,
		MaxPrice:            r.MaxPrice,
		PreferredThroughput: r.PreferredThroughput,
		PreferredLatency:    r.PreferredLatency,
	}
}

// openRouterExtra captures reasoning_details verbatim. Replaying it unchanged is
// the only way a model routed to anthropic keeps its thinking signatures.
func openRouterExtra(raw json.RawMessage, st *compatState) []Event {
	var chunk struct {
		Choices []struct {
			Delta struct {
				ReasoningDetails json.RawMessage `json:"reasoning_details"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &chunk); err != nil {
		return nil
	}
	for _, c := range chunk.Choices {
		if len(c.Delta.ReasoningDetails) > 0 {
			st.details = c.Delta.ReasoningDetails
		}
	}
	return nil
}

// openRouterModels is the GET /models response.
type openRouterModels struct {
	Data []struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		ContextLength int    `json:"context_length"`
		Architecture  struct {
			InputModalities []string `json:"input_modalities"`
		} `json:"architecture"`
		TopProvider struct {
			MaxCompletionTokens int `json:"max_completion_tokens"`
		} `json:"top_provider"`
		SupportedParameters []string `json:"supported_parameters"`
	} `json:"data"`
}

// parseOpenRouterModels turns the catalogue into model entries. Pricing fields
// in the response are deliberately dropped.
func parseOpenRouterModels(body []byte) ([]ModelConfig, error) {
	var wire openRouterModels
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	out := make([]ModelConfig, 0, len(wire.Data))
	for _, d := range wire.Data {
		m := ModelConfig{ID: d.ID, Name: d.Name}
		if d.ContextLength > 0 {
			m.ContextWindow = &d.ContextLength
		}
		if d.TopProvider.MaxCompletionTokens > 0 {
			m.MaxTokens = &d.TopProvider.MaxCompletionTokens
		}
		for _, mod := range d.Architecture.InputModalities {
			switch mod {
			case "text":
				m.Input = append(m.Input, ModalityText)
			case "image":
				m.Input = append(m.Input, ModalityImage)
			}
		}
		if slices.Contains(d.SupportedParameters, "reasoning") ||
			slices.Contains(d.SupportedParameters, "include_reasoning") {
			m.Reasoning = ptrOf(true)
		}
		if slices.Contains(d.SupportedParameters, "tools") {
			m.Compat = &Compat{SupportsToolChoice: ptrOf(true)}
		}
		out = append(out, m)
	}
	return out, nil
}
