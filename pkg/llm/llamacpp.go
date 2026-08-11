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
	var text strings.Builder
	text.WriteString(blocksText(req.System))
	for _, m := range RetainedMessages(req) {
		text.WriteString(countBlocks(m.Content))
	}
	// tool schemas ride in the request and occupy real tokens; count them once
	// when present (an empty list adds nothing to what a no-tool prompt sends).
	if len(req.Tools) > 0 {
		if schemas, err := json.Marshal(compatTools(req.Tools)); err == nil {
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
