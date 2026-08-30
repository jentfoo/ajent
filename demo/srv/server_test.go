package srv

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testServer mounts the demo routes on an httptest server.
func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := &Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/chat/completions", s.handleChat)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// complete posts a chat-completion request and returns the parsed SSE stream.
func (h *harness) complete(t *testing.T, msgs []wireMessage, tools []toolSpec) sseStream {
	t.Helper()
	body := fmt.Sprintf(`{"model":"anything","messages":%s,"stream":true}`, wireMessages(msgs))
	if len(tools) > 0 {
		body = body[:len(body)-1] + `,"tools":` + marshalJSONBytes(tools) + "}"
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		h.ts.URL+"/v1/chat/completions", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return parseSSE(string(b))
}

// TestScriptServed drives the whole script through HTTP and checks each step's
// emitted tools in order: only the final turn ends with finish_reason "stop".
func TestScriptServed(t *testing.T) {
	t.Parallel()
	ts := testServer(t)
	h := &harness{ts: ts}

	stps := script()
	msgs := make([]wireMessage, 0, len(stps)) // transcript fed back on each request
	for i, stp := range stps {
		s := h.complete(t, msgs, allNativeTools())
		if i < len(stps)-1 {
			assert.Equal(t, "tool_calls", s.finish)
			names := make([]string, 0, len(s.calls))
			for _, c := range s.calls {
				names = append(names, c.Function.Name)
			}
			want := make([]string, 0, len(stp.calls))
			for _, call := range stp.calls {
				want = append(want, call.name)
			}
			assert.Equal(t, want, names, "step %d tool order", i)
		} else {
			assert.Equal(t, "stop", s.finish)
		}
		// feed this turn back so the next request advances the step index
		msgs = append(msgs, wireMessage{Role: "assistant", ToolCalls: s.calls})
	}

	// every bash call in a non-final step carried a recoverable scratch path
	found := false
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			if strings.Contains(string(c.Function.Arguments), scratchPrefix) {
				found = true
			}
		}
	}
	assert.True(t, found)
}

// TestScriptElapsed verifies the final summary carries a parseable total.
func TestScriptElapsed(t *testing.T) {
	t.Parallel()
	ts := testServer(t)
	h := &harness{ts: ts}

	stps := script()
	msgs := make([]wireMessage, 0, len(stps))
	for range stps[:len(stps)-1] {
		s := h.complete(t, msgs, allNativeTools())
		msgs = append(msgs, wireMessage{Role: "assistant", ToolCalls: s.calls})
	}
	final := h.complete(t, msgs, allNativeTools())
	assert.Equal(t, "stop", final.finish)
	total := totalFrom(final)
	require.NotEmpty(t, total)
	// N.NNNs with a dot and three fractional digits
	sec, err := strconv.ParseFloat(total[:len(total)-1], 64)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, sec, 0.0)
}

// TestClassifierNoTools pins that a tool-less request is answered with the
// prompt's own approving word, so the agent's classifier approves whatever it
// asks about.
func TestClassifierNoTools(t *testing.T) {
	t.Parallel()
	ts := testServer(t)
	h := &harness{ts: ts}

	for _, cmd := range []string{"mkdir -p /tmp/x", "rm -rf /tmp/y", "ls -la /tmp"} {
		s := h.complete(t, []wireMessage{{Role: "user", Content: cmd}}, nil)
		assert.Equal(t, "stop", s.finish)
		assert.Contains(t, concatContent(s.deltas), "allow")
	}
}

// TestClassifierAnswersPromptVocabulary pins that the approving word is read
// from the prompt rather than assumed, so any harness's wording is approved.
func TestClassifierAnswersPromptVocabulary(t *testing.T) {
	t.Parallel()
	ts := testServer(t)
	h := &harness{ts: ts}

	cases := []struct {
		name   string
		system string
		want   string
	}{
		{"allow_deny_prompt", "Categories:\n" + `- "allow" — only reads` + "\n" + `- "deny" — has a side effect`, "allow"},
		{"readonly_write_prompt", "Categories:\n" + `- "readonly" — only reads` + "\n" + `- "write" — has a side effect`, "readonly"},
		{"no_categories", "answer in one word", "allow"},
		{"no_system_message", "", "allow"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msgs := []wireMessage{{Role: "user", Content: "mkdir -p build"}}
			if c.system != "" {
				msgs = append([]wireMessage{{Role: "system", Content: c.system}}, msgs...)
			}
			s := h.complete(t, msgs, nil)
			assert.Equal(t, "stop", s.finish)
			assert.Equal(t, c.want, concatContent(s.deltas))
		})
	}
}

// TestModelsAdvertised checks the models route lists ajent-demo-1.
func TestModelsAdvertised(t *testing.T) {
	t.Parallel()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		testServer(t).URL+"/v1/models", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), modelID)
}

// sseStream is one parsed completion: accumulated deltas, tool calls and reason.
type sseStream struct {
	finish string
	deltas []delta
	calls  []assistantCall
}

// parseSSE reads chat-completions SSE frames into a stream. Tool-call argument
// fragments are concatenated per index, so the result is valid JSON.
func parseSSE(out string) sseStream {
	var st sseStream
	args := map[int]string{}
	nativeName := map[int]string{}
	for _, frame := range strings.Split(out, "\n\n") {
		if !strings.HasPrefix(frame, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(frame, "data: ")
		var c chunk
		_ = jsonUnmarshal([]byte(payload), &c)
		for _, ch := range c.Choices {
			if ch.FinishReason != "" {
				st.finish = ch.FinishReason
			}
			st.deltas = append(st.deltas, ch.Delta)
			for _, tc := range ch.Delta.ToolCalls {
				if tc.Function.Name != "" {
					nativeName[tc.Index] = tc.Function.Name
				}
				args[tc.Index] += tc.Function.Arguments
			}
		}
	}
	for i := 0; i < len(args); i++ {
		st.calls = append(st.calls, assistantCall{ID: fmt.Sprintf("call_%d", i),
			Function: callInvocation{Name: nativeName[i], Arguments: json.RawMessage(args[i])}})
	}
	return st
}

// harness binds a test server for the script walk.
type harness struct{ ts *httptest.Server }

// wireMessage mirrors chatMessage for transcript construction in tests.
type wireMessage struct {
	Role      string
	Content   any
	ToolCalls []assistantCall
}

func allNativeTools() []toolSpec {
	var tools []toolSpec
	for _, stp := range script() {
		for _, call := range stp.calls {
			tools = append(tools, toolSpec{Type: "function",
				Function: toolFunction{Name: call.name}})
		}
	}
	return tools
}

// concatContent joins a stream's content deltas into one string.
func concatContent(deltas []delta) string {
	var b strings.Builder
	b.Grow(len(deltas))
	for _, d := range deltas {
		b.WriteString(d.Content)
	}
	return b.String()
}

// totalFrom extracts the N.NNNs run-time line from a final summary stream.
func totalFrom(s sseStream) string {
	content := concatContent(s.deltas)
	i := strings.Index(content, "total: ")
	if i == -1 {
		return ""
	}
	rest := content[i+len("total: "):]
	j := strings.IndexByte(rest, '\n')
	if j > 0 {
		rest = rest[:j]
	}
	// summaryMarkdown bolds the total as **N.NNNs**, so drop the markers
	return strings.ReplaceAll(rest, "*", "")
}

func wireMessages(msgs []wireMessage) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		c := marshalJSONBytes(m.Content)
		tc := ""
		if len(m.ToolCalls) > 0 {
			tc = `,"tool_calls":` + marshalJSONBytes(m.ToolCalls)
		}
		parts = append(parts, fmt.Sprintf(`{"role":"%s","content":%s%s}`, m.Role, c, tc))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func marshalJSONBytes(v any) string { return marshalJSON(v) }

var jsonUnmarshal = func(b []byte, v any) error { return json.Unmarshal(b, v) }
