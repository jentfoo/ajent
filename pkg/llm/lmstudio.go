package llm

import (
	"encoding/json"
	"slices"
)

// decorateLMStudio is a no-op today; lm-studio speaks plain chat-completions and
// its quirks are carried by capabilities rather than request fields.
func decorateLMStudio(_ *compatRequest, _ Request) {}

// lmStudioModels is the /api/v0/models response (native REST API before 0.4).
type lmStudioModels struct {
	Data []struct {
		ID                  string   `json:"id"`
		Type                string   `json:"type"`
		State               string   `json:"state"`
		MaxContextLength    int      `json:"max_context_length"`
		LoadedContextLength int      `json:"loaded_context_length"`
		Capabilities        []string `json:"capabilities"`
	} `json:"data"`
}

// lmStudioV1Models is the /api/v1/models response (LM Studio 0.4+).
type lmStudioV1Models struct {
	Models []struct {
		Key              string `json:"key"`
		Type             string `json:"type"`
		DisplayName      string `json:"display_name"`
		MaxContextLength int    `json:"max_context_length"`
		LoadedInstances  []struct {
			Config struct {
				ContextLength int `json:"context_length"`
			} `json:"config"`
		} `json:"loaded_instances"`
		Capabilities struct {
			Vision            bool `json:"vision"`
			TrainedForToolUse bool `json:"trained_for_tool_use"`
		} `json:"capabilities"`
	} `json:"models"`
}

// parseLMStudioModels turns the discovery response into model entries. A loaded
// model reports the context length it was actually loaded with, which is
// smaller than its maximum whenever the user chose so.
func parseLMStudioModels(body []byte) ([]ModelConfig, error) {
	var wire lmStudioModels
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	out := make([]ModelConfig, 0, len(wire.Data))
	for _, d := range wire.Data {
		if d.Type == "embeddings" {
			continue
		}
		m := ModelConfig{ID: d.ID, Name: d.ID}
		if ctx := d.LoadedContextLength; ctx > 0 && d.State == "loaded" {
			m.ContextWindow = &ctx
		} else if d.MaxContextLength > 0 {
			m.ContextWindow = &d.MaxContextLength
		}
		if len(d.Capabilities) > 0 {
			m.Input = []Modality{ModalityText}
			if slices.Contains(d.Capabilities, "vision") {
				m.Input = append(m.Input, ModalityImage)
			}
			m.Compat = &Compat{
				SupportsToolChoice: ptrOf(slices.Contains(d.Capabilities, "tool_use")),
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// parseLMStudioV1Models turns the v1 list into model entries, mirroring
// parseLMStudioModels.
func parseLMStudioV1Models(body []byte) ([]ModelConfig, error) {
	var wire lmStudioV1Models
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	out := make([]ModelConfig, 0, len(wire.Models))
	for _, d := range wire.Models {
		if d.Type == "embedding" || d.Type == "embeddings" {
			continue
		}
		m := ModelConfig{ID: d.Key, Name: d.DisplayName}
		if m.Name == "" {
			m.Name = d.Key
		}
		for _, inst := range d.LoadedInstances {
			if ctx := inst.Config.ContextLength; ctx > 0 {
				m.ContextWindow = &ctx
				break
			}
		}
		if m.ContextWindow == nil && d.MaxContextLength > 0 {
			m.ContextWindow = &d.MaxContextLength
		}
		m.Input = []Modality{ModalityText}
		if d.Capabilities.Vision {
			m.Input = append(m.Input, ModalityImage)
		}
		m.Compat = &Compat{SupportsToolChoice: ptrOf(d.Capabilities.TrainedForToolUse)}
		out = append(out, m)
	}
	return out, nil
}

// ptrOf returns a pointer to v, for building optional configuration fields.
func ptrOf[T any](v T) *T { return &v }
