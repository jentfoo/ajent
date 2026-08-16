package srv

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// writer emits chat-completions SSE frames and counts the completion tokens it
// has streamed, so usage tracks against real emitted bytes.
type writer struct {
	w      http.ResponseWriter
	head   string // chunk id prefix ("chatcmpl-demo-<model>")
	frame  int
	tokens int // completion_tokens: len(data)/4 accumulated across content+args
}

func newWriter(w http.ResponseWriter, model string) *writer {
	if model == "" {
		model = "unknown"
	}
	return &writer{w: w, head: "chatcmpl-demo-" + model}
}

// flush pushes one chunk as a data frame and accounts its bytes toward usage.
func (s *writer) flush(c chunk) {
	s.frame++
	c.ID = fmt.Sprintf("%s-%d", s.head, s.frame)
	data := marshalJSON(c)
	// completion tokens approximate the emitted payload; reasoning counts too
	for _, ch := range c.Choices {
		if ch.Delta.Reasoning != "" {
			s.tokens += len(ch.Delta.Reasoning) / 4
		}
		if ch.Delta.Content != "" {
			s.tokens += len(ch.Delta.Content) / 4
		}
		for _, tc := range ch.Delta.ToolCalls {
			s.tokens += len(tc.Function.Arguments) / 4
		}
	}
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", data)
}

// done terminates the stream with the OpenAI [DONE] sentinel.
func (s *writer) done() { _, _ = fmt.Fprint(s.w, "data: [DONE]\n\n") }

// reasoningDelta streams a fragment of thinking prose.
func (s *writer) reasoningDelta(model, text string) {
	s.flush(chunk{Model: model, Choices: []choiceDelta{{Index: 0,
		Delta: delta{Reasoning: text}}}})
}

// textDelta streams a fragment of assistant content.
func (s *writer) textDelta(model, text string) {
	s.flush(chunk{Model: model, Choices: []choiceDelta{{Index: 0,
		Delta: delta{Content: text}}}})
}

// toolCallOpen announces one call with its name; arguments follow as fragments.
func (s *writer) toolCallOpen(model string, index int, id, name string) {
	s.flush(chunk{Model: model, Choices: []choiceDelta{{Index: 0,
		Delta: delta{ToolCalls: []callDelta{
			{Index: index, ID: id, Type: "function", Function: functionArg{Name: name}},
		}}}}})
}

// toolCallArgs streams one fragment of a call's JSON arguments.
func (s *writer) toolCallArgs(model string, index int, frag string) {
	s.flush(chunk{Model: model, Choices: []choiceDelta{{Index: 0,
		Delta: delta{ToolCalls: []callDelta{
			{Index: index, Function: functionArg{Arguments: frag}},
		}}}}})
}

// finish emits the terminal choice carrying the stop reason and usage.
func (s *writer) finish(model, reason string, prompt int) {
	s.flush(chunk{Model: model, Choices: []choiceDelta{{Index: 0, FinishReason: reason}},
		Usage: &usageReport{PromptTokens: prompt, CompletionTokens: s.tokens}})
}

// marshalJSON encodes a chunk; encoding/json never fails on these types.
func marshalJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
