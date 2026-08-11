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
		headers: req.Model.Headers, classify: p.profile.classify,
	})
	if err != nil {
		return nil, err
	}
	return newCompatStream(ctx, resp, req.Model.Caps, p.profile), nil
}

// buildCompatBody marshals a request into a chat-completions body. It is pure,
// so request shape can be asserted without a server.
func buildCompatBody(req Request, profile compatProfile) ([]byte, error) {
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
		body.Tools = compatTools(req.Tools)
		if caps.ToolChoice {
			body.ToolChoice = compatToolChoice(req.ToolChoice)
		}
		if caps.ParallelTools {
			t := true
			body.ParallelToolCalls = &t
		}
	}
	if caps.Reasoning == ReasoningOpenAIEffort {
		body.ReasoningEffort = effortFor(req.Reasoning.Level, caps)
	}
	if profile.decorate != nil {
		profile.decorate(&body, req)
	}
	return marshalWithExtra(body, caps.ExtraBody)
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

// effortFor maps a level onto the provider's own effort value.
func effortFor(l Level, caps Capabilities) string {
	if caps.LevelMap != nil {
		v, ok := caps.LevelMap[l]
		if ok {
			if v == nil {
				return "" // an explicit null omits the parameter
			}
			return *v
		}
	}
	switch l {
	case LevelOff:
		return ""
	case LevelMinimal:
		return "minimal"
	case LevelLow:
		return "low"
	case LevelMedium:
		return "medium"
	default:
		return "high"
	}
}

// compatMessages flattens the content model onto chat-completions messages.
func compatMessages(req Request, caps Capabilities) ([]compatMessage, error) {
	msgs := applyRetention(req.Messages, req.Reasoning.Retain, caps)
	out := make([]compatMessage, 0, len(msgs)+1)

	if len(req.System) > 0 {
		role := roleSystem
		if caps.DeveloperRole {
			role = roleDeveloper
		}
		out = append(out, compatMessage{Role: role, Content: blocksText(req.System)})
	}
	for _, m := range msgs {
		converted, err := compatMessageFor(m, caps)
		if err != nil {
			return nil, err
		}
		out = append(out, converted...)
	}
	return out, nil
}

// compatMessageFor converts one message, which may expand into several when it
// carries tool results.
func compatMessageFor(m Message, caps Capabilities) ([]compatMessage, error) {
	// tool results become their own tool role messages
	var results []compatMessage
	for _, b := range m.Content {
		if tr, ok := b.(ToolResultBlock); ok {
			results = append(results, compatMessage{
				Role: roleTool, ToolCallID: tr.CallID, Content: blocksText(tr.Content),
			})
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
	for _, b := range m.Content {
		switch v := b.(type) {
		case ToolCallBlock:
			msg.ToolCalls = append(msg.ToolCalls, compatToolCall{
				ID: v.ID, Type: typeFunction,
				Function: compatToolFunction{Name: v.Name, Arguments: string(v.Input)},
			})
		case ThinkingBlock:
			if caps.ReplayReasoning && v.Text != "" {
				t := v.Text
				msg.ReasoningContent = &t
			}
			if len(v.Details) > 0 {
				msg.ReasoningDetails = v.Details
			}
		}
	}
	// deepseek requires every replayed assistant message to carry reasoning_content,
	// even when empty, so it stays in thinking mode across turns.
	if caps.ReplayReasoning && m.Role == RoleAssistant && msg.ReasoningContent == nil {
		e := ""
		msg.ReasoningContent = &e
	}
	// skip messages with neither content nor tool calls, which providers reject
	if msg.Content == nil && len(msg.ToolCalls) == 0 {
		return results, nil
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

// blocksText joins the text of every text block.
func blocksText(blocks BlockList) string {
	var b strings.Builder
	for _, blk := range blocks {
		if t, ok := blk.(TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// compatTools converts tool schemas.
func compatTools(tools []ToolSchema) []compatTool {
	out := make([]compatTool, len(tools))
	for i, t := range tools {
		out[i] = compatTool{Type: typeFunction, Function: compatToolSchema(t)}
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
	caps      Capabilities
	split     *thinkSplitter
	tools     *toolAccumulator
	nextBlock int
	thinkIdx  int
	textIdx   int
	thinkBuf  strings.Builder // accumulated for the end block
	textBuf   strings.Builder
	details   json.RawMessage
	usage     Usage
	stop      StopReason
	sawMeta   bool
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

	reasoning := c.Delta.Reasoning
	if reasoning == "" {
		reasoning = c.Delta.ReasoningContent
	}
	content := c.Delta.Content
	if st.caps.Reasoning == ReasoningInlineTags && content != "" {
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

// endThinking closes the thinking block, carrying the replay tokens. It is
// idempotent so both the text transition and the terminal drain may call it.
func (s *compatStream) endThinking() Event {
	st := s.st
	idx := st.thinkIdx
	st.thinkIdx = -2 // closed, so a later drain does not repeat it
	return Event{Type: EventThinkingEnd, Index: idx,
		Block: ThinkingBlock{Text: st.thinkBuf.String(), Details: st.details}}
}

// finish drains the accumulators and emits the terminal events.
func (s *compatStream) finish(cause error) []Event {
	if s.done {
		return nil
	}
	s.done = true

	var events []Event
	st := s.st
	if st.caps.Reasoning == ReasoningInlineTags {
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
		stop = StopEndTurn
	}
	return append(events, Event{Type: EventDone, StopReason: stop, Usage: st.usage, Err: streamErr})
}
