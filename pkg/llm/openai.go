package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
)

// responsesProvider serves the OpenAI Responses API, falling back to the shared
// chat-completions dialect for a model whose compat block asks for it.
type responsesProvider struct {
	client   *httpClient
	name     string
	fallback *compatProvider
}

// newResponsesProvider returns a Responses API provider with a chat-completions
// fallback for models that lack it.
func newResponsesProvider(name string, client *httpClient) *responsesProvider {
	return &responsesProvider{
		client: client,
		name:   name,
		fallback: &compatProvider{client: client, profile: compatProfile{
			name: name, classify: compatClassifier(name, FlavorOpenAI),
		}},
	}
}

// Name returns the provider name.
func (p *responsesProvider) Name() string { return p.name }

// Stream sends a request and returns its normalized event stream. The dialect
// is chosen per model from the resolved capabilities, not sniffed at runtime.
func (p *responsesProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	if req.Model.Caps.MaxTokensField == fieldMaxCompletion {
		return p.fallback.Stream(ctx, req) // this model speaks chat-completions
	}
	body, err := buildResponsesBody(req)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.do(ctx, httpReq{
		method: http.MethodPost, path: "/responses", body: body,
		classify: compatClassifier(p.name, FlavorOpenAI),
	})
	if err != nil {
		return nil, err
	}
	return newResponsesStream(ctx, resp, p.name), nil
}

// buildResponsesBody marshals a request into a Responses API body. It is pure,
// so request shape can be asserted without a server.
func buildResponsesBody(req Request) ([]byte, error) {
	caps := req.Model.Caps
	items, err := responsesInput(req, caps)
	if err != nil {
		return nil, err
	}

	body := respRequest{
		Model:        req.Model.ID,
		Input:        items,
		Instructions: blocksText(req.System),
		Stream:       true,
	}
	if req.MaxTokens > 0 {
		body.MaxOutputTokens = &req.MaxTokens
	}
	if req.Temperature != nil && caps.Temperature {
		body.Temperature = req.Temperature
	}
	if len(req.Tools) > 0 {
		body.Tools = responsesTools(req.Tools)
		if caps.ToolChoice {
			body.ToolChoice = responsesToolChoice(req.ToolChoice)
		}
		if caps.ParallelTools {
			body.ParallelTools = ptrOf(true)
		}
	}
	if caps.Reasoning == ReasoningOpenAIEffort && req.Reasoning.Level != LevelOff {
		if effort := effortFor(req.Reasoning.Level, caps); effort != "" {
			body.Reasoning = &respReasoning{Effort: effort, Summary: "auto"}
		}
	}
	if !caps.Store {
		// without server side state the encrypted payload is the only way to
		// replay reasoning on the next turn
		body.Store = ptrOf(false)
		if body.Reasoning != nil {
			body.Include = []string{respEncryptedInclude}
		}
	}
	return json.Marshal(body)
}

// responsesInput converts the content model to the typed input item list.
func responsesInput(req Request, caps Capabilities) ([]respItem, error) {
	msgs := applyRetention(req.Messages, req.Reasoning.Retain, caps)
	out := make([]respItem, 0, len(msgs))

	for _, m := range msgs {
		if m.Role == RoleSystem {
			continue // system goes to instructions
		}
		items, err := responsesItems(m, caps)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

// responsesItems converts one message, which expands into several items when it
// carries reasoning, tool calls or tool results alongside text.
func responsesItems(m Message, caps Capabilities) ([]respItem, error) {
	var out []respItem
	var content []respContent

	for _, b := range m.Content {
		switch v := b.(type) {
		case TextBlock:
			if v.Text == "" {
				continue
			}
			kind := respInputText
			if m.Role == RoleAssistant {
				kind = respOutputText
			}
			content = append(content, respContent{Type: kind, Text: v.Text})
		case ImageBlock:
			if !caps.Images {
				return nil, errors.New("llm: model does not accept image input")
			}
			content = append(content, respContent{Type: respInputImage,
				ImageURL: "data:" + v.MediaType + ";base64," + base64.StdEncoding.EncodeToString(v.Data)})
		case ThinkingBlock:
			if v.ItemID == "" {
				continue // nothing to reference, so it cannot be replayed
			}
			out = append(out, respItem{Type: respTypeReasoning, ID: v.ItemID,
				EncryptedContent: v.Encrypted, Summary: []any{}})
		case ToolCallBlock:
			out = append(out, respItem{Type: respTypeFunction, CallID: v.ID,
				Name: v.Name, Arguments: string(v.Input)})
		case ToolResultBlock:
			out = append(out, respItem{Type: respTypeFuncOutput, CallID: v.CallID,
				Output: blocksText(v.Content)})
		}
	}
	if len(content) > 0 {
		role := string(m.Role)
		if m.Role == RoleTool {
			role = roleUser
		}
		out = append(out, respItem{Type: respTypeMessage, Role: role, Content: content})
	}
	return out, nil
}

// responsesTools converts tool schemas to the flat Responses form.
func responsesTools(tools []ToolSchema) []respTool {
	out := make([]respTool, len(tools))
	for i, t := range tools {
		out[i] = respTool{Type: typeFunction, Name: t.Name,
			Description: t.Description, Parameters: t.Parameters}
	}
	return out
}

// responsesToolChoice renders the tool choice, nil when the default applies.
func responsesToolChoice(tc ToolChoice) any {
	switch tc.Mode {
	case ToolChoiceNone:
		return "none"
	case ToolChoiceRequired:
		return "required"
	case ToolChoiceSpecific:
		return map[string]string{"type": typeFunction, "name": tc.Name}
	default:
		return nil
	}
}

// responsesStream decodes a Responses API event stream.
type responsesStream struct {
	ctx      context.Context
	resp     *http.Response
	sse      *SSEReader
	provider string

	items   map[int]*respOpenItem
	pending []Event
	usage   Usage
	status  string
	sawTool bool
	done    bool
	err     error

	mu     sync.Mutex
	closed bool
}

// respOpenItem is an output item being streamed.
type respOpenItem struct {
	kind   string
	id     string
	callID string
	name   string
	text   strings.Builder
}

func newResponsesStream(ctx context.Context, resp *http.Response, provider string) *responsesStream {
	return &responsesStream{
		ctx: ctx, resp: resp, provider: provider,
		sse:   NewSSEReader(resp.Body, 0),
		items: make(map[int]*respOpenItem),
	}
}

// Next returns the next event, or false at end of stream.
func (s *responsesStream) Next() (Event, bool) {
	for {
		if s.isClosed() {
			return Event{}, false // a close abandons anything still buffered
		} else if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, true
		} else if s.done {
			return Event{}, false
		}
		s.pending = append(s.pending, s.readFrame()...)
	}
}

// readFrame decodes one SSE frame into zero or more events.
func (s *responsesStream) readFrame() []Event {
	frame, err := s.sse.Next(s.ctx)
	if err != nil {
		return s.finish(err)
	}

	var ev respEvent
	if err = json.Unmarshal(frame.Data, &ev); err != nil {
		return s.finish(err)
	}

	switch ev.Type {
	case "response.created":
		meta := &StreamMeta{}
		if ev.Response != nil {
			meta.Model, meta.RequestID = ev.Response.Model, ev.Response.ID
		}
		return []Event{{Type: EventMessageStart, Meta: meta}}
	case "response.output_item.added":
		return s.onItemAdded(ev)
	case "response.output_text.delta":
		return s.onDelta(ev, EventTextDelta)
	case "response.reasoning_summary_text.delta":
		return s.onDelta(ev, EventThinkingDelta)
	case "response.function_call_arguments.delta":
		return s.onDelta(ev, EventToolCallDelta)
	case "response.output_item.done":
		return s.onItemDone(ev)
	case "response.completed", "response.incomplete":
		return s.onCompleted(ev)
	case "response.failed", "error":
		return s.finish(s.frameError(ev))
	default:
		return nil // lifecycle frames we do not need
	}
}

// frameError builds the error a failure frame carries.
func (s *responsesStream) frameError(ev respEvent) error {
	msg, code := ev.Message, ev.Code
	if ev.Response != nil && ev.Response.Error != nil {
		msg, code = ev.Response.Error.Message, ev.Response.Error.Code
	}
	if msg == "" {
		msg = "stream error"
	}
	return &APIError{Provider: s.provider, Code: code, Message: msg}
}

func (s *responsesStream) onItemAdded(ev respEvent) []Event {
	if ev.Item == nil {
		return nil
	}
	it := &respOpenItem{kind: ev.Item.Type, id: ev.Item.ID,
		callID: ev.Item.CallID, name: ev.Item.Name}
	s.items[ev.OutputIndex] = it

	switch it.kind {
	case respTypeReasoning:
		return []Event{{Type: EventThinkingStart, Index: ev.OutputIndex}}
	case respTypeMessage:
		return []Event{{Type: EventTextStart, Index: ev.OutputIndex}}
	case respTypeFunction:
		s.sawTool = true
		return []Event{{Type: EventToolCallStart, Index: ev.OutputIndex,
			ToolCallID: it.callID, ToolName: it.name}}
	default:
		return nil
	}
}

func (s *responsesStream) onDelta(ev respEvent, typ EventType) []Event {
	it := s.items[ev.OutputIndex]
	if it == nil || ev.Delta == "" {
		return nil
	}
	it.text.WriteString(ev.Delta)

	out := Event{Type: typ, Index: ev.OutputIndex, Text: ev.Delta}
	if typ == EventToolCallDelta {
		out.ToolCallID = it.callID
	}
	return []Event{out}
}

func (s *responsesStream) onItemDone(ev respEvent) []Event {
	it := s.items[ev.OutputIndex]
	if it == nil {
		return nil
	}
	delete(s.items, ev.OutputIndex)
	if ev.Item != nil { // the completed item carries the replay tokens
		if ev.Item.ID != "" {
			it.id = ev.Item.ID
		}
		if ev.Item.CallID != "" {
			it.callID = ev.Item.CallID
		}
		if ev.Item.Name != "" {
			it.name = ev.Item.Name
		}
	}

	switch it.kind {
	case respTypeReasoning:
		block := ThinkingBlock{Text: it.text.String(), ItemID: it.id}
		if ev.Item != nil {
			block.Encrypted = ev.Item.EncryptedContent
		}
		return []Event{{Type: EventThinkingEnd, Index: ev.OutputIndex, Block: block}}
	case respTypeMessage:
		return []Event{{Type: EventTextEnd, Index: ev.OutputIndex,
			Block: TextBlock{Text: it.text.String()}}}
	case respTypeFunction:
		args := it.text.String()
		if ev.Item != nil && ev.Item.Arguments != "" {
			args = ev.Item.Arguments
		}
		input, err := finishToolInput(args)
		return []Event{{Type: EventToolCallEnd, Index: ev.OutputIndex,
			ToolCallID: it.callID, ToolName: it.name,
			Block: ToolCallBlock{ID: it.callID, Name: it.name, Input: input}, Err: err}}
	default:
		return nil
	}
}

func (s *responsesStream) onCompleted(ev respEvent) []Event {
	var out []Event
	if ev.Response != nil {
		s.status = ev.Response.Status
		if ev.Response.Usage != nil {
			s.usage = ev.Response.Usage.toUsage()
			out = append(out, Event{Type: EventUsage, Usage: s.usage})
		}
	}
	return append(out, s.finish(io.EOF)...)
}

// finish emits the terminal event.
func (s *responsesStream) finish(cause error) []Event {
	if s.done {
		return nil
	}
	s.done = true

	stop := respStopReason(s.status, s.sawTool)
	var streamErr error
	if cause != nil && !errors.Is(cause, io.EOF) {
		streamErr = cause
		stop = StopError
		s.err = cause
	}
	return []Event{{Type: EventDone, StopReason: stop, Usage: s.usage, Err: streamErr}}
}

// Err returns the failure that ended the stream, if any.
func (s *responsesStream) Err() error {
	if s.isClosed() {
		return nil
	}
	return s.err
}

// Close stops the stream and unblocks a pending Next.
func (s *responsesStream) Close() error {
	s.mu.Lock()
	already := s.closed
	s.closed = true
	s.mu.Unlock()
	if already {
		return nil
	}
	return s.resp.Body.Close()
}

func (s *responsesStream) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}
