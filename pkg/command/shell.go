package command

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

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
	root context.Context // derived per-run contexts, cancelled on shutdown

	timeout time.Duration // per-run ceiling, zero runs uncapped

	mu       sync.Mutex
	runs     []*stageRun // submission order
	nextID   int
	onChange func(est int) // reports the staged token estimate as it moves, nil before wiring
}

// stageRun is one entry in the staging area: an executing `!` command. A
// finished entry becomes one user message in Flush. An excluded run's output
// goes nowhere.
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
// streams output to sink like an agent-initiated bash call. Every staged run is
// derived from root, so a cancelled root stops in-flight `!` commands.
func NewStager(root context.Context, reg *tools.Registry, sink agent.Sink) *Stager {
	return &Stager{root: root, reg: reg, sink: sink}
}

// SetTimeout caps how long each staged run may take, zero (the default) runs
// uncapped so a long human command is never force-killed by the model-facing
// default.
func (s *Stager) SetTimeout(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.timeout = d
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
			msgs = append(msgs, r.message().Message)
		}
	}
	s.mu.Unlock()
	if fn != nil {
		fn(tokens.EstimateMessages(msgs))
	}
}

// Run starts cmd executing immediately. The bash tool resolves through Lookup, so
// disabling it for the agent never blocks the human's own shell. A registry
// without bash at all is a notice rather than a run. An empty command produces a
// notice and runs nothing. Run never blocks on the command itself. An excluded
// run (`!!`) still displays but its output never reaches context or the
// transcript.
func (s *Stager) Run(cmd string, excluded bool) {
	if strings.TrimSpace(cmd) == "" {
		s.sink.Notice("empty shell command", agent.LevelWarn)
		return
	}
	tool, ok := s.reg.Lookup(tools.ToolBash)
	if !ok {
		s.sink.Notice("shell mode needs the bash tool registered", agent.LevelWarn)
		return
	}
	s.mu.Lock()
	s.nextID++
	id := fmt.Sprintf("shell-%d", s.nextID)
	timeout := s.timeout
	// a `!` line is the human's own shell, mark it so the permission gate exempts it.
	// Derived from the stager root (appRoot) so shutdown cancels in-flight runs.
	runCtx, cancel := context.WithCancel(tools.WithUserInitiated(s.root))
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

	input := stageInput{Command: cmd}
	if timeout > 0 { // seconds, rounded up
		input.Timeout = int((timeout + time.Second - 1) / time.Second)
	}
	encoded, _ := json.Marshal(input)
	call := agent.ToolCall{ID: id, Name: tools.ToolBash, Input: encoded}
	out := agent.NewOutput(s.sink, id)
	// full display keeps `!`/`!!` output whole instead of collapsed to its head
	done := s.sink.ToolStart(call, run.label, true)

	go func() {
		// the inner scope closes run.done before the report, so reportStaged already
		// sees this run as finished and counts its output
		func() {
			defer close(run.done)
			res, err, fault := agent.RecoverTool(runCtx, done, s.sink, call,
				func() (agent.ToolResult, error) { return tool.Execute(runCtx, call, out) })
			if res.Content == nil {
				res.Content = llm.BlockList{}
			}
			// a fault result is already IsError with its panic text, and RecoverTool
			// has delivered it through done, so only the staged copy is left to fill
			if err != nil && !res.IsError {
				res.IsError = true
				if len(res.Content) == 0 {
					res.Content = llm.BlockList{llm.TextBlock{Text: err.Error()}}
				}
			}
			// a fault still stages, so Flush never emits "(no output)"
			run.result = res
			// release the root-child registration now the result is captured; every
			// completion path lands here, and Discard/Cancel stay idempotent with it
			run.cancel()
			if !fault {
				done(res)
			}
		}()
		s.reportStaged() // the output is now context the next prompt will carry
	}()
}

// stageInput is the JSON the stager hands the bash tool, mirroring the
// model-facing schema while letting a user run name its own timeout.
type stageInput struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
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
// message: finished ones drop outright and still-running ones stay in s.runs so
// Pending/Cancel keep tracking them. Their output goes nowhere, so Flush must not
// hold the next prompt hostage waiting for them.
func (s *Stager) Flush(ctx context.Context) []agent.MessageInfo {
	s.WaitReady(ctx)
	return s.TakeReady()
}

// WaitReady blocks until every included staged command finished or ctx ends,
// consuming nothing. Excluded runs never wait: their output goes nowhere.
func (s *Stager) WaitReady(ctx context.Context) {
	s.mu.Lock()
	included := bulk.SliceFilter(func(r *stageRun) bool { return !r.excluded }, s.runs)
	s.mu.Unlock()

	for _, r := range included {
		select {
		case <-r.done:
		case <-ctx.Done():
			return
		}
	}
}

// TakeReady consumes every included staged command that already finished and
// returns its message. Still-running ones stay staged for a later delivery.
// Non-blocking, so a caller may hold another lock across it.
func (s *Stager) TakeReady() []agent.MessageInfo {
	s.mu.Lock()
	// the split rewrites the input backing array, so s.runs becomes the running
	// tail and done is a fresh slice, never aliased
	running, done := bulk.SliceSplitInPlace(func(r *stageRun) bool { return !isDone(r.done) }, s.runs)
	s.runs = running
	done = bulk.SliceFilterInPlace(func(r *stageRun) bool { return !r.excluded }, done)
	s.mu.Unlock()

	out := make([]agent.MessageInfo, 0, len(done))
	for _, r := range done {
		out = append(out, r.message())
	}
	s.reportStaged() // the consumed results are the delivery's to account for now
	return out
}

// message renders what Flush emits and the estimate counts: an executed run
// builds its user message from cmd plus output.
func (r *stageRun) message() agent.MessageInfo {
	return shellUserMessage(r.cmd, r.result)
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
