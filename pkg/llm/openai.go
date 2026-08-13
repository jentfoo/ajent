package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
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
		headers: withSessionHeaders(req.Model.Headers, req), classify: compatClassifier(p.name, FlavorOpenAI),
	})
	if err != nil {
		return nil, err
	}
	return newResponsesStream(ctx, resp, p.name), nil
}

// buildResponsesBody marshals a request into a Responses API body. It is pure,
// so request shape can be asserted without a server.
func buildResponsesBody(req Request) ([]byte, error) {
	req = Prepare(req)
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
		// openai rejects a max_output_tokens below its floor
		body.MaxOutputTokens = ptrOf(max(req.MaxTokens, respMinOutputTokens))
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
	if caps.Dialect == DialectOpenAIResponses && caps.Reasoning {
		// the responses dialect stays ungated by SupportsReasoningEffort, as in pi;
		// it clamps internally like every encoder so a bare build is self-contained
		level := clampLevel(caps, req.Reasoning.Level)
		if on := level != LevelOff; on {
			if effort, ok := levelValue(caps, level); ok && effort != "" {
				body.Reasoning = &respReasoning{Effort: effort, Summary: "auto"}
				// pi requests the encrypted payload whenever a non-off reasoning
				// param is sent, independent of server-side store state
				body.Include = []string{respEncryptedInclude}
			}
		} else if e, ok := offValue(caps, "none"); ok {
			// explicit off still names an effort so the model stops thinking;
			// {off:null} suppresses the key entirely
			body.Reasoning = &respReasoning{Effort: e}
		}
	}
	if !caps.Store {
		// without server side state the encrypted payload is the only way to
		// replay reasoning on the next turn
		body.Store = ptrOf(false)
	}
	return json.Marshal(body)
}

// responsesInput converts the content model to the typed input item list.
func responsesInput(req Request, caps Capabilities) ([]respItem, error) {
	msgs := req.Messages // already normalized by Prepare
	out := make([]respItem, 0, len(msgs))

	// msgIndex counts every converted message so fallback ids stay unique across
	// turns; it matches pi's per-message counter.
	msgIndex := 0
	for _, m := range msgs {
		// mid-conversation system messages become input items rather than being
		// dropped; the top-level prompt is handled as instructions
		items, err := responsesItems(m, caps, msgIndex)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		msgIndex++
	}
	return out, nil
}

// responsesItems converts one message, which expands into several items when it
// carries reasoning, tool calls or tool results alongside text. Items follow
// block order; a run of text is flushed as one message item whenever a non-text
// block interrupts it and at the end of the message.
func responsesItems(m Message, caps Capabilities, msgIndex int) ([]respItem, error) {
	var out []respItem
	var content []respContent
	id, phase := "", ""
	textN := 0 // text-block counter for fallback ids within this message

	flushMessage := func() {
		if len(content) == 0 {
			return
		}
		role := string(m.Role)
		switch m.Role {
		case RoleTool:
			role = roleUser
		case RoleSystem:
			// a mid-conversation system message uses the developer role when the
			// model accepts it, else stays as system
			if caps.DeveloperRole {
				role = roleDeveloper
			}
		}
		out = append(out, respItem{Type: respTypeMessage, Role: role,
			Content: content, Status: "completed", ID: id, Phase: phase})
		content = nil
		id, phase = "", ""
	}

	for _, b := range m.Content {
		switch v := b.(type) {
		case TextBlock:
			if v.Text == "" {
				continue
			}
			iid, ph := parseTextSignature(v.Signature)
			// a signed block whose id differs from the open run starts its own
			// message item, so commentary and final answer keep distinct ids across turns
			if len(content) > 0 && iid != "" {
				if nid := shortenTextID(iid); nid != id {
					flushMessage()
				}
			}
			kind := respInputText
			if m.Role == RoleAssistant {
				kind = respOutputText
			}
			content = append(content, respContent{Type: kind, Text: v.Text})
			switch {
			case iid != "": // the original item id keeps the replay stable
				id, phase = shortenTextID(iid), ph
			case len(content) > 1:
				// an unsigned continuation merges into the open run, keeping its identity
			default: // first block of a message gets a fresh fallback id
				if textN > 0 {
					id = fmt.Sprintf("msg_pi_%d_%d", msgIndex, textN)
				} else {
					id = fmt.Sprintf("msg_pi_%d", msgIndex)
				}
				phase = ""
			}
			textN++
		case ImageBlock:
			if !caps.Images {
				return nil, errors.New("llm: model does not accept image input")
			}
			content = append(content, respContent{Type: respInputImage,
				ImageURL: "data:" + v.MediaType + ";base64," + base64.StdEncoding.EncodeToString(v.Data)})
		case ThinkingBlock:
			flushMessage()
			if v.ItemID == "" && len(v.Item) == 0 {
				continue // nothing to reference, so it cannot be replayed
			}
			var item respItem
			if len(v.Item) > 0 { // replay the original serialized item verbatim
				item = respItem{Type: respTypeReasoning, Raw: slices.Clone(v.Item)}
			} else { // transcripts written before this change fall back to a stub
				item = respItem{Type: respTypeReasoning, ID: v.ItemID,
					EncryptedContent: v.Encrypted, Summary: []any{}}
			}
			out = append(out, item)
		case ToolCallBlock:
			flushMessage()
			callID, itemID := cutToolCallID(v.ID)
			if !strings.HasPrefix(itemID, "fc_") {
				itemID = "" // the API rejects a function_call id in any other form
			}
			out = append(out, respItem{Type: respTypeFunction, CallID: callID,
				Name: v.Name, Arguments: string(v.Input), ID: itemID})
		case ToolResultBlock:
			flushMessage()
			callID, _, _ := strings.Cut(v.CallID, "|")
			out = append(out, respItem{Type: respTypeFuncOutput, CallID: callID,
				Output: toolResultOutput(caps, v.Content)})
		}
	}
	flushMessage()
	return out, nil
}

// toolResultOutput renders a tool result's output for function_call_output: a
// plain text string, or an array of input_text/input_image parts when the result
// kept images and the model accepts them (pi's shape).
func toolResultOutput(caps Capabilities, blocks BlockList) any {
	text := blocksText(blocks)
	if caps.Dialect != DialectOpenAIResponses || !caps.Images || !hasImage(blocks) {
		return text
	}
	parts := make([]respContent, 0, len(blocks))
	if text != "" {
		parts = append(parts, respContent{Type: respInputText, Text: text})
	}
	for _, b := range blocks {
		img, ok := b.(ImageBlock)
		if !ok {
			continue
		}
		url := "data:" + img.MediaType + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
		parts = append(parts, respContent{Type: respInputImage, ImageURL: url})
	}
	return parts
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

// textSignatureV1 is the versioned form a text block carries its message id and
// phase in, so replays stay stable across turns.
type textSignatureV1 struct {
	V     int    `json:"v"`
	ID    string `json:"id"`
	Phase string `json:"phase,omitempty"`
}

// encodeTextSignature renders a responses message id and phase into the signature
// a text block carries.
func encodeTextSignature(id, phase string) string {
	b, _ := json.Marshal(textSignatureV1{V: 1, ID: id, Phase: phase})
	return string(b)
}

// parseTextSignature returns the message id and phase a signature carries,
// accepting both the versioned object and a bare legacy id.
func parseTextSignature(sig string) (id, phase string) {
	sig = strings.TrimSpace(sig)
	if !strings.HasPrefix(sig, "{") || len(sig) == 0 {
		return sig, "" // a bare or empty signature is the id itself
	}
	var v textSignatureV1
	if err := json.Unmarshal([]byte(sig), &v); err != nil {
		return strings.TrimSpace(sig), ""
	}
	id = v.ID
	switch v.Phase { // only the phases pi round-trips are accepted back
	case "commentary", "final_answer":
		phase = v.Phase
	}
	return id, phase
}

// shortenTextID hashes an overlong message id so replays stay bounded; ids at or
// under the limit pass through unchanged.
func shortenTextID(id string) string {
	if len(id) > maxReplayTextID {
		return "msg_" + shortHash(id)
	}
	return id
}

// cutToolCallID splits the piped tool-call id into its call and item parts.
func cutToolCallID(id string) (callID, itemID string) {
	callID, itemID, _ = strings.Cut(id, "|")
	return callID, itemID
}

// responsesStream decodes a Responses API event stream.
type responsesStream struct {
	ctx      context.Context
	resp     *http.Response
	sse      *SSEReader
	provider string
	*streamPump

	items map[int]*respOpenItem
	// emittedReasoning remembers each thinking block's output index so a terminal
	// response that supplies an encrypted payload the done event omitted can
	// re-emit it (azure backfill). keyed by reasoning item id.
	emittedReasoning map[string]respEmittedThink
	usage            Usage
	status           string
	sawTool          bool
}

// respEmittedThink is a thinking block already emitted for an output index,
// kept so the azure backfill can replace it with one carrying encrypted content.
type respEmittedThink struct {
	idx   int
	block ThinkingBlock
}

// respOpenItem is an output item being streamed.
type respOpenItem struct {
	kind   string
	id     string
	callID string
	name   string
	text   strings.Builder
	raw    json.RawMessage // completed item, for verbatim replay
}

func newResponsesStream(ctx context.Context, resp *http.Response, provider string) *responsesStream {
	s := &responsesStream{
		ctx: ctx, resp: resp, provider: provider,
		sse:              NewSSEReader(resp.Body, 0),
		items:            make(map[int]*respOpenItem),
		emittedReasoning: make(map[string]respEmittedThink),
	}
	s.streamPump = newStreamPump(resp.Body, s.readFrame)
	return s
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
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		// reasoning_text is the inline content form; summary_text streams the
		// condensed version. both feed the thinking block, as in pi.
		return s.onDelta(ev, EventThinkingDelta)
	case "response.reasoning_summary_part.done":
		return s.onSummaryDone(ev)
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

// onSummaryDone appends the separator pi inserts between reasoning summary parts.
func (s *responsesStream) onSummaryDone(ev respEvent) []Event {
	it := s.items[ev.OutputIndex]
	if it == nil {
		return nil
	}
	const sep = "\n\n"
	it.text.WriteString(sep)
	return []Event{{Type: EventThinkingDelta, Index: ev.OutputIndex, Text: sep}}
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
		if len(ev.Item.Raw) > 0 {
			it.raw = slices.Clone(ev.Item.Raw)
		}
	}

	switch it.kind {
	case respTypeReasoning:
		block := ThinkingBlock{Text: it.text.String(), ItemID: it.id, Item: it.raw}
		if ev.Item != nil && ev.Item.EncryptedContent != "" {
			block.Encrypted = ev.Item.EncryptedContent
		}
		// remember the emission so a terminal response that supplies an encrypted
		// payload this done event omitted can re-emit it before EventDone
		s.emittedReasoning[block.ItemID] = respEmittedThink{idx: ev.OutputIndex, block: block}
		return []Event{{Type: EventThinkingEnd, Index: ev.OutputIndex, Block: block}}
	case respTypeMessage:
		sig := ""
		if ev.Item != nil {
			sig = encodeTextSignature(ev.Item.ID, ev.Item.Phase)
		}
		return []Event{{Type: EventTextEnd, Index: ev.OutputIndex,
			Block: TextBlock{Text: it.text.String(), Signature: sig}}}
	case respTypeFunction:
		args := it.text.String()
		if ev.Item != nil && ev.Item.Arguments != "" {
			args = ev.Item.Arguments
		}
		input, err := finishToolInput(args)
		// the item id pairs with the tool result for same-model reasoning reuse;
		// it is appended only when present so bare ids stay unchanged
		toolID := it.callID
		if ev.Item != nil && strings.HasPrefix(ev.Item.ID, "fc_") {
			toolID = it.callID + "|" + ev.Item.ID
		}
		return []Event{{Type: EventToolCallEnd, Index: ev.OutputIndex,
			ToolCallID: it.callID, ToolName: it.name,
			Block: ToolCallBlock{ID: toolID, Name: it.name, Input: input}, Err: err}}
	default:
		return nil
	}
}

func (s *responsesStream) onCompleted(ev respEvent) []Event {
	var out []Event
	if ev.Response != nil {
		s.status = ev.Response.Status
		// azure can omit reasoning.encrypted_content from output_item.done and give
		// it only in response.completed.output; re-emit any thinking block that was
		// missing one before EventDone so stateless replay keeps working
		for _, item := range ev.Response.Output {
			if item.Type != respTypeReasoning || item.EncryptedContent == "" {
				continue
			}
			got, ok := s.emittedReasoning[item.ID]
			if !ok || got.block.Encrypted != "" {
				continue // already captured or unknown id
			}
			blk := ThinkingBlock{Text: got.block.Text, ItemID: item.ID,
				Encrypted: item.EncryptedContent, Item: slices.Clone(item.Raw)}
			out = append(out, Event{Type: EventThinkingEnd, Index: got.idx, Block: blk})
		}
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
