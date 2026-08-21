package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// devBashCallTurn scripts a turn whose only output is one bash tool call.
func devBashCallTurn(id, name, args string) []llm.Event {
	return []llm.Event{
		{Type: llm.EventMessageStart},
		{Type: llm.EventToolCallStart, Index: 0, ToolCallID: id, ToolName: name},
		{Type: llm.EventToolCallEnd, Index: 0, Block: llm.ToolCallBlock{
			ID: id, Name: name, Input: json.RawMessage(args)}},
		{Type: llm.EventDone, StopReason: llm.StopToolUse},
	}
}

// wellFormedMain reports whether every ToolCallBlock in messages has a matching
// ToolResultBlock, which is what keeps the next Anthropic request valid.
func wellFormedMain(msgs []llm.Message) bool {
	calls := 0
	for _, m := range msgs {
		for _, b := range m.Content {
			switch blk := b.(type) {
			case llm.ToolCallBlock:
				calls++
			case llm.ToolResultBlock:
				if blk.CallID == "" {
					return false // an unanswered tool_use would 400 the next request
				}
				calls--
			}
		}
	}
	return calls == 0
}

// resultTextOf extracts concatenated text from a ToolResultBlock.
func resultTextOf(tr llm.ToolResultBlock) string {
	var sb strings.Builder
	for _, b := range tr.Content {
		if tb, ok := b.(llm.TextBlock); ok {
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}

// TestInterruptCancelsRunningBashEndToEnd is the phase's done-when: an interrupt
// while a turn's bash runs kills the process group, records partial output as an
// interrupted error result in call order, and Prompt returns promptly with StopAborted.
func TestInterruptCancelsRunningBashEndToEnd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	reg, err := tools.Builtins(tools.Options{Cwd: dir, SessionID: "cancel-e2e"})
	require.NoError(t, err)

	args := `{"command":"echo $$ > pid.txt; sh -c 'trap \"\" TERM; sleep 300'; echo finished"}`
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Events: devBashCallTurn("c1", "bash", args)},
	}}
	rec := &turnRecorder{}
	st := &agent.State{Model: llm.Model{ID: "test", Provider: "scripted"}, Reasoning: llm.ReasoningConfig{}}
	ag := agent.New(st, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) { return p, nil },
		Tools:    reg,
		Sinks:    []agent.Sink{rec},
		Env:      agent.Environment{Cwd: dir, OS: "linux/amd64", Date: "2026-08-20"},
	})

	errCh := make(chan error, 1)
	go func() { errCh <- ag.Prompt(t.Context(), agent.Input{Text: "run it"}) }()

	require.Eventually(t, func() bool {
		data, rerr := os.ReadFile(dir + "/pid.txt")
		return rerr == nil && len(strings.TrimSpace(string(data))) > 0
	}, time.Second*2, time.Millisecond*10, "the bash command must be running before the interrupt")

	ag.Interrupt()

	select {
	case err := <-errCh:
		require.NoError(t, err) // an interrupted turn is a clean stop, not an error
	case <-time.After(time.Second * 5):
		t.Fatal("Prompt did not return promptly after the interrupt")
	}
	assert.Equal(t, llm.StopAborted, rec.last().Stop)

	// prove the whole process group (leader and TERM-trapping grandchild) is gone
	data, rerr := os.ReadFile(dir + "/pid.txt")
	require.NoError(t, rerr)
	pid, aerr := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, aerr)
	require.Eventually(t, func() bool {
		err := syscall.Kill(pid, 0)
		return err != nil && errors.Is(err, syscall.ESRCH)
	}, time.Second*2, time.Millisecond*30, "the bash leader must be gone")
	require.Eventually(t, func() bool {
		err := syscall.Kill(-pid, 0)
		return err != nil && errors.Is(err, syscall.ESRCH)
	}, time.Second*2, time.Millisecond*30, "no descendant of the group may survive")

	// every tool call is answered, and the bash result reads as an interruption
	assert.True(t, wellFormedMain(st.Messages))
	var found bool
	for _, m := range st.Messages {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, b := range m.Content {
			if tr, ok := b.(llm.ToolResultBlock); ok && tr.CallID == "c1" {
				found = true
				assert.True(t, tr.IsError)
				text := resultTextOf(tr)
				assert.Contains(t, text, "interrupted by user")
			}
		}
	}
	assert.True(t, found, "the bash call must have an interrupted error result")
}

// TestClassifierAdapterCancelClosesStream verifies a cancelled classifier model
// call stops reading immediately and maps to ClassUnsure (never cached).
func TestClassifierAdapterCancelClosesStream(t *testing.T) {
	t.Parallel()

	onClose := make(chan struct{}, 1)
	bp := &blockingProvider{
		turn:    []llm.Event{{Type: llm.EventTextDelta, Text: "read"}},
		onClose: onClose,
	}
	adapter := classifierAdapter{
		providerFor: func(llm.Model) (llm.Provider, error) { return bp, nil },
		model:       func() llm.Model { return llm.Model{ID: "p/m"} },
	}

	ctx, cancel := context.WithCancel(t.Context())
	type clres struct {
		c permit.Class
	}
	resCh := make(chan clres, 1)
	go func() { resCh <- clres{adapter.Classify(ctx, permit.Subject{Name: "bash", Args: "stat a"})} }()

	require.Eventually(t, func() bool { return bp.created.Load() >= 1 }, time.Second, time.Millisecond,
		"the classifier model call must start before cancelling")
	cancel()

	var got clres
	select {
	case got = <-resCh:
	case <-time.After(time.Second * 3):
		t.Fatal("Classify did not return after cancellation")
	}
	assert.Equal(t, permit.ClassUnsure, got.c) // cancelled: never a partial "readonly" verdict

	require.Eventually(t, func() bool {
		select {
		case <-onClose:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond, "the classifier stream must be closed on cancel")
}

// cancelFakeDialog is an approval dialog that blocks until resolved or closed.
type cancelFakeDialog struct {
	ch       chan int
	resolved sync.Once
}

func (d *cancelFakeDialog) Wait(ctx context.Context) (int, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case idx := <-d.ch:
		return idx, nil
	}
}
func (d *cancelFakeDialog) Resolve(index int) { d.resolved.Do(func() { d.ch <- index }) }
func (d *cancelFakeDialog) Close()            {}

// cancelPrompter opens dialogs and records them for per-ask answering.
type cancelPrompter struct {
	mu      sync.Mutex
	dialogs []*cancelFakeDialog
}

func newCancelPrompter() *cancelPrompter { return &cancelPrompter{} }

func (p *cancelPrompter) Open(string, string, []string) (permit.Dialog, error) {
	d := &cancelFakeDialog{ch: make(chan int, 1)}
	p.mu.Lock()
	p.dialogs = append(p.dialogs, d)
	p.mu.Unlock()
	return d, nil
}

func (p *cancelPrompter) Reason(context.Context, string) (string, bool) { return "", false }

// count returns how many dialogs have been opened.
func (p *cancelPrompter) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.dialogs)
}

// TestUserAllowCancelsClassifierCall is the user's exact scenario end to end: an
// auto-mode dialog answer cancels the in-flight classifier model call, nothing is
// cached, and a second ask re-invokes the model.
func TestUserAllowCancelsClassifierCall(t *testing.T) {
	t.Parallel()

	onClose := make(chan struct{}, 1)
	bp := &blockingProvider{
		turn:    []llm.Event{{Type: llm.EventTextDelta, Text: "read"}},
		onClose: onClose,
	}
	adapter := classifierAdapter{
		providerFor: func(llm.Model) (llm.Provider, error) { return bp, nil },
		model:       func() llm.Model { return llm.Model{ID: "p/m"} },
	}

	prompter := newCancelPrompter()
	b := permit.NewBarrier(func(string) bool { return false })
	b.SetPrompter(prompter)
	b.SetMode(permit.ModeAuto)
	b.SetClassifier(permit.NewCachedClassifier(adapter.Classify))

	// askAllow drives one asker call, answering the dialog it opens with Allow.
	askAllow := func() tools.Decision {
		dialogsBefore := prompter.count()
		createdBefore := bp.created.Load()
		var got tools.Decision
		done := make(chan struct{})
		go func() { got = askCancel(b, t.Context(), `{"command":"stat f.txt"}`); close(done) }()
		// wait for this ask's own dialog to open and its classifier stream to start
		require.Eventually(t, func() bool { return prompter.count() > dialogsBefore }, time.Second, time.Millisecond,
			"a new approval dialog must open for each ask")
		prompter.mu.Lock()
		d := prompter.dialogs[len(prompter.dialogs)-1]
		prompter.mu.Unlock()
		require.Eventually(t, func() bool { return bp.created.Load() >= createdBefore+1 }, time.Second, time.Millisecond,
			"the classifier stream must be in flight before answering")
		d.Resolve(0) // "Allow"
		select {
		case <-done:
		case <-time.After(time.Second * 3):
			t.Fatal("the asker did not return after the dialog was answered")
		}
		return got
	}

	// first ask: user answers Allow before the classifier returns
	assert.Equal(t, tools.ActionAllow, askAllow().Action)

	require.Eventually(t, func() bool {
		select {
		case <-onClose:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond, "the cancelled classification stream must be closed")

	// the cancelled verdict was ClassUnsure, never cached: a second ask re-invokes the model
	assert.Equal(t, tools.ActionAllow, askAllow().Action)
	require.Eventually(t, func() bool { return bp.created.Load() >= 2 }, time.Second, time.Millisecond,
		"the classifier must run again for an identical subject after cancellation")
}

// askCancel drives one barrier asker call and returns its decision.
func askCancel(b *permit.Barrier, ctx context.Context, input string) tools.Decision {
	return b.Asker()(ctx, agent.ToolCall{ID: "c", Name: "bash", Input: []byte(input)}, tools.Decision{Action: tools.ActionAsk})
}
