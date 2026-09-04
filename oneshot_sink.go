package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// Run outcomes reported on the json result line.
const (
	statusOK    = "ok"
	statusError = "error"
	statusEmpty = "empty" // the turn ended without a final answer
)

// headSink drains one headless turn onto stdout. finish reports the outcome
// once the exit code is known, and is always the last thing written.
type headSink interface {
	agent.Sink
	result() agent.TurnResult
	// summary reports the run's tool and token totals, before finish.
	summary(sessionStats)
	finish(status string, code int, answer string)
}

// levelName is a notice level's json and stderr spelling.
func levelName(l agent.Level) string {
	switch l {
	case agent.LevelWarn:
		return "warn"
	case agent.LevelError:
		return "error"
	default:
		return "info"
	}
}

// textSink streams the model's prose to stdout as it arrives and reports
// everything else — notices, tool calls, failures — on stderr, so a redirected
// stdout holds the answer alone while a terminal still shows progress.
type textSink struct {
	agent.NopSink
	out, errw io.Writer

	mu      sync.Mutex
	pending strings.Builder   // whitespace held back until content follows it
	inBlock bool              // the current text block has written something
	midLine bool              // stdout is part way through a line
	paths   map[string]string // call id to target argument, from ToolProgress
	res     agent.TurnResult
}

// newTextSink returns the drain behind --output text.
func newTextSink(out, errw io.Writer) *textSink {
	return &textSink{out: out, errw: errw, paths: make(map[string]string)}
}

// Text streams a delta, holding trailing whitespace back until content follows.
// A model routinely emits a blank block before a tool call; printing those runs
// verbatim would scatter empty lines through the output, while dropping them
// outright would flatten the paragraph breaks inside real prose.
func (s *textSink) Text(delta string) {
	if delta == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	body := strings.TrimRightFunc(delta, unicode.IsSpace)
	if body == "" {
		s.pending.WriteString(delta)
		return
	}
	tail := delta[len(body):]
	if s.inBlock {
		body = s.pending.String() + body // the held run was interior after all
	} else {
		body = strings.TrimLeftFunc(body, unicode.IsSpace) // a block never opens on blank lines
		s.inBlock = true
	}
	s.pending.Reset()
	s.pending.WriteString(tail)

	_, _ = io.WriteString(s.out, body)
	s.midLine = !strings.HasSuffix(body, "\n")
}

// EndText closes the block on exactly one newline, dropping whatever trailing
// whitespace it was holding. A block that never produced content writes nothing.
func (s *textSink) EndText() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending.Reset()
	s.inBlock = false
	s.endLineLocked()
}

// endLineLocked terminates a partial stdout line.
func (s *textSink) endLineLocked() {
	if s.midLine {
		_, _ = io.WriteString(s.out, "\n")
		s.midLine = false
	}
}

// ToolProgress records a call's target so ToolStart can name it; tools whose
// label is just their name carry no detail of their own.
func (s *textSink) ToolProgress(p agent.ToolProgress) {
	if p.Path == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paths[p.CallID] = p.Path
}

func (s *textSink) ToolStart(call agent.ToolCall, label string) func(agent.ToolResult) {
	if strings.TrimSpace(label) == "" {
		label = call.Name
	}
	s.mu.Lock()
	if path := s.paths[call.ID]; path != "" && label == call.Name {
		label += ": " + path
	}
	delete(s.paths, call.ID)
	_, _ = fmt.Fprintf(s.errw, "ajent: tool: %s\n", label)
	s.mu.Unlock()

	return func(res agent.ToolResult) {
		if !res.IsError {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		reason := strings.TrimSpace(llm.FirstBlockText(res.Content))
		if reason == "" {
			reason = "failed"
		}
		_, _ = fmt.Fprintf(s.errw, "ajent: tool failed: %s: %s\n", call.Name, reason)
	}
}

func (s *textSink) Notice(msg string, level agent.Level) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = fmt.Fprintf(s.errw, "ajent: %s: %s\n", levelName(level), msg)
}

func (s *textSink) TurnEnd(r agent.TurnResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.res = r
}

func (s *textSink) result() agent.TurnResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.res
}

// summary writes the totals to stderr, keeping stdout the answer alone.
func (s *textSink) summary(st sessionStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endLineLocked()
	writeStats(s.errw, "ajent: ", st)
}

// finish only closes a partial line, for a turn that ended mid-block: the answer
// already streamed through Text.
func (s *textSink) finish(_ string, _ int, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending.Reset()
	s.endLineLocked()
}

// jsonEvent is one line of --output json. Every field but the type is omitted
// when unset, so a line carries only what its type means.
type jsonEvent struct {
	Type   string          `json:"type"`
	Model  string          `json:"model,omitempty"`
	Text   string          `json:"text,omitempty"`
	ID     string          `json:"id,omitempty"`
	Name   string          `json:"name,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output string          `json:"output,omitempty"`
	Error  bool            `json:"error,omitempty"`
	Level  string          `json:"level,omitempty"`
	Stop   string          `json:"stop,omitempty"`
	Steps  int             `json:"steps,omitempty"`
	Usage  *llm.Usage      `json:"usage,omitempty"`
}

// jsonSummary is the optional summary line of --output json, emitted before the
// result line so that line's contract is unchanged.
type jsonSummary struct {
	Type string `json:"type"`
	sessionStats
}

// jsonResult is the last line of --output json: how the run ended, the code the
// process exits with, and the final answer.
type jsonResult struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Exit   int    `json:"exit"`
	Text   string `json:"text,omitempty"`
}

// jsonSink writes one JSON object per line while the turn runs. Tool lines carry
// an id because parallel dispatch may interleave them, even though the
// transcript still records results in call order.
type jsonSink struct {
	agent.NopSink
	out io.Writer

	mu     sync.Mutex
	text   strings.Builder   // the assistant block currently streaming
	output map[string]string // streamed tool output by call id
	res    agent.TurnResult
}

// newJSONSink returns the drain behind --output json.
func newJSONSink(out io.Writer) *jsonSink {
	return &jsonSink{out: out, output: make(map[string]string)}
}

// emit writes one object, already under lock.
func (s *jsonSink) emit(v any) {
	line, err := json.Marshal(v)
	if err != nil {
		return // an unencodable event is dropped rather than corrupting the stream
	}
	_, _ = fmt.Fprintf(s.out, "%s\n", line)
}

func (s *jsonSink) TurnStart(i agent.TurnInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emit(jsonEvent{Type: "turn_start", Model: i.Model.Key()})
}

func (s *jsonSink) Text(delta string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.text.WriteString(delta)
}

func (s *jsonSink) EndText() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if text := strings.TrimSpace(s.text.String()); text != "" {
		s.emit(jsonEvent{Type: "text", Text: text})
	}
	s.text.Reset()
}

func (s *jsonSink) ToolStart(call agent.ToolCall, _ string) func(agent.ToolResult) {
	s.mu.Lock()
	s.emit(jsonEvent{Type: "tool_call", ID: call.ID, Name: call.Name, Input: call.Input})
	s.mu.Unlock()

	return func(res agent.ToolResult) {
		s.mu.Lock()
		defer s.mu.Unlock()
		out := res.Display
		if strings.TrimSpace(out) == "" {
			out = s.output[call.ID]
		}
		if strings.TrimSpace(out) == "" {
			out = llm.FirstBlockText(res.Content)
		}
		delete(s.output, call.ID)
		s.emit(jsonEvent{
			Type: "tool_result", ID: call.ID, Name: call.Name,
			Output: out, Error: res.IsError,
		})
	}
}

func (s *jsonSink) ToolOutput(callID, delta string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.output[callID] += delta
}

func (s *jsonSink) Notice(msg string, level agent.Level) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emit(jsonEvent{Type: "notice", Level: levelName(level), Text: msg})
}

func (s *jsonSink) TurnEnd(r agent.TurnResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.res = r
	usage := r.Usage
	s.emit(jsonEvent{Type: "turn_end", Stop: r.Stop.String(), Steps: r.Steps, Usage: &usage})
}

func (s *jsonSink) result() agent.TurnResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.res
}

func (s *jsonSink) summary(st sessionStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emit(jsonSummary{Type: "summary", sessionStats: st})
}

func (s *jsonSink) finish(status string, code int, answer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emit(jsonResult{Type: "result", Status: status, Exit: code, Text: answer})
}
