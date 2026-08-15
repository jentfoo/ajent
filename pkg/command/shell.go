package command

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/jentfoo/ajent/pkg/tools"
)

// shellProvenance rides in a ToolResultBlock.Details so the token counter can
// attribute staged results and prompt assembly can mark them injected.
type shellProvenance struct {
	Source string    `json:"source"` // "shell"
	TS     time.Time `json:"ts"`
}

// Stager runs `!` shell commands directly through the real bash tool path and
// stages their call + result pair ahead of the next user message.
type Stager struct {
	reg  *tools.Registry
	sink agent.Sink

	mu     sync.Mutex
	runs   []*stageRun // submission order
	nextID int
}

// stageRun is one staged shell command. Its call + result pair becomes two
// messages in Flush; the assistant message holds the tool call, the user
// message holds the result, so the transcript stays well formed.
type stageRun struct {
	id     string
	cmd    string
	label  string
	cancel context.CancelFunc

	// result written by the goroutine once the tool completes
	done   chan struct{}
	result llm.ToolResultBlock
}

// NewStager returns a stager that runs commands through reg's bash tool and
// streams output to sink like an agent-initiated bash call.
func NewStager(reg *tools.Registry, sink agent.Sink) *Stager {
	return &Stager{reg: reg, sink: sink}
}

// Run starts cmd executing immediately. A disabled bash tool is a refusal
// notice rather than a run, matching the /tools widening rule; an empty command
// produces a notice and runs nothing. Run never blocks on the command itself.
func (s *Stager) Run(cmd string) {
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
	run := &stageRun{
		id:     id,
		cmd:    cmd,
		label:  "! " + strutil.FirstLine(cmd),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	s.runs = append(s.runs, run)
	s.mu.Unlock()

	input, _ := json.Marshal(map[string]any{"command": cmd})
	call := agent.ToolCall{ID: id, Name: toolBash, Input: input}
	out := agent.NewOutput(s.sink, id)
	done := s.sink.ToolStart(call, run.label)

	go func() {
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
		res.Details = shellProvenance{Source: "shell", TS: time.Now()}
		run.result = llm.ToolResultBlock{
			CallID:  id,
			Content: res.Content,
			Display: res.Display,
			Details: res.Details,
			IsError: res.IsError,
		}
		done(res)
	}()
}

// Pending reports whether any staged command is still running.
func (s *Stager) Pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		select {
		case <-r.done:
		default:
			return true
		}
	}
	return false
}

// Cancel interrupts every still-running staged command. A cancelled command
// still stages its partial output plus an interrupted marker, so the model's
// view matches what the user saw.
func (s *Stager) Cancel() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		select {
		case <-r.done:
		default:
			r.cancel()
		}
	}
}

// Flush waits for every staged command to finish, in submission order, and
// returns one assistant + user message pair per run. Building the next model
// prompt is the only caller that waits, so "the turn is held until the pending
// command finishes" falls out of Flush blocking.
func (s *Stager) Flush(ctx context.Context) []llm.Message {
	s.mu.Lock()
	runs := s.runs
	s.runs = nil
	s.mu.Unlock()

	var out []llm.Message
	for _, r := range runs {
		select {
		case <-r.done:
		case <-ctx.Done():
			// requeue the unfinished run so a later flush picks it up
			s.mu.Lock()
			s.runs = append(s.runs, r)
			s.mu.Unlock()
			return out
		}
		input, _ := json.Marshal(map[string]any{"command": r.cmd})
		out = append(out,
			llm.Message{Role: llm.RoleAssistant, Content: llm.BlockList{llm.ToolCallBlock{
				ID: r.id, Name: toolBash, Input: input,
			}}},
			llm.Message{Role: llm.RoleUser, Content: llm.BlockList{r.result}},
		)
	}
	return out
}

const toolBash = "bash"
