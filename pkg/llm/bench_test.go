package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The benchmarks here share one synthetic conversation, sized like a long
// working session. Build it once per benchmark and keep the per-op work on the
// code under test.

// benchTools is the offered set: four immediate tools and two deferred ones.
func benchTools() []ToolSchema {
	schema := `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`
	return []ToolSchema{
		{Name: "read", Description: "read a file from the workspace", Parameters: json.RawMessage(schema)},
		{Name: "grep", Description: "search file contents", Parameters: json.RawMessage(schema)},
		{Name: "edit", Description: "edit a single file", Parameters: json.RawMessage(schema)},
		{Name: "bash", Description: "run a shell command", Parameters: json.RawMessage(schema)},
		{Name: "ctx7_docs", Description: "library docs lookup", Deferred: true, Parameters: json.RawMessage(schema)},
		{Name: "web_search", Description: "web search", Deferred: true, Parameters: json.RawMessage(schema)},
	}
}

// benchReasoning is the configured default: medium depth, thinking kept for the
// current tool-calling turn.
var benchReasoning = ReasoningConfig{Level: LevelMedium, Retain: RetainWholeTurn}

// benchModel resolves the fixture's target model for a dialect. The compat
// model carries the deepseek-style quirks that give the normalisation passes
// their branch work, and images are off so Prepare's downgrade ladder runs.
func benchModel(d Dialect) Model {
	var caps Capabilities
	provider := providerOpenAI
	switch d {
	case DialectAnthropic:
		caps = flavorDefaults[FlavorAnthropic].caps
		caps.ToolReferences = true
		provider = providerAnthropic
	case DialectOpenAIResponses:
		caps = flavorDefaults[FlavorOpenAI].caps
		caps.SupportsAdditionalTools = true
	default: // chat-completions
		caps = chatCaps()
		caps.ThinkOpen, caps.ThinkClose = thinkOpenTag, thinkCloseTag
		caps.ReasoningField = fieldReasoningConten
		caps.ReplayReasoning = true
		caps.RequiresAssistantAfterToolResult = true
	}
	caps.Images = false
	if len(caps.Budgets) == 0 {
		caps.Budgets = defaultBudgets(64000)
	}
	return Model{Provider: provider, ID: "bench-model",
		ContextWindow: 200000, MaxOutput: 64000, Caps: caps}
}

// benchImage is a small synthetic photo payload.
func benchImage() ImageBlock {
	data := make([]byte, 2048)
	for i := range data {
		data[i] = byte(i * 31)
	}
	return ImageBlock{MediaType: "image/jpeg", Data: data}
}

// benchConversation builds the shared fixture: ~100 messages carrying thinking
// blocks with replay tokens, parallel tool calls answered by results, deferred
// tool references, images, a leaked think-tag tail, a malformed tool input, an
// errored turn with its results, and an aborted turn with unanswered calls. A
// few messages carry foreign origins, what a mid-session model switch produces.
func benchConversation(m Model) []Message {
	own := Origin{Provider: m.Provider, Dialect: m.Caps.Dialect, Model: m.ID}
	switched := Origin{Provider: m.Provider, Dialect: m.Caps.Dialect, Model: "other-model"}
	crossed := Origin{Provider: m.Provider, Dialect: DialectOpenAICompletions, Model: "elsewhere"}

	msgs := make([]Message, 0, 2*benchTurns+6)
	msgs = append(msgs, Message{Role: RoleUser, Origin: &own, Content: BlockList{
		TextBlock{Text: strings.Repeat("audit the parser package and fix the panics. ", 4)},
		benchImage(),
	}})
	for i := 0; i < benchTurns; i++ {
		origin := own
		idSuffix := ""
		switch i % 9 { // a few turns arrive from before a model switch or another endpoint
		case 4:
			origin, idSuffix = switched, "|fc_"+fmt.Sprintf("%04x", i*7)
		case 7:
			origin, idSuffix = crossed, "|fc_"+fmt.Sprintf("%04x", i*13)
		}

		text := fmt.Sprintf("reading the reducer path, pass %d. ", i) +
			strings.Repeat("the window holds one block per turn, so growth is bounded. ", 4)
		if i == 7 { // a chat-completions tail leaked into the text channel
			text += "<think>the tags landed on content, not reasoning</think>"
		}
		content := BlockList{
			ThinkingBlock{Text: fmt.Sprintf("plan pass %d: isolate the trim boundary first", i),
				Signature: fmt.Sprintf("sig-%03d", i), ItemID: fmt.Sprintf("rs-%03d", i),
				Encrypted: fmt.Sprintf("enc-%03d", i), Field: fieldReasoningConten},
			TextBlock{Text: text},
			ToolCallBlock{ID: fmt.Sprintf("call_%03d_a%s", i, idSuffix), Name: "read",
				Input: json.RawMessage(fmt.Sprintf(`{"path":"internal/parser/reduce_%03d.go"}`, i))},
			ToolCallBlock{ID: fmt.Sprintf("call_%03d_b%s", i, idSuffix), Name: "grep",
				Input: json.RawMessage(`{"pattern":"panic","glob":"*.go"}`)},
		}
		if i == 11 { // malformed arguments ride raw until repairToolInputs
			content[3] = ToolCallBlock{ID: fmt.Sprintf("call_%03d_b%s", i, idSuffix), Name: "grep",
				Input: json.RawMessage(`{"pattern":`)}
		}
		if i == 13 { // an errored turn is dropped wholesale with its results
			content = append(content[:1:1], content[1:]...) // keep thinking and text
			content = append(content[:2:2],
				ToolCallBlock{ID: fmt.Sprintf("call_%03d_err%s", i, idSuffix), Name: "bash",
					Input: json.RawMessage(`{"command":"make test"}`)})
			msgs = append(msgs, Message{Role: RoleAssistant, Origin: &origin, Stop: StopError, Content: content})
			msgs = append(msgs, Message{Role: RoleUser, Origin: &own, Content: BlockList{
				ToolResultBlock{CallID: fmt.Sprintf("call_%03d_err%s", i, idSuffix), ToolName: "bash",
					Content: BlockList{TextBlock{Text: "exit 1: one test failed"}}},
			}})
			continue
		}
		msgs = append(msgs, Message{Role: RoleAssistant, Origin: &origin, Stop: StopToolUse, Content: content})

		result := ToolResultBlock{CallID: fmt.Sprintf("call_%03d_a%s", i, idSuffix), ToolName: "read",
			Content: BlockList{TextBlock{Text: fmt.Sprintf("package parser\n\n// reduce_%03d.go\n", i) +
				strings.Repeat("func step(n int) int { return n % 97 }\n", 8)}}}
		if i%5 == 2 { // a reference load rides the result it was triggered by
			result.AddedToolNames = []string{"ctx7_docs"}
		}
		results := BlockList{result}
		if i%7 == 3 { // images arrive inside tool output too, back to back
			results = append(results,
				ToolResultBlock{CallID: fmt.Sprintf("call_%03d_b%s", i, idSuffix), ToolName: "grep",
					Content: BlockList{benchImage(), benchImage(),
						TextBlock{Text: "two matches, rendered as screenshots"}}})
		} else {
			results = append(results,
				ToolResultBlock{CallID: fmt.Sprintf("call_%03d_b%s", i, idSuffix), ToolName: "grep",
					Content: BlockList{TextBlock{Text: strings.Repeat(fmt.Sprintf("reduce.go:%d: panic recovered\n", i), 6)}}})
		}
		msgs = append(msgs, Message{Role: RoleUser, Origin: &own, Content: results})
		if i%6 == 2 { // a user interjection follows the results
			msgs = append(msgs, Message{Role: RoleUser, Origin: &own, Content: BlockList{
				TextBlock{Text: fmt.Sprintf("also check the %dth caller before you edit", i)}}})
		}
	}
	// an aborted turn: its calls were never answered, so repairTurns synthesizes
	// the missing results
	msgs = append(msgs, Message{Role: RoleAssistant, Origin: &own, Stop: StopAborted, Content: BlockList{
		ToolCallBlock{ID: "call_aborted_a", Name: "edit",
			Input: json.RawMessage(`{"path":"reduce.go","edits":[]}`)},
		ToolCallBlock{ID: "call_aborted_b", Name: "bash",
			Input: json.RawMessage(`{"command":"go test ./internal/parser"}`)},
	}})
	return msgs
}

const benchTurns = 48 // turn pairs, ~100 messages with the extras

// benchSystem is the fixture's system block.
func benchSystem() BlockList {
	return BlockList{TextBlock{Text: strings.Repeat(
		"You are ajent, a terminal coding agent. Prefer the edit tool over shell writes. ", 12)}}
}

// benchRequest assembles the fixture into a request for m.
func benchRequest(m Model) Request {
	return Request{
		Model:     m,
		System:    benchSystem(),
		Messages:  benchConversation(m),
		Tools:     benchTools(),
		MaxTokens: 8192,
		Reasoning: benchReasoning,
	}
}

// BenchmarkPrepare sweeps the per-request normalisation over the shared
// fixture, one full Prepare pass per op. Messages are cloned per op so every
// pass measures the raw, unprepared list a fresh request carries.
func BenchmarkPrepare(b *testing.B) {
	raw := benchRequest(benchModel(DialectAnthropic))
	compat := benchRequest(benchModel(DialectOpenAICompletions))

	b.Run("anthropic", func(b *testing.B) {
		for b.Loop() {
			req := raw
			req.Messages = slices.Clone(raw.Messages)
			_ = Prepare(req)
		}
	})
	b.Run("completions", func(b *testing.B) {
		for b.Loop() {
			req := compat
			req.Messages = slices.Clone(compat.Messages)
			_ = Prepare(req)
		}
	})
}

// BenchmarkPrepareSettled measures the no-change re-pass providers make over
// already-normalized messages: every stage should scan, not rebuild.
func BenchmarkPrepareSettled(b *testing.B) {
	for _, tc := range []struct {
		name string
		raw  Request
	}{
		{"anthropic", benchRequest(benchModel(DialectAnthropic))},
		{"completions", benchRequest(benchModel(DialectOpenAICompletions))},
	} {
		b.Run(tc.name, func(b *testing.B) {
			settled := Prepare(tc.raw)
			again := Prepare(settled)
			require.Equal(b, settled.Messages, again.Messages)
			require.Same(b, &settled.Messages[0], &again.Messages[0]) // a scan shares the list

			for b.Loop() {
				_ = Prepare(settled)
			}
		})
	}
}

// BenchmarkProviderWire measures wire assembly off the prepared fixture: the
// per-dialect block builders, and one full Stream round trip through a fake
// server for the marshal, hardened client and redaction path.
func BenchmarkProviderWire(b *testing.B) {
	anthropic := benchModel(DialectAnthropic)
	compat := benchModel(DialectOpenAICompletions)
	responses := benchModel(DialectOpenAIResponses)
	tools := benchTools()

	preparedAnthropic := Prepare(benchRequest(anthropic))
	preparedCompat := Prepare(benchRequest(compat))
	preparedResponses := Prepare(benchRequest(responses))
	require.NotEmpty(b, preparedAnthropic.Messages)
	require.NotEmpty(b, preparedCompat.Messages)

	b.Run("anthropic_blocks", func(b *testing.B) {
		caps := anthropic.Caps
		deferred := anthropicDeferredNames(tools, caps)
		_, err := anthropicBlocks(preparedAnthropic.Messages[1].Content, caps, deferred, map[string]bool{})
		require.NoError(b, err)

		for b.Loop() {
			loaded := make(map[string]bool, 2) // request-scoped reference dedup
			for i := range preparedAnthropic.Messages {
				if _, err := anthropicBlocks(preparedAnthropic.Messages[i].Content, caps, deferred, loaded); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	b.Run("compat_messages", func(b *testing.B) {
		caps := compat.Caps
		msgs, err := compatMessages(preparedCompat, caps)
		require.NoError(b, err)
		require.NotEmpty(b, msgs)

		for b.Loop() {
			if _, err := compatMessages(preparedCompat, caps); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("responses_items", func(b *testing.B) {
		caps := responses.Caps
		items, err := responsesInput(preparedResponses, caps)
		require.NoError(b, err)
		require.NotEmpty(b, items)

		for b.Loop() {
			if _, err := responsesInput(preparedResponses, caps); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("stream_e2e", func(b *testing.B) {
		srv := benchSSEServer(b, "anthropic/thinking_usage.sse")
		c, err := newHTTPClient(clientOptions{provider: providerAnthropic, baseURL: srv.URL})
		require.NoError(b, err)
		p := newAnthropicProvider(providerAnthropic, c)
		req := benchRequest(anthropic)

		ctx := b.Context()
		s, err := p.Stream(ctx, req)
		require.NoError(b, err)
		msg, _, err := Accumulate(s)
		require.NoError(b, err)
		require.NotEmpty(b, msg.Content)

		b.ResetTimer()
		for b.Loop() {
			s, err := p.Stream(ctx, req)
			if err != nil {
				b.Fatal(err)
			}
			if _, _, err := Accumulate(s); err != nil {
				b.Fatal(err)
			}
			if err := s.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// benchSSEServer replays a testdata fixture as an event stream, split once so
// the handler only writes chunks inside the measured loop.
func benchSSEServer(b *testing.B, fixture string) *httptest.Server {
	b.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", fixture))
	require.NoError(b, err)
	chunks := splitStream(string(data), 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range chunks {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	}))
	b.Cleanup(srv.Close)
	return srv
}

// chunkReader hands back at most size bytes per Read, so a parser sees frames
// split mid-line the way socket writes deliver them.
type chunkReader struct {
	data []byte
	pos  int
	size int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := min(r.size, len(p), len(r.data)-r.pos)
	r.pos += copy(p[:n], r.data[r.pos:])
	return n, nil
}

// benchSSEBody builds a delta stream shaped like an OpenAI-dialect response:
// many small data frames, each with one field line and a blank dispatch line.
func benchSSEBody(frames int) string {
	var sb strings.Builder
	for i := range frames {
		sb.WriteString("event: message\ndata: ")
		sb.WriteString(`{"id":"chatcmpl-` + strconv.Itoa(i) +
			`","choices":[{"delta":{"content":" the parser keeps a window "}}]}`)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// BenchmarkSSEFrames measures framing alone: Next and readLine over a stream of
// realistic delta frames, with no provider decode behind it. The chunk size is
// what the wire decides, so the cases span an MTU-sized write and small writes
// that split almost every line.
func BenchmarkSSEFrames(b *testing.B) {
	ctx := b.Context()
	small := benchSSEBody(500)
	wide := strings.Repeat("x", 1<<17) // one data line past the bufio buffer

	for _, tc := range []struct {
		name, body string
		chunk      int
	}{
		{"mtu_chunks", small, 1400},
		{"tiny_chunks", small, 37},
		{"one_byte", small, 1},
		{"oversize_line", "event: message\ndata: " + wide + "\n\n", 512},
	} {
		b.Run(tc.name, func(b *testing.B) {
			want := strings.Count(tc.body, "\n\n")

			for b.Loop() {
				r := NewSSEReader(&chunkReader{data: []byte(tc.body), size: tc.chunk}, 0)
				var got int
				for {
					f, err := r.Next(ctx)
					if errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						b.Fatal(err)
					}
					if len(f.Data) == 0 {
						b.Fatal("frame with no data")
					}
					got++
				}
				if got != want {
					b.Fatalf("%d frames, want %d", got, want)
				}
			}
		})
	}
}

// benchTurnEvents shapes one provider response the way the streams deliver it:
// thinking, text and three tool calls fed in small deltas. With truncated set
// the last call never closes, so Message must rebuild it from buffered deltas.
func benchTurnEvents(truncated bool) []Event {
	const args = `{"path":"internal/parser/parse.go","oldText":"return buf.trim()","newText":"return buf.trimRight()"}`
	evs := make([]Event, 0, 128)
	evs = append(evs,
		Event{Type: EventMessageStart, Meta: &StreamMeta{Model: "bench-model", RequestID: "req_bench"}},
		Event{Type: EventUsage, Usage: Usage{Input: 48231, Output: 3, CacheRead: 39000, CacheWrite: 1200}},
	)

	var think strings.Builder
	evs = append(evs, Event{Type: EventThinkingStart, Index: 0})
	for i := range 24 {
		d := fmt.Sprintf(" step %d: weigh the parse order against buffer reuse", i)
		think.WriteString(d)
		evs = append(evs, Event{Type: EventThinkingDelta, Index: 0, Text: d})
	}
	evs = append(evs, Event{Type: EventThinkingEnd, Index: 0,
		Block: ThinkingBlock{Text: think.String(), Signature: "sig-bench"}})

	var text strings.Builder
	evs = append(evs, Event{Type: EventTextStart, Index: 1})
	for i := range 32 {
		d := fmt.Sprintf(" The parser keeps a %d-line window, so the fix lands there. ", i)
		text.WriteString(d)
		evs = append(evs, Event{Type: EventTextDelta, Index: 1, Text: d})
	}
	evs = append(evs, Event{Type: EventTextEnd, Index: 1, Block: TextBlock{Text: text.String()}})

	for c, name := range []string{"read", "edit", "bash"} {
		idx := 2 + c
		id := fmt.Sprintf("toolu_bench_%02d", c)
		evs = append(evs, Event{Type: EventToolCallStart, Index: idx, ToolCallID: id, ToolName: name})
		for i := 0; i < len(args); i += 8 {
			evs = append(evs, Event{Type: EventToolCallDelta, Index: idx, ToolCallID: id,
				Text: args[i:min(i+8, len(args))]})
		}
		if truncated && c == 2 {
			break // the stream ends mid-call
		}
		evs = append(evs, Event{Type: EventToolCallEnd, Index: idx, ToolCallID: id, ToolName: name,
			Block: ToolCallBlock{ID: id, Name: name, Input: json.RawMessage(args)}})
	}
	evs = append(evs, Event{Type: EventUsage,
		Usage: Usage{Input: 48231, Output: 812, CacheRead: 39000, CacheWrite: 1200, Reasoning: 250}})
	if !truncated {
		evs = append(evs, Event{Type: EventDone, StopReason: StopToolUse})
	}
	return evs
}

// BenchmarkAccumulator measures the per-event assembly of the assistant
// message: delta bookkeeping, and the pending rebuild a truncated stream forces
// on Message. One op is one full event sequence.
func BenchmarkAccumulator(b *testing.B) {
	for _, tc := range []struct {
		name      string
		events    []Event
		wantBlock int // blocks the final Message must carry
	}{
		{"complete_turn", benchTurnEvents(false), 5},
		{"truncated_tail", benchTurnEvents(true), 5}, // the open call rebuilt in place of its end event
	} {
		b.Run(tc.name, func(b *testing.B) {
			var acc Accumulator
			for _, ev := range tc.events {
				acc.Add(ev)
			}
			msg := acc.Message()
			require.Len(b, msg.Content, tc.wantBlock)
			if tc.events[len(tc.events)-1].Type != EventDone {
				_, ok := msg.Content[len(msg.Content)-1].(ToolCallBlock)
				require.True(b, ok) // the open call was rebuilt from its deltas
			}
			b.ReportMetric(float64(len(tc.events)), "events/op")

			for b.Loop() {
				var acc Accumulator
				for _, ev := range tc.events {
					acc.Add(ev)
				}
				if msg := acc.Message(); len(msg.Content) != tc.wantBlock {
					b.Fatalf("assembled %d blocks, want %d", len(msg.Content), tc.wantBlock)
				}
			}
		})
	}
}

// BenchmarkRepairToolCallIDs measures the id repair that runs after every
// streamed assistant message: the used-id set rebuild over the whole history
// plus the rewrite itself, across a growing session.
func BenchmarkRepairToolCallIDs(b *testing.B) {
	m := benchModel(DialectAnthropic)
	full := benchConversation(m)
	dup := Message{Role: RoleAssistant, Stop: StopToolUse, Content: BlockList{
		ToolCallBlock{ID: "call_010_a", Name: "read", // an id history already spent
			Input: json.RawMessage(`{"path":"internal/parser/reduce.go"}`)},
		ToolCallBlock{ID: "call_010_a", Name: "grep", // repeated in m
			Input: json.RawMessage(`{"pattern":"panic","glob":"*.go"}`)},
		ToolCallBlock{ID: "", Name: "bash", // absent id, hashed
			Input: json.RawMessage(`{"command":"go test ./..."}`)},
	}}

	for _, frac := range []int{4, 2, 1} {
		name := "full_history"
		history := full
		if frac > 1 {
			history = full[len(full)-len(full)/frac:]
			name = map[int]string{4: "quarter_history", 2: "half_history"}[frac]
		}
		b.Run(name, func(b *testing.B) {
			_, changed := RepairToolCallIDs(dup, history)
			require.True(b, changed)
			b.ReportMetric(float64(len(history)), "hist-msgs/op")

			for b.Loop() {
				RepairToolCallIDs(dup, history)
			}
		})
	}
}

// BenchmarkShortHash measures the call-id digest on the shapes repair feeds it:
// a short foreign item id and a longer one, so the inline loop stays honest
// against the hash/fnv implementation it replaced.
func BenchmarkShortHash(b *testing.B) {
	for _, tc := range []struct{ name, in string }{
		{"short_id", "fc_01H8ZQ9W3"},
		{"long_id", "item_respond_01H8ZQ9W3K2M4N6P8R0T2V4X6Z8A1C3E5G7J9L"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for b.Loop() {
				shortHash(tc.in)
			}
		})
	}
}
