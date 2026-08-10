package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"path"
	"strings"
)

// decorateLlamaCpp adds the template switch that turns reasoning on or off, and
// asks the server to reuse its prompt cache.
func decorateLlamaCpp(body *compatRequest, req Request) {
	cache := true
	body.CachePrompt = &cache
	if req.Model.Caps.Reasoning == ReasoningInlineTags {
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
// the real loaded context length, which is why discovery beats configuration
// for this field.
func parseLlamaProps(body []byte) ([]ModelConfig, error) {
	var p llamaProps
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	name := path.Base(p.ModelPath)
	if name == "" || name == "." || name == "/" {
		name = "llamacpp"
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

// llamaTokenize is the /tokenize response.
type llamaTokenize struct {
	Tokens []int `json:"tokens"`
}

// CountTokens returns the exact input token count from the server's own
// tokenizer, which is local and cheap enough to call freely.
func (p *compatProvider) CountTokens(ctx context.Context, req Request) (int, error) {
	if req.Model.Caps.Tokenizer != TokenizerRemoteTokenize {
		return 0, ErrNoTokenizer
	}
	var text strings.Builder
	text.WriteString(blocksText(req.System))
	for _, m := range req.Messages {
		text.WriteString(blocksText(m.Content))
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
