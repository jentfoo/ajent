package llm

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/go-analyze/bulk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The parity matrix. Three dialects encode one exchange very differently on the
// wire, and testdata/contract holds each dialect's encoding of the same logical
// scenario. What must agree is what reaches the caller, so every assertion here
// is on the normalised outcome rather than the frames that carried it.
//
// Event ordering is deliberately not compared: anthropic reports usage up front
// and chat-completions reports it last, which is a real difference in when a
// number arrives, not in the vocabulary. The distinct kinds are compared, the
// sequence is not. Tool call ids are not compared either, because Prepare
// derives them per dialect on purpose (see normalizeCallID).
//
// A new dialect adds a row to contractDialects and a testdata/contract/<dir>
// beside the others; a new scenario adds one file per dialect.

type contractDialect struct {
	name        string
	dir         string
	model       Model
	newProvider func(*testing.T, string) Provider
}

func contractDialects() []contractDialect {
	return []contractDialect{
		{
			name: "anthropic", dir: "anthropic", model: anthropicModel(nil),
			newProvider: func(t *testing.T, url string) Provider { t.Helper(); return newAnthropicTestProvider(t, url) },
		}, {
			name: "responses", dir: "openai", model: responsesModel(nil),
			newProvider: func(t *testing.T, url string) Provider { t.Helper(); return newResponsesTestProvider(t, url) },
		}, {
			name: "chat_completions", dir: "compat", model: compatModel(nil),
			newProvider: func(t *testing.T, url string) Provider { t.Helper(); return newCompatTestProvider(t, url) },
		},
	}
}

// contractCall is a tool call stripped to what every dialect must agree on.
type contractCall struct {
	Name string
	Args string
}

// contractResult is one scenario's normalised outcome, the unit of comparison
// between dialects.
type contractResult struct {
	Text     string
	Thinking string
	Calls    []contractCall
	Stop     StopReason
	Input    int
	Output   int
	Kinds    []string
}

// runContract replays one dialect's encoding of a scenario and reduces the
// stream to its normalised outcome. A zero chunk writes whole frames.
func runContract(t *testing.T, d contractDialect, scenario string, chunk int) (contractResult, error) {
	t.Helper()

	srv, _ := sseServerChunked(t, filepath.Join("contract", d.dir, scenario+".sse"), chunk)
	p := d.newProvider(t, srv.URL)

	s, err := p.Stream(t.Context(), Request{Model: d.model})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	var acc Accumulator
	var events []Event
	for ev, ok := s.Next(); ok; ev, ok = s.Next() {
		acc.Add(ev)
		events = append(events, ev)
	}

	got := contractResult{
		Text:     textOf(events),
		Thinking: thinkingOf(events),
		Stop:     acc.StopReason(),
		Input:    acc.Usage().Input,
		Output:   acc.Usage().Output,
		Kinds:    distinctKinds(events),
	}
	for _, b := range acc.Message().Content {
		if call, ok := b.(ToolCallBlock); ok {
			got.Calls = append(got.Calls, contractCall{Name: call.Name, Args: canonicalJSON(t, call.Input)})
		}
	}
	return got, s.Err()
}

// distinctKinds is the sorted set of event kinds a stream produced, so the
// vocabulary is compared without pinning arrival order.
func distinctKinds(events []Event) []string {
	kinds := bulk.SliceToSet(eventKinds(events))
	out := bulk.MapKeysSlice(kinds)
	slices.Sort(out)
	return out
}

// canonicalJSON re-marshals so formatting differences between fixtures cannot
// masquerade as a behaviour difference.
func canonicalJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal(raw, &v))
	out, err := json.Marshal(v)
	require.NoError(t, err)
	return string(out)
}

func TestContractStreamParity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		scenario string
		want     contractResult
		wantErr  bool
	}{
		{
			scenario: "text",
			want: contractResult{
				Text: "Hello there", Stop: StopEndTurn, Input: 100, Output: 7,
				Kinds: []string{"done", "message_start", "text_delta", "text_end", "text_start", "usage"},
			},
		}, {
			scenario: "reasoning",
			want: contractResult{
				Text: "the answer", Thinking: "let me consider", Stop: StopEndTurn, Input: 100, Output: 20,
				Kinds: []string{"done", "message_start", "text_delta", "text_end", "text_start",
					"thinking_delta", "thinking_end", "thinking_start", "usage"},
			},
		}, {
			scenario: "tool_call",
			want: contractResult{
				Text:  "Checking",
				Calls: []contractCall{{Name: "read", Args: `{"path":"main.go"}`}},
				Stop:  StopToolUse, Input: 100, Output: 37,
				Kinds: []string{"done", "message_start", "text_delta", "text_end", "text_start",
					"tool_call_delta", "tool_call_end", "tool_call_start", "usage"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.scenario, func(t *testing.T) {
			for _, d := range contractDialects() {
				got, err := runContract(t, d, tt.scenario, 0)
				require.NoError(t, err, d.name)
				assert.Equal(t, tt.want, got, "%s must normalise to the shared outcome", d.name)
			}
		})
	}
}

// TestContractErrorParity keeps failure on the same contract as success: the
// partial content survives and the error is reported, on every dialect.
func TestContractErrorParity(t *testing.T) {
	t.Parallel()

	for _, d := range contractDialects() {
		t.Run(d.name, func(t *testing.T) {
			got, err := runContract(t, d, "error_midstream", 0)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "upstream exploded")
			assert.Equal(t, "partial", got.Text, "the partial answer is not discarded")
		})
	}
}

// TestContractChunkedParity re-runs the matrix with writes landing inside JSON
// tokens, proving reassembly happens before decoding on every dialect.
func TestContractChunkedParity(t *testing.T) {
	t.Parallel()

	for _, scenario := range []string{"text", "reasoning", "tool_call"} {
		t.Run(scenario, func(t *testing.T) {
			for _, d := range contractDialects() {
				whole, err := runContract(t, d, scenario, 0)
				require.NoError(t, err, d.name)
				// 17 is coprime with the frame lengths, so boundaries land mid-token
				split, err := runContract(t, d, scenario, 17)
				require.NoError(t, err, d.name)
				assert.Equal(t, whole, split, "%s must not depend on write boundaries", d.name)
			}
		})
	}
}

// contractBody builds one dialect's request body, the other half of the
// contract: Prepare is the single normalisation pass every builder goes through.
func contractBody(t *testing.T, d contractDialect, req Request) []byte {
	t.Helper()

	req.Model = d.model
	var body []byte
	var err error
	switch d.dir {
	case "anthropic":
		body, err = buildAnthropicBody(req)
	case "openai":
		body, err = buildResponsesBody(req)
	default:
		body, err = buildCompatBody(req, compatProfile{name: "lmstudio"})
	}
	require.NoError(t, err)
	return body
}

// callsAndResults walks a request body for the tool call ids it offers and the
// ids its results answer, whatever shape the dialect wraps them in.
func callsAndResults(t *testing.T, body []byte) ([]string, []string) {
	t.Helper()

	var root any
	require.NoError(t, json.Unmarshal(body, &root))

	var calls, results []string
	var walk func(v any)
	walk = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			typ, _ := node["type"].(string)
			switch {
			case typ == "tool_use":
				calls = append(calls, str(node["id"]))
			case typ == "function_call":
				calls = append(calls, str(node["call_id"]))
			case node["function"] != nil && node["id"] != nil:
				calls = append(calls, str(node["id"]))
			case node["tool_use_id"] != nil:
				results = append(results, str(node["tool_use_id"]))
			case node["tool_call_id"] != nil:
				results = append(results, str(node["tool_call_id"]))
			case typ == "function_call_output":
				results = append(results, str(node["call_id"]))
			}
			for _, child := range node {
				walk(child)
			}
		case []any:
			for _, child := range node {
				walk(child)
			}
		}
	}
	walk(root)
	return calls, results
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// TestContractRequestParity sends one exchange through all three body builders:
// the wire shapes differ entirely, but each must carry the same content and
// leave every tool result matched to a call in the same body.
func TestContractRequestParity(t *testing.T) {
	t.Parallel()

	req := Request{
		System:    BlockList{TextBlock{Text: "system rules"}},
		MaxTokens: 256,
		Tools: []ToolSchema{{
			Name: "read", Description: "read a file",
			Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
		Messages: []Message{
			Text(RoleUser, "read main.go"),
			{Role: RoleAssistant, Content: BlockList{
				TextBlock{Text: "Checking"},
				// a responses-style id: replaying it to another dialect must rewrite
				// it, and the result has to follow (see normalizeCallID)
				ToolCallBlock{ID: "call_abc|fc_1", Name: "read", Input: json.RawMessage(`{"path":"main.go"}`)},
			}},
			{Role: RoleUser, Content: BlockList{
				ToolResultBlock{CallID: "call_abc|fc_1", ToolName: "read",
					Content: BlockList{TextBlock{Text: "package main"}}},
			}},
			Text(RoleUser, "thanks"),
		},
	}

	for _, d := range contractDialects() {
		t.Run(d.name, func(t *testing.T) {
			body := contractBody(t, d, req)
			text := string(body)

			for _, want := range []string{"system rules", "read main.go", "Checking", "package main", "thanks"} {
				assert.Contains(t, text, want, "every dialect carries the same content")
			}
			assert.Contains(t, text, `"read"`, "the tool schema is offered")

			calls, results := callsAndResults(t, body)
			require.Len(t, calls, 1)
			require.Len(t, results, 1)
			require.NotEmpty(t, calls[0])
			assert.Equal(t, calls[0], results[0], "the result answers the call as this dialect names it")
		})
	}
}
