package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"strings"
)

const (
	// minThinkingBudget is the smallest budget the Messages API accepts.
	minThinkingBudget = 1024
	// betaInterleavedThinking enables interleaved thinking, which streams each
	// thought as it happens; without it thoughts arrive only at message_stop.
	betaInterleavedThinking = "interleaved-thinking-2025-05-14"
	// betaFineGrainedTools streams tool input arguments in smaller deltas when
	// eager streaming is off.
	betaFineGrainedTools = "fine-grained-tool-streaming-2025-05-14"
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
		headers: anthropicHeaders(req), classify: anthropicClassifier(p.name),
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
	for _, k := range []string{"stream", "max_tokens", "temperature", "output_config"} {
		delete(trimmed, k)
	}
	body, err = json.Marshal(trimmed)
	if err != nil {
		return 0, err
	}

	resp, err := p.client.do(ctx, httpReq{
		method: http.MethodPost, path: "/v1/messages/count_tokens", body: body,
		headers: anthropicHeaders(req), classify: anthropicClassifier(p.name),
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
	req = Prepare(req)
	caps := req.Model.Caps
	msgs, err := anthropicMessages(req, caps)
	if err != nil {
		return nil, err
	}

	level := clampLevel(caps, req.Reasoning.Level)
	on := caps.Dialect == DialectAnthropic && caps.Reasoning && level != LevelOff

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = req.Model.MaxOutput
	}
	// the thinking budget eats into max_tokens, so it is inflated to keep a full
	// answer window; adaptive models replace the budget with an effort instead
	var budget int
	if on && !caps.ForceAdaptiveThinking {
		maxTokens += levelBudgetFor(req, level)
		if req.Model.MaxOutput > 0 {
			maxTokens = min(maxTokens, req.Model.MaxOutput)
		}
		budget = thinkingBudget(caps, level, req.Reasoning.Budget, maxTokens)
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
		body.Tools = anthropicTools(req.Tools, caps.EagerToolInputStreaming)
		body.ToolChoice = anthropicToolChoice(req.ToolChoice)
	}

	switch {
	case on && caps.ForceAdaptiveThinking:
		// display summarized keeps thinking text streaming on adaptive models
		body.Thinking = &antThinking{Type: antThinkAdaptive, Display: "summarized"}
		if effort := anthropicEffort(caps, level); effort != "" {
			body.OutputConfig = &antOutputConfig{Effort: effort}
		}
	case on && budget > 0:
		body.Thinking = &antThinking{Type: antThinkEnabled, BudgetTokens: budget,
			Display: "summarized"}
	case !on && caps.Dialect == DialectAnthropic && caps.Reasoning && !offSuppressed(caps):
		// an explicit off shape matches pi and keeps the deepseek parity locked in B
		body.Thinking = &antThinking{Type: antThinkDisabled}
	default:
	}

	if body.Thinking == nil || body.Thinking.Type == antThinkDisabled {
		if req.Temperature != nil && caps.Temperature {
			// the API rejects any temperature but the default while thinking is on,
			// so it is dropped rather than surfaced as an error the user cannot act on
			body.Temperature = req.Temperature
		}
	}

	if req.Cache.Enabled && caps.PromptCache {
		applyCacheBreakpoints(&body, req.Cache.KeepLast, caps.LongCache, caps.CacheControlOnTools)
	}
	return json.Marshal(body)
}

// levelBudgetFor returns the request's explicit reasoning budget, or the model's
// configured budget for the level.
func levelBudgetFor(req Request, l Level) int {
	if req.Reasoning.Budget > 0 {
		return req.Reasoning.Budget
	}
	return req.Model.Caps.Budgets[l]
}

// thinkingBudget returns the reasoning token budget for a resolved level, or zero
// when it cannot fit under maxTokens with the answer floor kept.
func thinkingBudget(caps Capabilities, level Level, explicitBudget int, maxTokens int) int {
	if caps.Dialect != DialectAnthropic || !caps.Reasoning || level == LevelOff {
		return 0
	}
	budget := explicitBudget
	if budget <= 0 {
		budget = caps.Budgets[level]
	}
	// the API rejects budgets below its minimum, so small explicit ones are raised
	if budget < minThinkingBudget {
		budget = minThinkingBudget
	}
	// the shared answer floor keeps reasoning from eating the whole output, and a
	// max_tokens too small to leave any room drops thinking entirely
	headroom := max(0, maxTokens-minAnswerTokens)
	if budget > headroom {
		budget = headroom
	}
	if budget < minThinkingBudget {
		return 0
	}
	return budget
}

// anthropicEffort returns the adaptive-thinking effort for a level: the model's
// configured value when it has one, else low for minimal and low, medium for
// medium, and high for everything above.
func anthropicEffort(caps Capabilities, l Level) string {
	if v, ok := caps.LevelMap[l]; ok && v != nil {
		return *v
	}
	switch l {
	case LevelMinimal, LevelLow:
		return "low"
	case LevelMedium:
		return "medium"
	default:
		return "high"
	}
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
	msgs := req.Messages // already normalized by Prepare
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
			sig := strings.TrimSpace(v.Signature)
			switch {
			case v.Redacted != "":
				out = append(out, antBlock{Type: antTypeRedacted, Data: v.Redacted})
			case sig != "": // a signature replays even when the text is empty
				out = append(out, antBlock{Type: antTypeThinking, Thinking: v.Text, Signature: &sig})
			case strings.TrimSpace(v.Text) == "":
				continue // nothing to send
			case caps.AllowEmptySignature:
				out = append(out, antBlock{Type: antTypeThinking, Thinking: v.Text, Signature: ptrOf("")})
			default:
				// an aborted stream's reasoning survives as visible text
				out = append(out, antBlock{Type: antTypeText, Text: v.Text})
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

// anthropicHeaders returns the request headers: model headers with an
// anthropic-beta value merged over them. It never mutates req.Model.Headers.
func anthropicHeaders(req Request) map[string]string {
	caps := req.Model.Caps
	var values []string
	if len(req.Tools) > 0 && !caps.EagerToolInputStreaming {
		values = append(values, betaFineGrainedTools)
	}
	// interleaving is built into the adaptive shape, so it is only requested for
	// budget models that actually reason
	if caps.Reasoning && !caps.ForceAdaptiveThinking {
		values = append(values, betaInterleavedThinking)
	}
	base := req.Model.Headers
	if len(values) > 0 {
		base = maps.Clone(base)
		if base == nil {
			base = make(map[string]string, 1)
		}
		// pi joins in order and last-wins over a model-declared beta value
		base["anthropic-beta"] = strings.Join(values, ",")
	}
	return withSessionHeaders(base, req)
}

// sessionAffinityKeys returns the header names to carry SessionID for caps,
// empty when no affinity applies.
func sessionAffinityKeys(caps Capabilities) []string {
	if !caps.SessionAffinity {
		return nil
	}
	switch caps.Dialect {
	case DialectOpenAIResponses:
		switch caps.SessionAffinityFormat {
		case openRouterAffinityFormat:
			return []string{"x-session-id"}
		case "openai-nosession":
			return []string{"x-client-request-id"}
		default: // openai
			return []string{"session_id", "x-client-request-id"}
		}
	case DialectAnthropic:
		return []string{"x-session-affinity"}
	default: // chat-completions
		switch caps.SessionAffinityFormat {
		case openRouterAffinityFormat:
			return []string{"x-session-id"}
		case "openai-nosession":
			return []string{"x-client-request-id", "x-session-affinity"}
		default: // openai
			return []string{"session_id", "x-client-request-id", "x-session-affinity"}
		}
	}
}

// withSessionHeaders overlays the session-affinity keys onto base, cloning only
// when something is added so the common path allocates nothing.
func withSessionHeaders(base map[string]string, req Request) map[string]string {
	keys := sessionAffinityKeys(req.Model.Caps)
	if len(keys) == 0 || req.SessionID == "" {
		return base
	}
	out := maps.Clone(base)
	if out == nil {
		out = make(map[string]string, len(keys))
	}
	for _, k := range keys {
		out[k] = req.SessionID
	}
	return out
}

// anthropicTools converts tool schemas.
func anthropicTools(tools []ToolSchema, eagerStreaming bool) []antTool {
	out := make([]antTool, len(tools))
	for i, t := range tools {
		out[i] = antTool{Name: t.Name, Description: t.Description, InputSchema: t.Parameters}
		if eagerStreaming {
			out[i].EagerInputStreaming = ptrOf(true)
		}
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
func applyCacheBreakpoints(body *antRequest, keepLast int, longTTL bool, cacheOnTools bool) {
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
	// a model that cannot cache tool definitions leaves the budget for prompts
	if cacheOnTools {
		if n := len(body.Tools); n > 0 && remaining > 0 {
			body.Tools[n-1].CacheControl = mark()
			remaining--
		}
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
		if status == http.StatusBadRequest {
			if matchesOverflow(msg, FlavorAnthropic) {
				return err.Overflow()
			}
			err.Hint = anthropicCompatHint(msg)
		}
		return err
	}
}

// anthropicCompatHint names the compat flag a rejection is asking for. These two
// default to the value newer models reject, and the provider's message never
// mentions the setting that would fix it.
func anthropicCompatHint(msg string) string {
	l := strings.ToLower(msg)
	switch {
	case strings.Contains(l, "temperature"):
		return `set compat.supportsTemperature to false for this model`
	case strings.Contains(l, "thinking"), strings.Contains(l, "effort"):
		return `set compat.forceAdaptiveThinking to true for this model`
	}
	return ""
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
