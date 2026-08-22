package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// decodeLines parses newline-delimited json, failing on anything unparseable.
func decodeLines(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), line)
		out = append(out, m)
	}
	return out
}

// jsonInt reads a decoded json number as an int.
func jsonInt(t *testing.T, m map[string]any, key string) int {
	t.Helper()
	v, ok := m[key].(float64)
	require.True(t, ok, key)
	return int(v)
}

func TestTextSink(t *testing.T) {
	t.Parallel()

	t.Run("prose_streams_to_stdout", func(t *testing.T) {
		var out, errw bytes.Buffer
		s := newTextSink(&out, &errw)
		s.Text("the ")
		assert.Equal(t, "the", out.String()) // written before the block ends, space held
		s.Text("answer")
		s.EndText()
		s.TurnEnd(agent.TurnResult{Stop: llm.StopEndTurn, Steps: 2})
		s.finish(statusOK, exitOK, "the answer")

		assert.Equal(t, "the answer\n", out.String()) // not printed twice by finish
		assert.Empty(t, errw.String())
		assert.Equal(t, llm.StopEndTurn, s.result().Stop)
		assert.Equal(t, 2, s.result().Steps)
	})

	t.Run("blocks_end_on_their_own_line", func(t *testing.T) {
		var out, errw bytes.Buffer
		s := newTextSink(&out, &errw)
		s.Text("first")
		s.EndText()
		s.Text("second\n") // already terminated, no extra newline
		s.EndText()
		s.finish(statusOK, exitOK, "")

		assert.Equal(t, "first\nsecond\n", out.String())
	})

	t.Run("blank_block_prints_nothing", func(t *testing.T) {
		var out, errw bytes.Buffer
		s := newTextSink(&out, &errw)
		s.Text("\n\n") // what a model emits before a tool call
		s.EndText()
		s.ToolStart(agent.ToolCall{ID: "c1", Name: "ls"}, "ls")(agent.ToolResult{})
		s.Text("\n")
		s.EndText()
		s.Text("done")
		s.EndText()
		s.finish(statusOK, exitOK, "")

		assert.Equal(t, "done\n", out.String())
	})

	t.Run("trims_block_edges_keeps_middle", func(t *testing.T) {
		var out, errw bytes.Buffer
		s := newTextSink(&out, &errw)
		s.Text("\n\n  para one\n")
		s.Text("\n") // interior break, kept once content follows
		s.Text("para two\n\n\n")
		s.EndText()
		s.finish(statusOK, exitOK, "")

		assert.Equal(t, "para one\n\npara two\n", out.String())
	})

	t.Run("tool_progress_to_stderr", func(t *testing.T) {
		var out, errw bytes.Buffer
		s := newTextSink(&out, &errw)

		// a bare label takes the target from the call's streamed arguments
		s.ToolProgress(agent.ToolProgress{CallID: "c1", Name: "read", Path: "notes.go"})
		s.ToolStart(agent.ToolCall{ID: "c1", Name: "read"}, "read")(agent.ToolResult{})

		// a label already carrying its own detail is left alone
		s.ToolProgress(agent.ToolProgress{CallID: "c2", Name: "bash", Path: "ls -la"})
		done := s.ToolStart(agent.ToolCall{ID: "c2", Name: "bash"}, "bash: ls -la")
		done(agent.ToolResult{IsError: true, Content: llm.BlockList{llm.TextBlock{Text: "refused"}}})

		assert.Empty(t, out.String()) // stdout stays the model's prose alone
		assert.Equal(t, "ajent: tool: read: notes.go\najent: tool: bash: ls -la\n"+
			"ajent: tool failed: bash: refused\n", errw.String())
	})

	t.Run("notices_to_stderr", func(t *testing.T) {
		var out, errw bytes.Buffer
		s := newTextSink(&out, &errw)
		s.Notice("careful", agent.LevelWarn)
		s.finish(statusEmpty, exitTurn, "")

		assert.Empty(t, out.String()) // no prose, nothing printed
		assert.Equal(t, "ajent: warn: careful\n", errw.String())
	})
}

func TestJSONSink(t *testing.T) {
	t.Parallel()

	t.Run("full_turn_stream", func(t *testing.T) {
		var out bytes.Buffer
		s := newJSONSink(&out)

		s.TurnStart(agent.TurnInfo{Model: llm.Model{Provider: "p", ID: "m"}})
		call := agent.ToolCall{ID: "c1", Name: "read", Input: []byte(`{"path":"x.go"}`)}
		done := s.ToolStart(call, "read x.go")
		s.ToolOutput("c1", "file body")
		done(agent.ToolResult{})
		s.Text("the ")
		s.Text("answer")
		s.EndText()
		s.TurnEnd(agent.TurnResult{Stop: llm.StopEndTurn, Steps: 2, Usage: llm.Usage{Input: 10, Output: 3}})
		s.finish(statusOK, exitOK, "the answer")

		lines := decodeLines(t, out.String())
		kinds := make([]string, 0, len(lines))
		for _, l := range lines {
			kinds = append(kinds, l["type"].(string))
		}
		assert.Equal(t, []string{"turn_start", "tool_call", "tool_result", "text", "turn_end", "result"}, kinds)

		assert.Equal(t, "p/m", lines[0]["model"])
		assert.Equal(t, "c1", lines[1]["id"])
		assert.Equal(t, map[string]any{"path": "x.go"}, lines[1]["input"])
		assert.Equal(t, "file body", lines[2]["output"]) // streamed output stands in for a display string
		assert.NotContains(t, lines[2], "error")
		assert.Equal(t, "the answer", lines[3]["text"])
		assert.Equal(t, "end_turn", lines[4]["stop"])
		assert.Equal(t, 10, jsonInt(t, lines[4]["usage"].(map[string]any), "input"))

		last := lines[len(lines)-1]
		assert.Equal(t, statusOK, last["status"])
		assert.Equal(t, exitOK, jsonInt(t, last, "exit"))
		assert.Equal(t, "the answer", last["text"])
	})

	t.Run("error_result_and_notice", func(t *testing.T) {
		var out bytes.Buffer
		s := newJSONSink(&out)

		done := s.ToolStart(agent.ToolCall{ID: "c1", Name: "bash"}, "bash")
		done(agent.ToolResult{IsError: true, Content: llm.BlockList{llm.TextBlock{Text: "refused"}}})
		s.Notice("heads up", agent.LevelWarn)
		s.finish(statusEmpty, exitTurn, "")

		lines := decodeLines(t, out.String())
		require.Len(t, lines, 4)
		assert.Equal(t, true, lines[1]["error"])
		assert.Equal(t, "refused", lines[1]["output"]) // falls back to the first text block
		assert.Equal(t, "notice", lines[2]["type"])
		assert.Equal(t, "warn", lines[2]["level"])
		assert.Equal(t, exitTurn, jsonInt(t, lines[3], "exit"))
		assert.NotContains(t, lines[3], "text") // no answer, no text field
	})

	t.Run("empty_text_block_skipped", func(t *testing.T) {
		var out bytes.Buffer
		s := newJSONSink(&out)
		s.Text("   ")
		s.EndText()
		s.finish(statusEmpty, exitTurn, "")

		lines := decodeLines(t, out.String())
		require.Len(t, lines, 1)
		assert.Equal(t, "result", lines[0]["type"])
	})
}
