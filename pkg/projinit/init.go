// Package projinit surveys a repository and assembles the prompt that writes its
// AGENTS.md. The survey runs the real read and agent_* tools, so its findings
// reach the model as ordinary tool calls rather than a pasted report.
package projinit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tools"
)

// Survey's terminal errors. The fan-out is the whole value of a run, so a
// missing tool or a failed spawn reports rather than quietly drafting from less.
var (
	ErrNoSubAgents = errors.New("sub-agents are unavailable")
	ErrNoRead      = errors.New("the read tool is unavailable")
	ErrNoneStarted = errors.New("no sub-agent could be started")
)

// Options configures a Runner. Registry supplies the real read and agent_* tools,
// Sink renders their calls into history as the loop would.
type Options struct {
	Cwd      string
	Registry *tools.Registry
	Sink     agent.Sink
	Notify   func(msg string, level agent.Level)
	// Started reports each spawned investigation id, so a caller cancelling a
	// survey stops its own children and not the model's.
	Started func(id string)
}

// Runner performs project surveys.
type Runner struct {
	opts Options
	runs atomic.Int64 // call ids carry it: a second survey must not reuse the first's
}

// New returns a Runner over opts.
func New(opts Options) *Runner { return &Runner{opts: opts} }

// Survey reads the project's README files, fans the build and the codebase out to
// read-only sub-agents, and returns the distillation prompt carrying every call
// and result the survey produced. It spends no model tokens.
func (r *Runner) Survey(ctx context.Context) (agent.Input, error) {
	if r.opts.Registry == nil {
		return agent.Input{}, ErrNoSubAgents
	}
	read, rok := r.opts.Registry.Lookup("read")
	if !rok { // without it the survey cannot see the project at all
		return agent.Input{}, ErrNoRead
	}
	start, sok := r.opts.Registry.Lookup("agent_start")
	poll, pok := r.opts.Registry.Lookup("agent_poll")
	if !sok || !pok {
		return agent.Input{}, ErrNoSubAgents
	}
	run := r.runs.Add(1)

	before, existing := r.readDocs(ctx, read, run)
	if err := ctx.Err(); err != nil {
		return agent.Input{}, err
	}

	tasks := surveyTasks(r.opts.Cwd)
	started, ids, failed := r.startAll(ctx, start, tasks, run)
	before = append(before, started...)
	if len(ids) == 0 {
		return agent.Input{}, startError(failed)
	}
	r.notify("init: surveying the project with "+strconv.Itoa(len(ids))+" sub-agents", agent.LevelInfo)

	before = append(before, r.pollAll(ctx, poll, ids, run)...)
	if err := ctx.Err(); err != nil {
		return agent.Input{}, err
	}

	text := distillNew
	if existing {
		text = distillUpdate
	}
	return agent.Input{Text: text, Before: before, Injected: true}, nil
}

// startError distinguishes spawns that were refused from tools that were never
// there, so the notice names what actually went wrong.
func startError(failed []string) error {
	if len(failed) == 0 {
		return ErrNoSubAgents
	}
	return fmt.Errorf("%w: %s", ErrNoneStarted, strings.Join(failed, "; "))
}

// readDocs runs stage 1: a real read per README and any existing AGENTS.md,
// reporting whether that file was found.
func (r *Runner) readDocs(ctx context.Context, tool agent.Tool, run int64) ([]llm.Message, bool) {
	paths, existing := docFiles(r.opts.Cwd)
	out := make([]llm.Message, 0, 2*len(paths))
	for i, p := range paths {
		input, _ := json.Marshal(map[string]any{"path": p})
		call := agent.ToolCall{ID: callID(run, "read", strconv.Itoa(i+1)), Name: "read", Input: input}
		msgs, _ := agent.InjectPair(ctx, tool, r.opts.Sink, call, "read "+p)
		out = append(out, msgs...)
	}
	return out, existing
}

// startAll runs stage 2's spawns, returning their pairs, the ids to poll and a
// reason per spawn that was refused.
func (r *Runner) startAll(ctx context.Context, tool agent.Tool, tasks []string, run int64) (msgs []llm.Message, ids, failed []string) {
	for i, task := range tasks {
		if ctx.Err() != nil {
			break
		}
		input, _ := json.Marshal(map[string]any{"task": task})
		call := agent.ToolCall{ID: callID(run, "start", strconv.Itoa(i+1)), Name: "agent_start", Input: input}
		pair, res := agent.InjectPair(ctx, tool, r.opts.Sink, call, tool.Label(call))
		msgs = append(msgs, pair...)

		id := detail(res, "id")
		switch {
		case id != "":
			ids = append(ids, id)
			if r.opts.Started != nil {
				r.opts.Started(id)
			}
		default: // a refused spawn (denied, bad child model) is not a missing tool
			failed = append(failed, resultText(res))
		}
	}
	return msgs, ids, failed
}

// pollAll waits for every investigation at once, so a poller is registered before
// any child can finish and its completion never notifies or steers the parent.
// Results are collected in call order regardless of completion order.
func (r *Runner) pollAll(ctx context.Context, tool agent.Tool, ids []string, run int64) []llm.Message {
	pairs := make([][]llm.Message, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pairs[i] = r.pollOne(ctx, tool, id, run)
		}()
	}
	wg.Wait()

	out := make([]llm.Message, 0, 2*len(ids)) // one call and one result per investigation
	for _, p := range pairs {
		out = append(out, p...)
	}
	return out
}

// pollOne waits for one investigation, re-polling past a timeout so only the
// terminal call and result reach context.
func (r *Runner) pollOne(ctx context.Context, tool agent.Tool, id string, run int64) []llm.Message {
	input, _ := json.Marshal(map[string]any{"id": id})
	for attempt := 1; ctx.Err() == nil; attempt++ {
		call := agent.ToolCall{
			ID:    callID(run, "poll", id+"-"+strconv.Itoa(attempt)),
			Name:  "agent_poll",
			Input: input,
		}
		msgs, res := agent.InjectPair(ctx, tool, r.opts.Sink, call, tool.Label(call))
		if terminal(res) {
			return msgs
		}
	}
	return nil
}

// notify reports progress when the host supplied a sink for it.
func (r *Runner) notify(msg string, level agent.Level) {
	if r.opts.Notify != nil {
		r.opts.Notify(msg, level)
	}
}

// terminal reports whether a poll result names a finished job. An unrecognised or
// missing status counts as terminal, so an unexpected value cannot spin the loop.
func terminal(res agent.ToolResult) bool {
	switch detail(res, "status") {
	case "queued", "running":
		return false
	default:
		return true
	}
}

// callID names one survey call. The run number is what keeps a second /init in the
// same session from reusing the first's ids: Input.Before stays in State, and a
// repeated tool_use id makes every later Anthropic request fail permanently.
func callID(run int64, stage, suffix string) string {
	return fmt.Sprintf("init-%d-%s-%s", run, stage, suffix)
}

// resultText joins a tool result's text blocks, for reporting a refusal.
func resultText(res agent.ToolResult) string {
	var b strings.Builder
	for _, blk := range res.Content {
		if tb, ok := blk.(llm.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	if b.Len() == 0 {
		return "no id returned"
	}
	return b.String()
}

// detail reads one key from a tool result's Details map.
func detail(res agent.ToolResult, key string) string {
	d, ok := res.Details.(map[string]string)
	if !ok {
		return ""
	}
	return d[key]
}
