package srv

import (
	"bufio"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// collectFrames runs fn against a writer and returns every data line parsed.
func collectFrames(t *testing.T, fn func(w *writer)) []chunk {
	t.Helper()
	w := httptest.NewRecorder()
	s := newWriter(w, "m")
	fn(s)
	body := w.Body.String()

	var out []chunk
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.TrimSpace(line) == "data:" {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			out = append(out, chunk{}) // sentinel marker; see doneFrames for detection
			continue
		}
		var c chunk
		require.NoError(t, json.Unmarshal([]byte(payload), &c))
		out = append(out, c)
	}
	return out
}

func TestFrameWritersProduceWellFormedFrames(t *testing.T) {
	t.Parallel()

	frames := collectFrames(t, func(s *writer) {
		s.reasoningDelta("m", "think")
		s.textDelta("m", "hello ")
		s.toolCallOpen("m", 0, "call_1", "write")
		s.toolCallArgs("m", 0, `{"path":"`)
		s.finish("m", "tool_calls", 10)
	})
	require.Len(t, frames, 5)

	assert.Equal(t, "think", frames[0].Choices[0].Delta.Reasoning)
	assert.Equal(t, "hello ", frames[1].Choices[0].Delta.Content)

	tc := frames[2].Choices[0].Delta.ToolCalls
	require.Len(t, tc, 1)
	assert.Equal(t, "call_1", tc[0].ID)
	assert.Equal(t, "write", tc[0].Function.Name)

	final := frames[len(frames)-1]
	assert.Equal(t, "tool_calls", final.Choices[0].FinishReason)
	require.NotNil(t, final.Usage)
	assert.Equal(t, 10, final.Usage.PromptTokens)
}

func TestToolCallArgsConcatenateToValidJSON(t *testing.T) {
	t.Parallel()

	frames := collectFrames(t, func(s *writer) {
		s.toolCallOpen("m", 0, "call_1", "write")
		for _, frag := range []string{`{"path":"`, `/tmp/x.go","content":`, `"hi"}`} {
			s.toolCallArgs("m", 0, frag)
		}
	})
	var b strings.Builder
	b.WriteString(frames[2].Choices[0].Delta.ToolCalls[0].Function.Name) // noop sanity
	for _, f := range frames[1:] {                                       // skip the open frame's empty args
		if len(f.Choices) == 0 {
			continue
		}
		for _, tc := range f.Choices[0].Delta.ToolCalls {
			b.WriteString(tc.Function.Arguments)
		}
	}
	var got map[string]string
	require.NoError(t, json.Unmarshal([]byte(b.String()), &got))
	assert.Equal(t, "/tmp/x.go", got["path"])
	assert.Equal(t, "hi", got["content"])
}

func TestStreamTerminatesWithDone(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	s := newWriter(w, "m")
	s.textDelta("m", "x")
	s.finish("m", "stop", 4)
	s.done()

	scanner := bufio.NewScanner(strings.NewReader(w.Body.String()))
	var last string
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); strings.HasPrefix(line, "data:") {
			last = strings.TrimPrefix(line, "data:")
		}
	}
	assert.Equal(t, "[DONE]", strings.TrimSpace(last))
}

func TestCompletionTokenAccounting(t *testing.T) {
	t.Parallel()

	s := newWriter(httptest.NewRecorder(), "m")
	// each flush counts len(content)/4 toward completion tokens
	s.textDelta("m", "0123456789012345678901234567890123456789")
	assert.Equal(t, 10, s.tokens)
}
