package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// decorateLlamaCpp adds the template switch that turns reasoning on or off, and
// asks the server to reuse its prompt cache.
func decorateLlamaCpp(body *compatRequest, req Request) {
	cache := true
	body.CachePrompt = &cache
	if req.Model.Caps.ThinkOpen != "" {
		body.ChatTemplateKwarg = map[string]any{
			"enable_thinking": req.Reasoning.Level != LevelOff,
		}
	}
}

// llamaProps is the subset of /props that describes the loaded model.
type llamaProps struct {
	DefaultGenerationSettings struct {
		NCtx int `json:"n_ctx"`
	} `json:"default_generation_settings"`
	ModelPath   string `json:"model_path"`
	TotalSlots  int    `json:"total_slots"`
	ChatFormat  string `json:"chat_format"`
	BuildInfo   string `json:"build_info"`
	NCtxPerSlot int    `json:"n_ctx_per_slot"`
}

// parseLlamaProps turns /props into a single model entry. Only the server knows
// the real loaded context length, which is why discovery beats configuration for
// this field. A multi-model router reports no single loaded model, so it yields an
// empty result and lets discovery fall back to /v1/models.
func parseLlamaProps(body []byte) ([]ModelConfig, error) {
	var p llamaProps
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	// a router reports no loaded model path; nothing here is worth naming
	if p.ModelPath == "" || p.ModelPath == "none" {
		return nil, nil
	}
	name := path.Base(p.ModelPath)
	// a degenerate base name has nothing to identify the model by either
	if name == "." || name == "/" {
		return nil, nil
	}
	ctx := p.DefaultGenerationSettings.NCtx
	if ctx == 0 {
		ctx = p.NCtxPerSlot
	}
	m := ModelConfig{ID: name, Name: name}
	if ctx > 0 {
		m.ContextWindow = &ctx
	}
	return []ModelConfig{m}, nil
}

// countBlocks renders a message's content into token-countable text, including
// tool calls and results that blocksText skips. Images contribute a size marker.
func countBlocks(blocks BlockList) string {
	var b strings.Builder
	for _, blk := range blocks {
		switch t := blk.(type) {
		case TextBlock:
			b.WriteString(t.Text)
		case ThinkingBlock:
			b.WriteString(t.Text)
		case ToolCallBlock:
			if j, err := json.Marshal(map[string]any{"name": t.Name, "arguments": rawJSON(t.Input)}); err == nil {
				b.Write(j)
			}
		case ToolResultBlock:
			b.WriteString(countBlocks(t.Content))
		case ImageBlock:
			b.WriteString("[[image " + strconv.Itoa(len(t.Data)) + " bytes]]")
		}
	}
	return b.String()
}

// rawJSON returns the raw arguments as-is, or an empty object when malformed.
func rawJSON(raw json.RawMessage) any {
	if len(raw) == 0 || !json.Valid(raw) {
		return map[string]any{}
	}
	var v any
	_ = json.Unmarshal(raw, &v)
	return v
}

// llamaTokenize is the /tokenize response.
type llamaTokenize struct {
	Tokens []int `json:"tokens"`
}

// CountTokens returns the exact input token count from the server's own
// tokenizer, which is local and cheap enough to call freely. It renders every
// block a request would send so tool schemas, calls, results and images are not
// silently counted as zero.
func (p *compatProvider) CountTokens(ctx context.Context, req Request) (int, error) {
	if req.Model.Caps.Tokenizer != TokenizerRemoteTokenize {
		return 0, ErrNoTokenizer
	}
	req = Prepare(req)
	var text strings.Builder
	text.WriteString(blocksText(req.System))
	for _, m := range req.Messages {
		text.WriteString(countBlocks(m.Content))
	}
	// tool schemas ride in the request and occupy real tokens; count them once
	// when present (an empty list adds nothing to what a no-tool prompt sends).
	if len(req.Tools) > 0 {
		if schemas, err := json.Marshal(compatTools(req.Tools, req.Model.Caps.SupportsStrict)); err == nil {
			text.Write(schemas)
		}
	}

	body, err := json.Marshal(map[string]string{"content": text.String()})
	if err != nil {
		return 0, err
	}
	resp, err := p.client.do(ctx, httpReq{
		method: http.MethodPost, path: "/tokenize", body: body, classify: p.profile.classify,
	})
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	var out llamaTokenize
	if err = json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return len(out.Tokens), nil
}

// openAIModels is the standard chat-completions /v1/models response. The status
// and meta fields are optional extras llama.cpp routers attach; other servers omit
// them, in which case every listed model stays available.
type openAIModels struct {
	Data []struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Status struct {
			Value string `json:"value"` // llama.cpp router: "loaded" or "unloaded"
		} `json:"status"`
		Meta struct {
			NCtx int `json:"n_ctx"`
		} `json:"meta"`
		Architecture struct {
			InputModalities []string `json:"input_modalities"`
		} `json:"architecture"`
	} `json:"data"`
}

// parseOpenAIModels turns the standard /v1/models list into model entries. It is
// the common denominator every chat-completions server speaks, so it backs up a
// flavor whose own endpoint cannot describe what is loaded (a llama.cpp router). A
// router marks models it has not yet swapped in as unloaded; those are dropped so
// only actually-available models surface. Its meta.n_ctx carries the real context
// window, which beats configuration for that field.
func parseOpenAIModels(body []byte) ([]ModelConfig, error) {
	var wire openAIModels
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	out := make([]ModelConfig, 0, len(wire.Data))
	for _, d := range wire.Data {
		// a blank id names nothing usable
		if d.ID == "" {
			continue
		}
		// a plain OpenAI server reports no status; only an explicit unloaded drops it
		if d.Status.Value == "unloaded" {
			continue
		}
		m := ModelConfig{ID: d.ID, Name: d.ID}
		var input []Modality
		for _, mod := range d.Architecture.InputModalities {
			switch Modality(mod) {
			case ModalityText:
				input = append(input, ModalityText)
			case ModalityImage:
				input = append(input, ModalityImage)
			}
		}
		if len(input) > 0 {
			m.Input = input
		}
		if d.Meta.NCtx > 0 {
			m.ContextWindow = &d.Meta.NCtx
		}
		out = append(out, m)
	}
	return out, nil
}
