package command

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
)

// Stager runs `!` shell commands directly through the real bash tool path and
// stages their output ahead of the next user message as a single user-authored
// text ("User Ran: <cmd> / Output:"), so it reads as the human's own action.
type Stager struct {
	reg  *tools.Registry
	sink agent.Sink

	mu       sync.Mutex
	runs     []*stageRun // submission order
	nextID   int
	onChange func(est int) // reports the staged token estimate as it moves; nil until wired
}

// stageRun is one staged shell command. Its finished result becomes one user
// message in Flush; an excluded run's output goes nowhere.
type stageRun struct {
	id       string
	cmd      string
	label    string
	cancel   context.CancelFunc
	excluded bool

	// result written by the goroutine once the tool completes
	done   chan struct{}
	result agent.ToolResult
}

// NewStager returns a stager that runs commands through reg's bash tool and
// streams output to sink like an agent-initiated bash call.
func NewStager(reg *tools.Registry, sink agent.Sink) *Stager {
	return &Stager{reg: reg, sink: sink}
}

// SetOnChange registers the hook told how many tokens the staged results now
// carry, so the context bar counts `!` output while it waits for a prompt. It
// fires as each run completes and again whenever the staged set is emptied.
func (s *Stager) SetOnChange(fn func(est int)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onChange = fn
}

// reportStaged recomputes the staged estimate and hands it to the change hook.
func (s *Stager) reportStaged() {
	s.mu.Lock()
	fn := s.onChange
	var msgs []llm.Message
	for _, r := range s.runs {
		// an excluded run never reaches context, and an unfinished one has no result yet
		if !r.excluded && isDone(r.done) {
			msgs = append(msgs, shellUserMessage(r.cmd, r.result).Message)
		}
	}
	s.mu.Unlock()
	if fn != nil {
		fn(tokens.EstimateMessages(msgs))
	}
}

// Run starts cmd executing immediately. A disabled bash tool is a refusal
// notice rather than a run, matching the /tools widening rule; an empty command
// produces a notice and runs nothing. Run never blocks on the command itself.
// An excluded run (`!!`) still displays but its output never reaches context or
// the transcript.
func (s *Stager) Run(cmd string, excluded bool) {
	if strings.TrimSpace(cmd) == "" {
		s.sink.Notice("empty shell command", agent.LevelWarn)
		return
	}
	tool, ok := s.reg.Get(toolBash)
	if !ok {
		s.sink.Notice("shell mode needs the bash tool enabled; use /tools", agent.LevelWarn)
		return
	}
	s.mu.Lock()
	s.nextID++
	id := fmt.Sprintf("shell-%d", s.nextID)
	// a `!` line is the human's own shell; mark it so the permission gate exempts it.
	runCtx, cancel := context.WithCancel(tools.WithUserInitiated(context.Background()))
	label := "! " + strutil.FirstLine(cmd)
	if excluded {
		label = "!! " + strutil.FirstLine(cmd)
	}
	run := &stageRun{
		id:       id,
		cmd:      cmd,
		label:    label,
		cancel:   cancel,
		excluded: excluded,
		done:     make(chan struct{}),
	}
	s.runs = append(s.runs, run)
	s.mu.Unlock()

	input, _ := json.Marshal(map[string]any{"command": cmd})
	call := agent.ToolCall{ID: id, Name: toolBash, Input: input}
	out := agent.NewOutput(s.sink, id)
	done := s.startTool(call, run.label)

	go func() {
		// the inner scope closes run.done before the report, so reportStaged already
		// sees this run as finished and counts its output
		func() {
			defer close(run.done)
			res, err := tool.Execute(runCtx, call, out)
			if res.Content == nil {
				res.Content = llm.BlockList{}
			}
			if err != nil && !res.IsError {
				res.IsError = true
				if len(res.Content) == 0 {
					res.Content = llm.BlockList{llm.TextBlock{Text: err.Error()}}
				}
			}
			run.result = res
			done(res)
		}()
		s.reportStaged() // the output is now context the next prompt will carry
	}()
}

// startTool opens the run's header and stream on sink, preferring a full-display
// variant when the sink offers one so `!`/`!!` output is never collapsed.
func (s *Stager) startTool(call agent.ToolCall, label string) func(agent.ToolResult) {
	if fs, ok := s.sink.(fullToolStarter); ok {
		return fs.ToolStartFull(call, label)
	}
	return s.sink.ToolStart(call, label)
}

// fullToolStarter is implemented by sinks that can mark a staged shell command's
// streamed output for full (untruncated) display. The base agent.Sink collapses
// tool history to its head; user-initiated `!`/`!!` shells show everything.
type fullToolStarter interface {
	ToolStartFull(call agent.ToolCall, label string) func(agent.ToolResult)
}

// Discard cancels every still-running staged command and drops all staged
// results. A rewind or fork rebuilds context from a different branch point, so a
// command staged against the abandoned one must not ride the next prompt.
func (s *Stager) Discard() {
	s.mu.Lock()
	for _, r := range s.runs {
		r.cancel()
	}
	s.runs = nil
	s.mu.Unlock()
	s.reportStaged()
}

// Pending reports whether any staged command is still running.
func (s *Stager) Pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.ContainsFunc(s.runs, func(r *stageRun) bool { return !isDone(r.done) })
}

// Cancel interrupts every still-running staged command. A cancelled command
// still stages its partial output plus an interrupted marker, so the model's
// view matches what the user saw.
func (s *Stager) Cancel() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		if !isDone(r.done) {
			r.cancel()
		}
	}
}

// Flush returns one user message per included staged command, staging each
// completed run's output ahead of the next prompt. Excluded runs never produce a
// message: finished ones are dropped outright and still-running ones stay in
// s.runs so Pending/Cancel keep tracking them — their output goes nowhere, so
// Flush must not hold the next prompt hostage waiting for them.
func (s *Stager) Flush(ctx context.Context) []agent.MessageInfo {
	s.mu.Lock()
	// included runs are taken out for flushing; excluded finished ones drop, and
	// still-running excluded stay so Pending/Cancel keep tracking them.
	included := bulk.SliceFilterInto(nil, func(r *stageRun) bool { return !r.excluded }, s.runs)
	s.runs = bulk.SliceFilterInPlace(func(r *stageRun) bool { return r.excluded && !isDone(r.done) }, s.runs)
	s.mu.Unlock()

	var out []agent.MessageInfo
	for i := range included {
		r := included[i]
		select {
		case <-r.done:
		case <-ctx.Done():
			// requeue every unfinished included run (this one and the tail) so a
			// later flush picks them up instead of dropping staged results.
			s.mu.Lock()
			s.runs = append(s.runs, included[i:]...)
			s.mu.Unlock()
			s.reportStaged()
			return out
		}
		out = append(out, shellUserMessage(r.cmd, r.result))
	}
	s.reportStaged() // the flushed results are the submission's to account for now
	return out
}

// shellUserMessage renders one completed run as the user message the model sees.
// It is marked replayed so a resume or rewind redraws it above the prompt it fed.
func shellUserMessage(cmd string, res agent.ToolResult) agent.MessageInfo {
	var sb strings.Builder
	sb.WriteString("User Ran: ")
	sb.WriteString(cmd)
	sb.WriteString("\n\nOutput:\n")
	text := resultText(res.Content)
	if text == "" {
		text = "(no output)"
	}
	sb.WriteString(text)
	return agent.MessageInfo{
		Message:  llm.Message{Role: llm.RoleUser, Content: llm.BlockList{llm.TextBlock{Text: sb.String()}}},
		Replayed: true,
	}
}

// resultText concatenates the non-empty TextBlocks in content. The bash tool
// returns a single text block holding status prefix + combined output.
func resultText(content llm.BlockList) string {
	var sb strings.Builder
	for _, b := range content {
		if tb, ok := b.(llm.TextBlock); ok && tb.Text != "" {
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}

// isDone reports whether a completion channel has closed.
func isDone(done chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

const toolBash = "bash"
