// Package projinit surveys a repository and assembles the prompt that writes its
// AGENTS.md. The survey runs the real read and agent_* tools, so its findings
// reach the model as ordinary tool calls rather than a pasted report.
package projinit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tools"
)

// Survey's terminal errors. The fan-out is the whole value of a run, so a
// missing tool, a failed spawn or an unreadable file reports rather than
// quietly drafting from less.
var (
	ErrNoSubAgents = errors.New("sub-agents are unavailable")
	// ErrSubAgentsDisabled reports a registered sub-agent tool the user turned off:
	// /init spends child tokens implicitly, so an explicit opt-out is honored.
	ErrSubAgentsDisabled = errors.New("sub-agents are disabled (/tools can re-enable them)")
	ErrNoRead            = errors.New("the read tool is unavailable")
	ErrNoneStarted       = errors.New("no sub-agent could be started")

	// ErrAgentsUnreadable reports an existing AGENTS.md whose read failed: /init
	// cannot correct what it never saw, and drafting anew would replace it.
	ErrAgentsUnreadable = errors.New("the existing AGENTS.md could not be read")
)

// Options configures a Runner. Registry supplies the real read and agent_* tools,
// Sink renders their calls into history as the loop would, Notify reports progress.
type Options struct {
	Cwd      string
	Registry *tools.Registry
	Sink     agent.Sink
	Notify   func(msg string, level agent.Level)
	// Started reports each spawned investigation id, so a caller cancelling a
	// survey stops its own children and not the model's. Calls run synchronously
	// on the survey goroutine and complete before Survey returns.
	Started func(id string)
}

// initPrefix leads every survey call id in the transcript. Package-local and
// given to the run sequencer at construction.
const initPrefix = "init-"

// Runner performs project surveys.
type Runner struct {
	opts Options
	seq  *agent.RunSequencer // numbers each survey, so a second never reuses the first's ids
}

// New returns a Runner over opts.
func New(opts Options) *Runner {
	return &Runner{opts: opts, seq: agent.NewRunSequencer(initPrefix)}
}

// Seed raises the run counter above every /init id already in msgs, so a fresh
// runner's first survey after a resume cannot mint ids the transcript holds.
func (r *Runner) Seed(msgs []llm.Message) {
	r.seq.Seed(msgs)
}

// Survey reads the project's README files, fans the build and the codebase out to
// read-only sub-agents, and returns the distillation prompt carrying every call
// and result the survey produced. It spends no model tokens.
func (r *Runner) Survey(ctx context.Context) (agent.Input, error) {
	read, rok := r.opts.Registry.Lookup("read")
	if !rok { // without it the survey cannot see the project at all
		return agent.Input{}, ErrNoRead
	}
	start, sok := r.opts.Registry.Lookup("agent_start")
	poll, pok := r.opts.Registry.Lookup("agent_poll")
	if !sok || !pok {
		return agent.Input{}, ErrNoSubAgents
	}
	// Lookup deliberately ignores enable state, but /init spends child tokens
	// implicitly: a disabled subagents group reports instead of spawning anyway
	if !r.opts.Registry.IsEnabled("agent_start") || !r.opts.Registry.IsEnabled("agent_poll") {
		return agent.Input{}, ErrSubAgentsDisabled
	}
	run := r.seq.Next()

	before, existing, err := r.readDocs(ctx, read, run)
	if err != nil {
		return agent.Input{}, err
	}

	tasks := surveyTasks(ctx, r.opts.Cwd)
	started, ids, failed := r.startAll(ctx, start, tasks, run)
	before = append(before, started...)
	if len(ids) == 0 {
		// distinguish spawns that were refused from tools that were never there
		if len(failed) == 0 {
			return agent.Input{}, ErrNoSubAgents
		}
		return agent.Input{}, fmt.Errorf("%w: %s", ErrNoneStarted, strings.Join(failed, "; "))
	}
	if err := ctx.Err(); err != nil { // cancelled mid-spawn, so the counts would mislead
		return agent.Input{}, err
	}
	if len(failed) > 0 { // each refusal already reached the model above, so this one is for the user
		r.notify(fmt.Sprintf("init: %d of %d sub-agent spawns refused: %s",
			len(failed), len(tasks), strings.Join(failed, "; ")), agent.LevelWarn)
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
	// the survey's tool pairs are machinery, not conversation: they carry no
	// replay mark, so a resume shows the distilled result rather than the survey
	infos := make([]agent.MessageInfo, 0, len(before))
	for _, m := range before {
		infos = append(infos, agent.MessageInfo{Message: m})
	}
	return agent.Input{Text: text, Before: infos, Injected: true}, nil
}

// readDocs does one real read per README and any existing AGENTS.md, reporting
// whether that file was found. A directory listing or an AGENTS.md read that
// fails stops the survey: /init must not draft from less than it can see.
func (r *Runner) readDocs(ctx context.Context, tool agent.Tool, run int64) ([]llm.Message, bool, error) {
	paths, existing, err := docFiles(r.opts.Cwd)
	if err != nil {
		return nil, false, err
	}
	out := make([]llm.Message, 0, 2*len(paths))
	var agentsRes agent.ToolResult
	for i, p := range paths {
		input, _ := json.Marshal(map[string]any{"path": p})
		call := agent.ToolCall{ID: r.seq.CallID(run, "read-"+strconv.Itoa(i+1)), Name: "read", Input: input}
		msgs, res := agent.InjectPair(ctx, tool, r.opts.Sink, call, "read "+p)
		out = append(out, msgs...)
		if p == agentsFile {
			agentsRes = res
		}
	}
	// a cancelled read is not an unreadable file, so cancellation wins the gate
	if err := ctx.Err(); err != nil {
		return out, existing, err
	}
	if existing && agentsRes.IsError {
		if reason := llm.JoinText(agentsRes.Content); reason != "" {
			return out, existing, fmt.Errorf("%w: %s", ErrAgentsUnreadable, reason)
		}
		return out, existing, ErrAgentsUnreadable
	}
	return out, existing, nil
}

// startAll runs the survey spawns, returning their pairs, the ids to poll and
// one named entry per refused spawn, its label carrying the task and its reason.
func (r *Runner) startAll(ctx context.Context, tool agent.Tool, tasks []surveyTask, run int64) (msgs []llm.Message, ids, failed []string) {
	for i, task := range tasks {
		if ctx.Err() != nil {
			break
		}
		input, _ := json.Marshal(map[string]any{"task": task.text})
		call := agent.ToolCall{ID: r.seq.CallID(run, "start-"+strconv.Itoa(i+1)), Name: "agent_start", Input: input}
		pair, res := agent.InjectPair(ctx, tool, r.opts.Sink, call, tool.Label(call))
		msgs = append(msgs, pair...)

		id := detail(res, "id")
		switch {
		case id != "":
			ids = append(ids, id)
			r.opts.Started(id)
		default: // a refused spawn (denied, bad child model) is not a missing tool
			refused := llm.JoinText(res.Content)
			if refused == "" {
				refused = "no id returned"
			}
			failed = append(failed, task.label+" ("+refused+")")
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

	return slices.Concat(pairs...) // one call and one result per investigation
}

// pollOne waits for one investigation, re-polling past a timeout so only the
// terminal call and result are returned.
func (r *Runner) pollOne(ctx context.Context, tool agent.Tool, id string, run int64) []llm.Message {
	input, _ := json.Marshal(map[string]any{"id": id})
	for attempt := 1; ctx.Err() == nil; attempt++ {
		call := agent.ToolCall{
			ID:    r.seq.CallID(run, "poll-"+id+"-"+strconv.Itoa(attempt)),
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

// notify reports survey progress to the host.
func (r *Runner) notify(msg string, level agent.Level) {
	r.opts.Notify(msg, level)
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

// detail reads one key from a tool result's Details map, empty when absent or
// when Details holds some other type.
func detail(res agent.ToolResult, key string) string {
	d, ok := res.Details.(map[string]string)
	if !ok {
		return ""
	}
	return d[key]
}
