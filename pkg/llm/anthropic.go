package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

const (
	// minThinkingBudget is the smallest budget the Messages API accepts.
	minThinkingBudget = 1024
	// maxCacheBreakpoints is the API limit on cache_control markers.
	maxCacheBreakpoints = 4
	// longCacheTTL is the extended retention tier, for models that offer it.
	longCacheTTL = "1h"
)

// anthropicProvider serves the Messages API.
type anthropicProvider struct {
	client *httpClient
	name   string
}

// newAnthropicProvider returns a Messages API provider.
func newAnthropicProvider(name string, client *httpClient) *anthropicProvider {
	return &anthropicProvider{client: client, name: name}
}

// Name returns the provider name.
func (p *anthropicProvider) Name() string { return p.name }

// Stream sends a request and returns its normalized event stream.
func (p *anthropicProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	body, err := buildAnthropicBody(req)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.do(ctx, httpReq{
		method: http.MethodPost, path: "/v1/messages", body: body,
		headers: req.Model.Headers, classify: anthropicClassifier(p.name),
	})
	if err != nil {
		return nil, err
	}
	return newAnthropicStream(ctx, resp, p.name), nil
}

// CountTokens returns the exact input token count. It is a billed network round
// trip, so callers use it on demand rather than per keystroke.
func (p *anthropicProvider) CountTokens(ctx context.Context, req Request) (int, error) {
	body, err := buildAnthropicBody(req)
	if err != nil {
		return 0, err
	}
	// count_tokens rejects the streaming and sampling fields
	var trimmed map[string]json.RawMessage
	if err = json.Unmarshal(body, &trimmed); err != nil {
		return 0, err
	}
	for _, k := range []string{"stream", "max_tokens", "temperature"} {
		delete(trimmed, k)
	}
	body, err = json.Marshal(trimmed)
	if err != nil {
		return 0, err
	}

	resp, err := p.client.do(ctx, httpReq{
		method: http.MethodPost, path: "/v1/messages/count_tokens", body: body,
		headers: req.Model.Headers, classify: anthropicClassifier(p.name),
	})
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	var out antCountResponse
	if err = json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.InputTokens, nil
}

// buildAnthropicBody marshals a request into a Messages API body. It is pure,
// so request shape can be asserted without a server.
func buildAnthropicBody(req Request) ([]byte, error) {
	caps := req.Model.Caps
	msgs, err := anthropicMessages(req, caps)
	if err != nil {
		return nil, err
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = req.Model.MaxOutput
	}
	body := antRequest{
		Model:     req.Model.ID,
		MaxTokens: maxTokens,
		Messages:  msgs,
		Stream:    true,
	}
	if len(req.System) > 0 {
		body.System = []antBlock{{Type: antTypeText, Text: blocksText(req.System)}}
	}
	if len(req.Tools) > 0 {
		body.Tools = anthropicTools(req.Tools)
		body.ToolChoice = anthropicToolChoice(req.ToolChoice)
	}

	if budget := thinkingBudget(req, maxTokens); budget > 0 {
		body.Thinking = &antThinking{Type: "enabled", BudgetTokens: budget}
		// the API rejects any temperature but the default while thinking is on,
		// so it is dropped rather than surfaced as an error the user cannot act on
	} else if req.Temperature != nil && caps.Temperature {
		body.Temperature = req.Temperature
	}

	if req.Cache.Enabled && caps.PromptCache {
		applyCacheBreakpoints(&body, req.Cache.KeepLast, caps.LongCache)
	}
	return json.Marshal(body)
}

// thinkingBudget returns the reasoning token budget, or zero when reasoning is
// off or unsupported.
func thinkingBudget(req Request, maxTokens int) int {
	caps := req.Model.Caps
	if caps.Reasoning != ReasoningAnthropicBudget || req.Reasoning.Level == LevelOff {
		return 0
	}
	budget := req.Reasoning.Budget
	if budget <= 0 {
		budget = levelBudget(req.Reasoning.Level, req.Model.MaxOutput)
	}
	if budget < minThinkingBudget {
		budget = minThinkingBudget
	}
	if maxTokens > 0 && budget >= maxTokens {
		budget = maxTokens - 1 // the budget must leave room for the reply
	}
	if budget < minThinkingBudget {
		return 0
	}
	return budget
}

// levelBudget maps a reasoning level onto a token budget.
func levelBudget(l Level, maxOutput int) int {
	var budget int
	switch l {
	case LevelMinimal:
		budget = 1024
	case LevelLow:
		budget = 2048
	case LevelMedium:
		budget = 8192
	case LevelHigh:
		budget = 16384
	case LevelXHigh:
		budget = 32768
	case LevelMax:
		budget = 64000
	default:
		return 0
	}
	if maxOutput > 0 && budget >= maxOutput {
		budget = maxOutput - 1
	}
	return budget
}

// anthropicMessages converts the content model, collapsing to the user and
// assistant roles the API accepts.
func anthropicMessages(req Request, caps Capabilities) ([]antMessage, error) {
	msgs := applyRetention(req.Messages, req.Reasoning.Retain, caps)
	out := make([]antMessage, 0, len(msgs))

	for _, m := range msgs {
		if m.Role == RoleSystem {
			continue // system is a top level field, never a message
		}
		blocks, err := anthropicBlocks(m.Content, caps)
		if err != nil {
			return nil, err
		} else if len(blocks) == 0 {
			continue
		}
		role := roleAssistant
		if m.Role != RoleAssistant {
			role = roleUser // tool results ride on a user message
		}
		// consecutive same role messages must be merged, which is what a tool
		// result following a user turn would otherwise produce
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			continue
		}
		out = append(out, antMessage{Role: role, Content: blocks})
	}
	return out, nil
}

// anthropicBlocks converts content blocks to their wire form.
func anthropicBlocks(blocks BlockList, caps Capabilities) ([]antBlock, error) {
	out := make([]antBlock, 0, len(blocks))
	for _, b := range blocks {
		switch v := b.(type) {
		case TextBlock:
			if v.Text != "" {
				out = append(out, antBlock{Type: antTypeText, Text: v.Text})
			}
		case ThinkingBlock:
			if v.Redacted != "" {
				out = append(out, antBlock{Type: antTypeRedacted, Data: v.Redacted})
			} else if v.Signature != "" {
				out = append(out, antBlock{Type: antTypeThinking, Thinking: v.Text, Signature: v.Signature})
			}
		case ToolCallBlock:
			input := v.Input
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			out = append(out, antBlock{Type: antTypeToolUse, ID: v.ID, Name: v.Name, Input: input})
		case ToolResultBlock:
			nested, err := anthropicBlocks(v.Content, caps)
			if err != nil {
				return nil, err
			}
			out = append(out, antBlock{
				Type: antTypeToolRes, ToolUseID: v.CallID, Content: nested, IsError: v.IsError,
			})
		case ImageBlock:
			if !caps.Images {
				return nil, errors.New("llm: model does not accept image input")
			}
			out = append(out, antBlock{Type: antTypeImage, Source: &antSource{
				Type: "base64", MediaType: v.MediaType,
				Data: base64.StdEncoding.EncodeToString(v.Data),
			}})
		}
	}
	return out, nil
}

// anthropicTools converts tool schemas.
func anthropicTools(tools []ToolSchema) []antTool {
	out := make([]antTool, len(tools))
	for i, t := range tools {
		out[i] = antTool{Name: t.Name, Description: t.Description, InputSchema: t.Parameters}
	}
	return out
}

// anthropicToolChoice renders the tool choice, nil when the default applies.
func anthropicToolChoice(tc ToolChoice) *antToolChoi {
	switch tc.Mode {
	case ToolChoiceNone:
		return &antToolChoi{Type: "none"}
	case ToolChoiceRequired:
		return &antToolChoi{Type: "any"}
	case ToolChoiceSpecific:
		return &antToolChoi{Type: "tool", Name: tc.Name}
	default:
		return nil
	}
}

// applyCacheBreakpoints marks the stable prefix so the cache grows with the
// conversation. Breakpoints are recomputed every request, and the same history
// always produces the same ones.
func applyCacheBreakpoints(body *antRequest, keepLast int, longTTL bool) {
	remaining := maxCacheBreakpoints
	mark := func() *antCache {
		c := &antCache{Type: antCacheEphemeral}
		if longTTL {
			c.TTL = longCacheTTL
		}
		return c
	}

	if n := len(body.System); n > 0 {
		body.System[n-1].CacheControl = mark()
		remaining--
	}
	if n := len(body.Tools); n > 0 && remaining > 0 {
		body.Tools[n-1].CacheControl = mark()
		remaining--
	}
	// the newest message is still changing, so breakpoints start one back
	for i := len(body.Messages) - 2; i >= 0 && keepLast > 0 && remaining > 0; i-- {
		blocks := body.Messages[i].Content
		if len(blocks) == 0 {
			continue
		}
		blocks[len(blocks)-1].CacheControl = mark()
		keepLast--
		remaining--
	}
}

// anthropicClassifier maps a Messages API error response.
func anthropicClassifier(provider string) func(int, []byte) error {
	return func(status int, body []byte) error {
		var wire antErrBody
		_ = json.Unmarshal(body, &wire)

		msg := strings.TrimSpace(string(body))
		var code string
		if wire.Error != nil {
			if wire.Error.Message != "" {
				msg = wire.Error.Message
			}
			code = wire.Error.Type
		}
		err := &APIError{
			Provider: provider, Status: status, Code: code, Message: msg, Body: body,
			Retryable: shouldRetryStatus(status, false),
		}
		if status == http.StatusBadRequest && matchesOverflow(msg, FlavorAnthropic) {
			return err.Overflow()
		}
		return err
	}
}

// anthropicStream decodes a Messages API event stream.
type anthropicStream struct {
	ctx      context.Context
	resp     *http.Response
	sse      *SSEReader
	provider string
	*streamPump

	blocks map[int]*antOpenBlock
	usage  Usage
	stop   StopReason
}

// antOpenBlock is a content block being streamed.
type antOpenBlock struct {
	kind      string
	text      strings.Builder
	signature string
	data      string
	id        string
	name      string
}

func newAnthropicStream(ctx context.Context, resp *http.Response, provider string) *anthropicStream {
	s := &anthropicStream{
		ctx: ctx, resp: resp, provider: provider,
		sse:    NewSSEReader(resp.Body, 0),
		blocks: make(map[int]*antOpenBlock),
	}
	s.streamPump = newStreamPump(resp.Body, s.readFrame)
	return s
}

// readFrame decodes one SSE frame into zero or more events.
func (s *anthropicStream) readFrame() []Event {
	frame, err := s.sse.Next(s.ctx)
	if err != nil {
		return s.finish(err)
	}

	var ev antEvent
	if err = json.Unmarshal(frame.Data, &ev); err != nil {
		return s.finish(err)
	}

	switch ev.Type {
	case "message_start":
		return s.onMessageStart(ev)
	case "content_block_start":
		return s.onBlockStart(ev)
	case "content_block_delta":
		return s.onBlockDelta(ev)
	case "content_block_stop":
		return s.onBlockStop(ev)
	case "message_delta":
		return s.onMessageDelta(ev)
	case "message_stop":
		return s.finish(io.EOF)
	case "error":
		msg := "stream error"
		if ev.Error != nil {
			msg = ev.Error.Message
		}
		return s.finish(&APIError{Provider: s.provider, Message: msg})
	default:
		return nil // ping and anything added later
	}
}

func (s *anthropicStream) onMessageStart(ev antEvent) []Event {
	out := []Event{{Type: EventMessageStart}}
	if ev.Message != nil {
		out[0].Meta = &StreamMeta{Model: ev.Message.Model, RequestID: ev.Message.ID}
		if ev.Message.Usage != nil {
			s.usage = ev.Message.Usage.toUsage()
			out = append(out, Event{Type: EventUsage, Usage: s.usage})
		}
	}
	return out
}

func (s *anthropicStream) onBlockStart(ev antEvent) []Event {
	if ev.ContentBlock == nil {
		return nil
	}
	b := &antOpenBlock{kind: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
	if ev.ContentBlock.Type == antTypeRedacted {
		b.data = ev.ContentBlock.Data
	}
	s.blocks[ev.Index] = b

	switch b.kind {
	case antTypeThinking, antTypeRedacted:
		return []Event{{Type: EventThinkingStart, Index: ev.Index}}
	case antTypeText:
		return []Event{{Type: EventTextStart, Index: ev.Index}}
	case antTypeToolUse:
		return []Event{{Type: EventToolCallStart, Index: ev.Index, ToolCallID: b.id, ToolName: b.name}}
	default:
		return nil
	}
}

func (s *anthropicStream) onBlockDelta(ev antEvent) []Event {
	b := s.blocks[ev.Index]
	if b == nil || ev.Delta == nil {
		return nil
	}
	switch ev.Delta.Type {
	case "text_delta":
		b.text.WriteString(ev.Delta.Text)
		return []Event{{Type: EventTextDelta, Index: ev.Index, Text: ev.Delta.Text}}
	case "thinking_delta":
		b.text.WriteString(ev.Delta.Thinking)
		return []Event{{Type: EventThinkingDelta, Index: ev.Index, Text: ev.Delta.Thinking}}
	case "signature_delta":
		b.signature += ev.Delta.Signature // accumulated, emitted with the end event
		return nil
	case "input_json_delta":
		b.text.WriteString(ev.Delta.PartialJSON)
		return []Event{{Type: EventToolCallDelta, Index: ev.Index,
			ToolCallID: b.id, Text: ev.Delta.PartialJSON}}
	default:
		return nil
	}
}

func (s *anthropicStream) onBlockStop(ev antEvent) []Event {
	b := s.blocks[ev.Index]
	if b == nil {
		return nil
	}
	delete(s.blocks, ev.Index)

	switch b.kind {
	case antTypeThinking, antTypeRedacted:
		return []Event{{Type: EventThinkingEnd, Index: ev.Index, Block: ThinkingBlock{
			Text: b.text.String(), Signature: b.signature, Redacted: b.data,
		}}}
	case antTypeText:
		return []Event{{Type: EventTextEnd, Index: ev.Index,
			Block: TextBlock{Text: b.text.String()}}}
	case antTypeToolUse:
		input, err := finishToolInput(b.text.String())
		return []Event{{Type: EventToolCallEnd, Index: ev.Index,
			ToolCallID: b.id, ToolName: b.name,
			Block: ToolCallBlock{ID: b.id, Name: b.name, Input: input}, Err: err}}
	default:
		return nil
	}
}

func (s *anthropicStream) onMessageDelta(ev antEvent) []Event {
	var out []Event
	if ev.Delta != nil && ev.Delta.StopReason != "" {
		s.stop = antStopReason(ev.Delta.StopReason)
	}
	if ev.Usage != nil {
		// message_delta reports only the output side, the rest came at the start
		u := ev.Usage.toUsage()
		s.usage.Output = u.Output
		if u.Input > 0 {
			s.usage.Input = u.Input
		}
		out = append(out, Event{Type: EventUsage, Usage: s.usage})
	}
	return out
}

// finish drains any open block and emits the terminal event.
func (s *anthropicStream) finish(cause error) []Event {
	if s.done {
		return nil
	}
	s.done = true

	var events []Event
	stop := s.stop
	var streamErr error
	if cause != nil && !errors.Is(cause, io.EOF) {
		streamErr = cause
		stop = StopError
		s.err = cause
	} else if stop == StopUnknown {
		stop = StopEndTurn
	}
	return append(events, Event{Type: EventDone, StopReason: stop, Usage: s.usage, Err: streamErr})
}
