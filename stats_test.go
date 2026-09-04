package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatsSinkCounts(t *testing.T) {
	t.Parallel()

	s := newStatsSink()
	record := func(name string, failed bool) {
		s.ToolStart(agent.ToolCall{ID: "c", Name: name}, name)(agent.ToolResult{IsError: failed})
	}
	record("read", false)
	record("edit", false)
	record("edit", true)
	record("edit", false)

	got := s.collect(nil, 2*time.Second)
	assert.Equal(t, map[string]int{"read": 1, "edit": 3}, got.Calls)
	assert.Equal(t, map[string]int{"edit": 1}, got.Failed) // only the failure counts
	assert.InDelta(t, 2.0, got.Seconds, 0.001)
}

func TestThousands(t *testing.T) {
	t.Parallel()

	cases := map[int]string{0: "0", 5: "5", 999: "999", 1000: "1,000",
		12345: "12,345", 184203: "184,203", 1000000: "1,000,000"}
	for in, want := range cases {
		assert.Equal(t, want, thousands(in))
	}
}

func TestWriteStats(t *testing.T) {
	t.Parallel()

	st := sessionStats{
		Turns:   14,
		Seconds: 192,
		Calls:   map[string]int{"read": 9, "edit": 11, "bash": 3},
		Failed:  map[string]int{"edit": 2},
		Usage:   llm.Usage{Input: 184203, Output: 12880, CacheRead: 160112},
		ByModel: map[string]llm.Usage{"claude-opus-5": {Input: 184203, Output: 12880}},
	}

	var b bytes.Buffer
	writeStats(&b, "ajent: ", st)
	out := b.String()

	assert.Contains(t, out, "turns 14  wall 3m12s")
	assert.Contains(t, out, "bash 3  edit 11 (2 failed)  read 9") // name order, failures marked
	assert.Contains(t, out, "tokens in 184,203  out 12,880  cache-r 160,112")
	assert.Contains(t, out, "claude-opus-5  in 184,203")
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		assert.True(t, strings.HasPrefix(line, "ajent: ")) // stdout stays the answer alone
	}
}

func TestJSONSinkSummaryPrecedesResult(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	s := newJSONSink(&b)
	s.summary(sessionStats{Turns: 2, Calls: map[string]int{"edit": 1}})
	s.finish(statusOK, exitOK, "done")

	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	require.Len(t, lines, 2)

	var summary struct {
		Type  string         `json:"type"`
		Turns int            `json:"turns"`
		Calls map[string]int `json:"calls"`
	}
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &summary))
	assert.Equal(t, "summary", summary.Type)
	assert.Equal(t, 2, summary.Turns)
	assert.Equal(t, map[string]int{"edit": 1}, summary.Calls)

	var result jsonResult
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &result))
	assert.Equal(t, "result", result.Type) // the result line stays last
}

// usageCallTurn scripts a turn that reports token usage and then calls one tool,
// so a run produces both the tool events and the accounting the summary reads.
func usageCallTurn(in, out int, id, name, args string) []llm.Event {
	return []llm.Event{
		{Type: llm.EventMessageStart},
		{Type: llm.EventUsage, Usage: llm.Usage{Input: in, Output: out}},
		{Type: llm.EventToolCallStart, Index: 0, ToolCallID: id, ToolName: name},
		{Type: llm.EventToolCallEnd, Index: 0, Block: llm.ToolCallBlock{
			ID: id, Name: name, Input: json.RawMessage(args)}},
		{Type: llm.EventDone, StopReason: llm.StopToolUse},
	}
}

// usageTextTurn scripts a closing turn that reports usage and answers.
func usageTextTurn(in, out int, text string) []llm.Event {
	return []llm.Event{
		{Type: llm.EventMessageStart},
		{Type: llm.EventUsage, Usage: llm.Usage{Input: in, Output: out}},
		{Type: llm.EventTextStart, Index: 0},
		{Type: llm.EventTextDelta, Index: 0, Text: text},
		{Type: llm.EventTextEnd, Index: 0, Block: llm.TextBlock{Text: text}},
		{Type: llm.EventDone, StopReason: llm.StopEndTurn},
	}
}

// TestStatsBenchmarkDataPath walks the whole path the benchmark depends on: the
// json stream must carry per-call tool events with names and error flags, and a
// summary line carrying real token totals. The benchmark derives tool counts
// from the events and tokens from the summary, so both halves are asserted here
// rather than trusted.
func TestStatsBenchmarkDataPath(t *testing.T) {
	find := func(lines []map[string]any, typ string) []map[string]any {
		var out []map[string]any
		for _, l := range lines {
			if l["type"] == typ {
				out = append(out, l)
			}
		}
		return out
	}

	t.Run("successful_edit_is_counted_and_tokens_recorded", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "a.txt")
		require.NoError(t, os.WriteFile(path, []byte("hello world\n"), 0o644))
		args := fmt.Sprintf(`{"path":%q,"edits":[{"oldText":"world","newText":"ajent"}]}`, path)

		_, out, _ := headlessHarness(t,
			cliFlags{prompt: "go", output: outputJSON, allowAll: true, stats: true}, "",
			[]llm.ScriptedTurn{
				{Events: usageCallTurn(1200, 40, "c1", "edit", args)},
				{Events: usageTextTurn(1500, 25, "done")},
			})
		lines := decodeLines(t, out)

		calls := find(lines, "tool_call")
		require.Len(t, calls, 1)
		assert.Equal(t, "edit", calls[0]["name"]) // the benchmark keys edits off this

		results := find(lines, "tool_result")
		require.Len(t, results, 1)
		assert.Equal(t, "edit", results[0]["name"])
		assert.Nil(t, results[0]["error"]) // omitempty: absent means success

		summaries := find(lines, "summary")
		require.Len(t, summaries, 1)
		usage, ok := summaries[0]["usage"].(map[string]any)
		require.True(t, ok, "summary must carry a usage object")
		assert.Positive(t, usage["input"], "tok_in must be non-zero or the arm is unusable")
		assert.Positive(t, usage["output"])

		byTool, ok := summaries[0]["calls"].(map[string]any)
		require.True(t, ok)
		assert.EqualValues(t, 1, byTool["edit"])
		assert.Positive(t, summaries[0]["turns"])

		// the summary must precede the result line, which stays last
		assert.Equal(t, "result", lines[len(lines)-1]["type"])
		assert.Equal(t, "summary", lines[len(lines)-2]["type"])

		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "hello ajent\n", string(data)) // the edit really applied
	})

	t.Run("failed_edit_is_marked_and_classifiable", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "a.txt")
		require.NoError(t, os.WriteFile(path, []byte("hello world\n"), 0o644))
		args := fmt.Sprintf(`{"path":%q,"edits":[{"oldText":"absent text","newText":"x"}]}`, path)

		_, out, _ := headlessHarness(t,
			cliFlags{prompt: "go", output: outputJSON, allowAll: true, stats: true}, "",
			[]llm.ScriptedTurn{
				{Events: usageCallTurn(900, 30, "c1", "edit", args)},
				{Events: usageTextTurn(1000, 20, "gave up")},
			})
		lines := decodeLines(t, out)

		results := find(lines, "tool_result")
		require.Len(t, results, 1)
		assert.Equal(t, "edit", results[0]["name"])
		assert.Equal(t, true, results[0]["error"]) // drives edit_fail

		// the benchmark classifies the failure by substring; keep that parseable
		output, _ := results[0]["output"].(string)
		assert.Contains(t, output, "no match for edit")

		summaries := find(lines, "summary")
		require.Len(t, summaries, 1)
		failed, ok := summaries[0]["failed"].(map[string]any)
		require.True(t, ok)
		assert.EqualValues(t, 1, failed["edit"])
	})
}
