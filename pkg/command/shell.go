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

// stageRun is one entry in the staging area: an executing `!` command or a
// re-staged flushed result. A finished entry becomes one user message in Flush.
// An excluded run's output goes nowhere.
type stageRun struct {
	id       string
	cmd      string
	label    string
	cancel   context.CancelFunc
	excluded bool

	// restaged carries a pre-rendered message handed back by Restage: the entry
	// never executed, so cmd/label/result stay unused and it is born finished.
	restaged *agent.MessageInfo

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
	done := s.startTool(call, run.label)

	go func() {
		// the inner scope closes run.done before the report, so reportStaged already
		// sees this run as finished and counts its output
		func() {
			defer close(run.done)
			var res agent.ToolResult
			var err error
			var fault bool
			func() {
				defer func() {
					if r := recover(); r != nil {
						fault = true
						err = fmt.Errorf("tool panicked: %v", r)
					}
				}()
				res, err = tool.Execute(runCtx, call, out)
			}()
			if fault {
				return
			}
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

// stageInput is the JSON the stager hands the bash tool, mirroring the
// model-facing schema while letting a user run name its own timeout.
type stageInput struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// startTool opens the run's header and stream on sink with full display, so
// `!`/`!!` output is never collapsed to its head.
func (s *Stager) startTool(call agent.ToolCall, label string) func(agent.ToolResult) {
	return s.sink.ToolStart(call, label, true)
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
	s.mu.Lock()
	included := bulk.SliceFilter(func(r *stageRun) bool { return !r.excluded }, s.runs)
	s.mu.Unlock()

	var out []agent.MessageInfo
	for i := range included {
		r := included[i]
		select {
		case <-r.done:
		case <-ctx.Done():
			// unfinished runs (this one and the tail) stay tracked in s.runs, so a
			// later flush picks them up instead of dropping staged results.
			s.reportStaged()
			return out
		}
		out = append(out, r.message())
		s.removeRun(r)
	}

	// included runs are all consumed, dropping finished excluded ones outright and
	// keeping still-running ones so Pending/Cancel keep tracking them.
	s.mu.Lock()
	s.runs = bulk.SliceFilterInPlace(func(r *stageRun) bool { return !r.excluded || !isDone(r.done) }, s.runs)
	s.mu.Unlock()

	s.reportStaged() // the flushed results are the submission's to account for now
	return out
}

// message renders what Flush emits and the estimate counts: a restaged entry
// carries its own rendered result, an executed one builds from cmd plus output.
func (r *stageRun) message() agent.MessageInfo {
	if r.restaged != nil {
		return *r.restaged
	}
	return shellUserMessage(r.cmd, r.result)
}

// Restage returns flushed results to the staging area so they ride the next
// prompt: a queued input aborted or recalled after its flush still owns those
// Before messages, and dropping them would silently lose context the user saw.
// Entries are born finished in submission order (anything staged later is
// newer), count on the bar again through reportStaged, and Discard drops them
// like any staged result.
func (s *Stager) Restage(msgs []agent.MessageInfo) {
	if len(msgs) == 0 {
		return
	}
	s.mu.Lock()
	for _, mi := range msgs {
		run := &stageRun{
			cancel:   func() {}, // Discard cancels every run; a restaged one has nothing to cancel
			restaged: &mi,
			done:     make(chan struct{}),
		}
		close(run.done)
		s.runs = append(s.runs, run)
	}
	s.mu.Unlock()
	s.reportStaged()
}

// removeRun drops a consumed run from tracking so Pending/Cancel stop seeing it and
// a later flush does not re-emit its message.
func (s *Stager) removeRun(r *stageRun) {
	s.mu.Lock()
	s.runs = bulk.SliceFilterInPlace(func(x *stageRun) bool { return x != r }, s.runs)
	s.mu.Unlock()
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
