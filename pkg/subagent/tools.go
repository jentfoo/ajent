package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
)

// sharedToolHint is appended to every agent_* description so a model learns the
// contract up front instead of by trial.
const sharedToolHint = "A sub-agent has no session context: pass file paths and key facts, not content (it can read files itself). It is read-only (read, grep, find, ls plus any MCP tool marked read-only); anything needing write, edit or shell must be done directly. Its final message is the entire return value."

// startTool spawns a background investigation.
type startTool struct{ m *Manager }

func (t *startTool) Name() string { return "agent_start" }

// label shows which agent was spawned so the header identifies it, not just that
// one was started. Falls back to the generic form when args do not parse.
func (t *startTool) Label(call agent.ToolCall) string {
	var p startParams
	switch {
	case json.Unmarshal(call.Input, &p) != nil:
	default:
		if task := strings.TrimSpace(p.Task); task != "" {
			return "sub-agent: start " + shortLabel(task)
		}
	}
	return "sub-agent: start"
}
func (t *startTool) Description() string {
	return "Spawn an isolated, read-only sub-agent to investigate a focused question. Returns a job id immediately; the work runs in the background. Call agent_poll with that id later to retrieve the final summary. Fan out several in one batch and keep working while they run. " + sharedToolHint
}
func (t *startTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{Parameters: paramsSchema("task", map[string]any{
		"task":         strProp("the focused question to investigate; give file paths and key facts, not content"),
		"instructions": strProp("optional extra guidance for the sub-agent (constraints, style, focus areas)"),
	})}
}
func (t *startTool) Mode() agent.ExecutionMode { return agent.ModeParallel }

// startParams is the model-facing arguments shape.
type startParams struct {
	Task         string `json:"task"`
	Instructions string `json:"instructions,omitempty"`
}

func (t *startTool) Execute(ctx context.Context, call agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	var p startParams
	if err := json.Unmarshal(call.Input, &p); err != nil {
		return resultErr("bad args: " + err.Error()), nil
	}
	p.Task = strings.TrimSpace(p.Task)
	if p.Task == "" {
		return resultErr("agent_start requires a task"), nil
	}
	id := t.m.Start(p.Task, p.Instructions)
	return agent.ToolResult{
		Content: llm.BlockList{llm.TextBlock{Text: "Sub-agent " + id + " started. Call agent_poll with id " + id + " to retrieve the summary."}},
		Details: map[string]string{"id": id},
	}, nil
}

// pollTool blocks until a sub-agent finishes or times out.
type pollTool struct{ m *Manager }

func (t *pollTool) Name() string { return "agent_poll" }

// label names the agent being waited on so parallel polls are distinguishable.
func (t *pollTool) Label(call agent.ToolCall) string {
	var p pollParams
	if json.Unmarshal(call.Input, &p) == nil {
		if id := strings.TrimSpace(p.ID); id != "" {
			return "sub-agent: poll " + normalizeID(id)
		}
	}
	return "sub-agent: poll"
}
func (t *pollTool) Description() string {
	return "Wait for a sub-agent to complete and return its summary. When it has finished, returns the final summary as the only text content; on an error or abort that is reported instead. On timeout reports still-running plus elapsed time and the child's context usage against its model window, so you can judge whether to keep waiting. Accepts id like sub-2 or bare 2."
}
func (t *pollTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{Parameters: paramsSchema("id", map[string]any{
		"id": strProp("the sub-agent id, e.g. sub-2 (or bare 2)"),
	})}
}
func (t *pollTool) Mode() agent.ExecutionMode { return agent.ModeParallel }

// pollParams is the model-facing arguments shape.
type pollParams struct {
	ID string `json:"id"`
}

func (t *pollTool) Execute(ctx context.Context, call agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	var p pollParams
	if err := json.Unmarshal(call.Input, &p); err != nil {
		return resultErr("bad args: " + err.Error()), nil
	}
	j, ok := t.m.lookup(p.ID)
	if !ok {
		return resultErr("unknown sub-agent id " + strings.TrimSpace(p.ID)), nil
	}
	snap, complete := t.m.Poll(ctx, j.id)
	if ctx.Err() != nil { // the turn was interrupted; release promptly and let abort fill this call
		return agent.ToolResult{Content: llm.BlockList{llm.TextBlock{Text: "poll interrupted"}}}, nil
	}
	switch {
	case !complete:
		return result(j.pollProgress()), nil
	case snap.Status == StatusAborted:
		return result("sub-agent " + j.id + " aborted"), nil
	case snap.Status == StatusError && snap.Err != nil:
		return resultErr(snap.Err.Error()), nil
	default: // done with a summary, or an empty one fell back to the placeholder
		out := strings.TrimSpace(snap.Summary)
		if out == "" {
			out = placeholder
		}
		return result(out), nil
	}
}

// listTool lists every job and its status.
type listTool struct{ m *Manager }

func (t *listTool) Name() string { return "agent_list" }
func (t *listTool) Label(agent.ToolCall) string {
	return "sub-agent: list"
}
func (t *listTool) Description() string {
	return "List every sub-agent with its id, status and elapsed time. Returns one row per job as tab-separated columns, or '(no sub-agents)' when none exist."
}
func (t *listTool) Schema() llm.ToolSchema { return llm.ToolSchema{Parameters: paramsSchema("", nil)} }
func (t *listTool) Mode() agent.ExecutionMode {
	return agent.ModeParallel
}

func (t *listTool) Execute(ctx context.Context, _ agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	jobs := t.m.List()
	if len(jobs) == 0 {
		return result("(no sub-agents)"), nil
	}
	var b strings.Builder
	b.WriteString("id\tstatus\telapsed\n")
	for _, j := range jobs {
		// a finished job's elapsed freezes at its end time; only live ones keep counting
		elapsed := time.Since(j.Started)
		if !j.Ended.IsZero() {
			elapsed = j.Ended.Sub(j.Started)
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\n", j.ID, j.Status, strutil.Elapsed(elapsed))
	}
	return result(b.String()), nil
}

// paramsSchema builds an object JSON schema with the given string properties. A
// parameterless tool still needs a valid empty properties object: serializing nil
// as null makes strict providers (e.g. lmstudio) reject the whole request.
func paramsSchema(required string, props map[string]any) json.RawMessage {
	if len(props) == 0 { // never emit "properties": null for a no-arg tool
		props = map[string]any{}
	}
	m := map[string]any{"type": "object", "properties": props}
	if required != "" {
		m["required"] = []string{required}
	}
	b, _ := json.Marshal(m)
	return b
}

// strProp is one string property node for a hand-built tool schema.
func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// result wraps text as a successful model-visible block. Display mirrors it so
// the TUI's output head shows the same payload: a poll timeout commits its
// elapsed line and a summary gets the head-plus-collapse treatment.
func result(text string) agent.ToolResult {
	return agent.ToolResult{Content: llm.BlockList{llm.TextBlock{Text: text}}, Display: text}
}

// resultErr builds an error ToolResult carrying the message the model should see.
func resultErr(msg string) agent.ToolResult {
	r := result(msg)
	r.IsError = true
	return r
}
