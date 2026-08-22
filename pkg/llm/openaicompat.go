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

// compatProfile is what one provider changes about the shared chat-completions
// dialect. A provider needing a fourth hook belongs in the shared layer instead.
type compatProfile struct {
	name     string
	path     string                                             // defaults to /chat/completions
	decorate func(body *compatRequest, req Request)             // provider only request fields
	extra    func(raw json.RawMessage, st *compatState) []Event // provider only delta fields
	classify func(status int, body []byte) error
}

// compatProvider serves the chat-completions dialect for one endpoint.
type compatProvider struct {
	client  *httpClient
	profile compatProfile
}

// Name returns the provider name.
func (p *compatProvider) Name() string { return p.profile.name }

// Stream sends a request and returns its normalized event stream.
func (p *compatProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	body, err := buildCompatBody(req, p.profile)
	if err != nil {
		return nil, err
	}
	path := p.profile.path
	if path == "" {
		path = "/chat/completions"
	}
	resp, err := p.client.do(ctx, httpReq{
		method: http.MethodPost, path: path, body: body,
		headers: withSessionHeaders(req.Model.Headers, req), classify: p.profile.classify,
	})
	if err != nil {
		return nil, err
	}
	return newCompatStream(ctx, resp, req.Model.Caps, p.profile), nil
}

// buildCompatBody marshals a request into a chat-completions body. It is pure,
// so request shape can be asserted without a server.
func buildCompatBody(req Request, profile compatProfile) ([]byte, error) {
	req = Prepare(req)
	caps := req.Model.Caps
	msgs, err := compatMessages(req, caps)
	if err != nil {
		return nil, err
	}

	body := compatRequest{
		Model:    req.Model.ID,
		Messages: msgs,
		Stream:   true,
	}
	if caps.StreamUsage {
		body.StreamOptions = &compatStreamOpts{IncludeUsage: true}
	}
	if req.MaxTokens > 0 {
		if caps.MaxTokensField == fieldMaxCompletion {
			body.MaxCompletion = &req.MaxTokens
		} else {
			body.MaxTokens = &req.MaxTokens
		}
	}
	if req.Temperature != nil && caps.Temperature {
		body.Temperature = req.Temperature
	}
	if len(req.Tools) > 0 {
		body.Tools = compatTools(req.Tools, caps.SupportsStrict)
		if caps.ToolChoice {
			body.ToolChoice = compatToolChoice(req.ToolChoice)
		}
		if caps.ParallelTools {
			t := true
			body.ParallelToolCalls = &t
		}
		if caps.ZaiToolStream {
			body.ToolStream = ptrOf(true)
		}
	}
	applyThinking(&body, req) // runs after the max-tokens fields for its budget read
	if profile.decorate != nil {
		profile.decorate(&body, req)
	}
	// configured extra keys win over everything, including our own dynamic ones
	return marshalWithExtra(body, mergeExtra(body.extra, caps.ExtraBody))
}

// marshalWithExtra folds any configured extra body keys into the request, which
// is the escape hatch for vendor parameters we do not model.
func marshalWithExtra(body compatRequest, extra map[string]json.RawMessage) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil || len(extra) == 0 {
		return data, err
	}
	var merged map[string]json.RawMessage
	if err = json.Unmarshal(data, &merged); err != nil {
		return nil, err
	}
	for k, v := range extra {
		merged[k] = v
	}
	return json.Marshal(merged)
}

// compatMessages flattens the content model onto chat-completions messages.
func compatMessages(req Request, caps Capabilities) ([]compatMessage, error) {
	msgs := req.Messages // already normalized by Prepare
	out := make([]compatMessage, 0, len(msgs)+1)
	// whether reasoning is on for this turn, which gates the empty replay field
	on := caps.Reasoning && clampLevel(caps, req.Reasoning.Level) != LevelOff

	if len(req.System) > 0 {
		role := roleSystem
		if caps.DeveloperRole {
			role = roleDeveloper
		}
		out = append(out, compatMessage{Role: role, Content: blocksText(req.System)})
	}
	for _, m := range msgs {
		converted, err := compatMessageFor(m, caps, on)
		if err != nil {
			return nil, err
		}
		out = append(out, converted...)
	}
	return out, nil
}

// compatMessageFor converts one message, which may expand into several when it
// carries tool results. on reports whether this turn reasons at all.
func compatMessageFor(m Message, caps Capabilities, on bool) ([]compatMessage, error) {
	// tool results become their own tool role messages
	var results []compatMessage
	for _, b := range m.Content {
		if tr, ok := b.(ToolResultBlock); ok {
			rm := compatMessage{
				Role: roleTool, ToolCallID: tr.CallID, Content: blocksText(tr.Content),
			}
			if caps.RequiresToolResultName && tr.ToolName != "" {
				rm.Name = tr.ToolName
			}
			results = append(results, rm)
		}
	}
	if len(results) > 0 && len(results) == len(m.Content) {
		return results, nil
	}

	msg := compatMessage{Role: string(m.Role)}
	if m.Role == RoleSystem && caps.DeveloperRole {
		msg.Role = roleDeveloper
	}
	parts, text, err := compatContent(m.Content, caps)
	if err != nil {
		return nil, err
	}
	if len(parts) > 0 {
		msg.Content = parts
	} else if text != "" {
		msg.Content = text
	}
	var thinkingTexts []string
	for _, b := range m.Content {
		switch v := b.(type) {
		case ToolCallBlock:
			msg.ToolCalls = append(msg.ToolCalls, compatToolCall{
				ID: v.ID, Type: typeFunction,
				Function: compatToolFunction{Name: v.Name, Arguments: string(v.Input)},
			})
		case ThinkingBlock:
			if strings.TrimSpace(v.Text) != "" {
				thinkingTexts = append(thinkingTexts, v.Text)
			}
			if len(v.Details) > 0 {
				msg.ReasoningDetails = v.Details
			}
		}
	}

	// reasoning replay rides the configured field or a surviving block's own source;
	// an empty field is forced only when this turn reasons, which keeps deepseek-style
	// models in thinking mode across turns.
	if len(thinkingTexts) > 0 {
		if msg.reasoningField = resolveReasonField(m, caps); msg.reasoningField != "" {
			msg.reasoningText = strings.Join(thinkingTexts, "\n")
		}
	} else if on && m.Role == RoleAssistant {
		// force an empty reasoning field so a model stays in thinking mode across
		// turns; this defaults on for deepseek via detection and is user-overridable.
		// think-tags models never get it: they parse tags from content, not fields.
		if caps.ReplayReasoning {
			msg.reasoningField = resolveReasonField(m, caps)
			if msg.reasoningField == "" {
				msg.reasoningField = "reasoning_content"
			}
		}
	}
	// skip messages with neither content nor tool calls, which providers reject,
	// unless a bridging assistant placeholder must be emitted as explicit empty text.
	if msg.Content == nil && len(msg.ToolCalls) == 0 {
		if caps.RequiresAssistantAfterToolResult && m.Role == RoleAssistant {
			msg.Content = ""
		} else {
			return results, nil
		}
	}
	return append(results, msg), nil
}

// compatContent renders blocks as a plain string, or as parts when an image is
// present.
func compatContent(blocks BlockList, caps Capabilities) ([]compatPart, string, error) {
	var hasImage bool
	for _, b := range blocks {
		if _, ok := b.(ImageBlock); ok {
			hasImage = true
			break
		}
	}
	if !hasImage {
		return nil, blocksText(blocks), nil
	}
	if !caps.Images {
		return nil, "", errors.New("llm: model does not accept image input")
	}

	var parts []compatPart
	for _, b := range blocks {
		switch v := b.(type) {
		case TextBlock:
			parts = append(parts, compatPart{Type: "text", Text: v.Text})
		case ImageBlock:
			parts = append(parts, compatPart{Type: "image_url", ImageURL: &compatImageURL{
				URL: "data:" + v.MediaType + ";base64," + base64.StdEncoding.EncodeToString(v.Data),
			}})
		}
	}
	return parts, "", nil
}

// resolveReasonField returns which key reasoning replays under: the configured
// field, else a surviving thinking block's own originating source.
func resolveReasonField(m Message, caps Capabilities) string {
	if caps.ReasoningField != "" {
		return caps.ReasoningField
	}
	for _, b := range m.Content {
		if tb, ok := b.(ThinkingBlock); ok && tb.Field != "" {
			return tb.Field
		}
	}
	return ""
}

// blocksText joins the text of every non-empty text block with a newline. This
// differs from pi's separator-less join for assistant parts, which is only hit by
// responses phase blocks replayed onto compat.
func blocksText(blocks BlockList) string {
	var texts []string
	for _, blk := range blocks {
		if t, ok := blk.(TextBlock); ok && t.Text != "" {
			texts = append(texts, t.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// compatTools converts tool schemas. strict applies the provider's default wire
// shape (strict: false) when the capability gate is set.
func compatTools(tools []ToolSchema, strict bool) []compatTool {
	out := make([]compatTool, len(tools))
	for i, t := range tools {
		schema := compatToolSchema{Name: t.Name, Description: t.Description, Parameters: t.Parameters}
		if strict {
			schema.Strict = ptrOf(false)
		}
		out[i] = compatTool{Type: typeFunction, Function: schema}
	}
	return out
}

// compatToolChoice renders the tool choice, nil when the default applies.
func compatToolChoice(tc ToolChoice) any {
	switch tc.Mode {
	case ToolChoiceNone:
		return "none"
	case ToolChoiceRequired:
		return "required"
	case ToolChoiceSpecific:
		return map[string]any{
			"type": typeFunction, typeFunction: map[string]string{"name": tc.Name},
		}
	default:
		return nil
	}
}

// compatState is the mutable decode state one stream carries.
type compatState struct {
	caps        Capabilities
	split       *thinkSplitter
	tools       *toolAccumulator
	nextBlock   int
	thinkIdx    int
	textIdx     int
	thinkBuf    strings.Builder // accumulated for the end block
	textBuf     strings.Builder
	reasonField string // delta field that supplied the thinking text
	details     json.RawMessage
	usage       Usage
	stop        StopReason
	sawMeta     bool
}

// compatStream decodes a chat-completions event stream.
type compatStream struct {
	ctx     context.Context
	resp    *http.Response
	sse     *SSEReader
	profile compatProfile
	st      *compatState
	*streamPump
}

func newCompatStream(ctx context.Context, resp *http.Response, caps Capabilities, profile compatProfile) *compatStream {
	s := &compatStream{
		ctx:     ctx,
		resp:    resp,
		sse:     NewSSEReader(resp.Body, 0),
		profile: profile,
		st: &compatState{
			caps:     caps,
			split:    newThinkSplitter(caps.ThinkOpen, caps.ThinkClose),
			thinkIdx: -1,
			textIdx:  -1,
		},
	}
	s.streamPump = newStreamPump(resp.Body, s.readFrame)
	return s
}

// readFrame decodes one SSE frame into zero or more events.
func (s *compatStream) readFrame() []Event {
	frame, err := s.sse.Next(s.ctx)
	if err != nil {
		return s.finish(err)
	} else if frame.IsDone() {
		return s.finish(io.EOF)
	}

	var chunk compatChunk
	if err = json.Unmarshal(frame.Data, &chunk); err != nil {
		return s.finish(err)
	}
	if chunk.Error != nil {
		return s.finish(&APIError{
			Provider: s.profile.name, Code: chunk.Error.codeString(), Message: chunk.Error.Message,
		})
	}

	var events []Event
	if !s.st.sawMeta {
		s.st.sawMeta = true
		events = append(events, Event{
			Type: EventMessageStart, Meta: &StreamMeta{Model: chunk.Model, RequestID: chunk.ID},
		})
	}
	if chunk.Usage != nil {
		s.st.usage = chunk.Usage.toUsage()
		events = append(events, Event{Type: EventUsage, Usage: s.st.usage})
	}
	for _, c := range chunk.Choices {
		events = append(events, s.decodeDelta(c)...)
	}
	if s.profile.extra != nil {
		events = append(events, s.profile.extra(frame.Data, s.st)...)
	}
	return events
}

// decodeDelta turns one choice delta into events.
func (s *compatStream) decodeDelta(c compatChoi) []Event {
	st := s.st
	var events []Event

	// pi reads the first non-empty of reasoning_content, reasoning, reasoning_text;
	// chutes.ai sends the same text in two fields and this order picks the right one.
	reasoning, field := deltaReasonText(c.Delta)
	if field != "" {
		st.reasonField = field
	}
	content := c.Delta.Content
	if st.caps.ThinkOpen != "" && content != "" {
		var thinking string
		content, thinking = st.split.Write(content)
		reasoning += thinking
	}
	if len(c.Delta.ReasoningDetails) > 0 {
		st.details = c.Delta.ReasoningDetails
	}

	if reasoning != "" {
		if st.thinkIdx < 0 {
			st.thinkIdx = st.nextBlock
			st.nextBlock++
			events = append(events, Event{Type: EventThinkingStart, Index: st.thinkIdx})
		}
		st.thinkBuf.WriteString(reasoning)
		events = append(events, Event{Type: EventThinkingDelta, Index: st.thinkIdx, Text: reasoning})
	}
	if content != "" {
		if st.thinkIdx >= 0 && st.textIdx < 0 {
			events = append(events, s.endThinking())
		}
		if st.textIdx < 0 {
			st.textIdx = st.nextBlock
			st.nextBlock++
			events = append(events, Event{Type: EventTextStart, Index: st.textIdx})
		}
		st.textBuf.WriteString(content)
		events = append(events, Event{Type: EventTextDelta, Index: st.textIdx, Text: content})
	}

	for _, tc := range c.Delta.ToolCalls {
		if st.tools == nil {
			st.tools = newToolAccumulator(st.nextBlock)
		}
		events = append(events, st.tools.Delta(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)...)
	}
	if c.FinishReason != "" {
		st.stop = stopReasonFrom(c.FinishReason)
	}
	return events
}

// endThinking closes the thinking block, carrying the replay tokens and its
// originating delta field. It is idempotent so both the text transition and the
// terminal drain may call it.
func (s *compatStream) endThinking() Event {
	st := s.st
	idx := st.thinkIdx
	st.thinkIdx = -2 // closed, so a later drain does not repeat it
	return Event{Type: EventThinkingEnd, Index: idx,
		Block: ThinkingBlock{Text: st.thinkBuf.String(), Details: st.details, Field: st.reasonField}}
}

// deltaReasonText returns the first non-empty reasoning field and its name.
func deltaReasonText(d compatDelta) (text, field string) {
	switch {
	case d.ReasoningContent != "":
		return d.ReasoningContent, "reasoning_content"
	case d.Reasoning != "":
		return d.Reasoning, "reasoning"
	case d.ReasoningText != "":
		return d.ReasoningText, "reasoning_text"
	default:
		return "", ""
	}
}

// finish drains the accumulators and emits the terminal events.
func (s *compatStream) finish(cause error) []Event {
	if s.done {
		return nil
	}
	s.done = true

	var events []Event
	st := s.st
	if st.caps.ThinkOpen != "" {
		text, thinking := st.split.Flush()
		if thinking != "" && st.thinkIdx >= 0 {
			events = append(events, Event{Type: EventThinkingDelta, Index: st.thinkIdx, Text: thinking})
		}
		if text != "" && st.textIdx >= 0 {
			events = append(events, Event{Type: EventTextDelta, Index: st.textIdx, Text: text})
		}
	}
	if st.thinkIdx >= 0 {
		events = append(events, s.endThinking())
	}
	if st.textIdx >= 0 {
		events = append(events, Event{Type: EventTextEnd, Index: st.textIdx,
			Block: TextBlock{Text: st.textBuf.String()}})
	}
	if st.tools != nil {
		closed := st.tools.Close()
		events = append(events, closed...)
		for _, ev := range closed {
			if ev.Err != nil && s.err == nil {
				s.err = ev.Err
			}
		}
	}

	stop := st.stop
	var streamErr error
	if cause != nil && !errors.Is(cause, io.EOF) {
		streamErr = cause
		stop = StopError
		s.err = cause
	} else if stop == StopUnknown {
		// no finish reason arrived: infer for providers that never send one, or an
		// interrupted stream; otherwise a truncated stream is surfaced as an error.
		if !st.caps.SupportsFinishReason || s.ctx.Err() != nil {
			if st.tools != nil {
				stop = StopToolUse
			} else {
				stop = StopEndTurn
			}
		} else {
			streamErr = errors.New("llm: stream ended without finish_reason")
			stop = StopError
			s.err = streamErr
		}
	}
	return append(events, Event{Type: EventDone, StopReason: stop, Usage: st.usage, Err: streamErr})
}
