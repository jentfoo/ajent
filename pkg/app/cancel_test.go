package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
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
	var calls int
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
	}, time.Second*2, time.Millisecond*10)

	ag.Interrupt()

	var promptErr error
	require.Eventually(t, func() bool { // poll so teardown (group-kill + reap) can take variable real time
		select {
		case e := <-errCh:
			promptErr = e
			return true
		default:
			return false
		}
	}, 10*time.Second, time.Millisecond*10)
	require.NoError(t, promptErr) // an interrupted turn is a clean stop, not an error
	assert.Equal(t, llm.StopAborted, rec.last().Stop)

	// prove the whole process group (leader and TERM-trapping grandchild) is gone
	data, rerr := os.ReadFile(dir + "/pid.txt")
	require.NoError(t, rerr)
	pid, aerr := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, aerr)
	require.Eventually(t, func() bool {
		err := syscall.Kill(pid, 0)
		return err != nil && errors.Is(err, syscall.ESRCH)
	}, time.Second*2, time.Millisecond*30)
	require.Eventually(t, func() bool {
		err := syscall.Kill(-pid, 0)
		return err != nil && errors.Is(err, syscall.ESRCH)
	}, time.Second*2, time.Millisecond*30)

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
	assert.True(t, found)
}

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

	require.Eventually(t, func() bool { return bp.created.Load() >= 1 }, time.Second, time.Millisecond)
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
	}, time.Second, 10*time.Millisecond)
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

func (p *cancelPrompter) Hold(context.Context) {}

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
	b := permit.NewBarrier(func(string) bool { return false }, permit.Options{
		Mode: permit.ModeAuto, ModeSet: true,
		Prompter: prompter, Classifier: permit.NewCachedClassifier(adapter.Classify),
	})

	// askAllow drives one asker call, answering the dialog it opens with Allow
	askAllow := func() tools.Decision {
		dialogsBefore := prompter.count()
		createdBefore := bp.created.Load()
		var got tools.Decision
		done := make(chan struct{})
		go func() { got = askCancel(b, t.Context(), `{"command":"stat f.txt"}`); close(done) }()
		// wait for this ask's own dialog to open and its classifier stream to start
		require.Eventually(t, func() bool { return prompter.count() > dialogsBefore }, time.Second, time.Millisecond)
		prompter.mu.Lock()
		d := prompter.dialogs[len(prompter.dialogs)-1]
		prompter.mu.Unlock()
		require.Eventually(t, func() bool { return bp.created.Load() >= createdBefore+1 }, time.Second, time.Millisecond)
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
	}, time.Second, 10*time.Millisecond)

	// the cancelled verdict was ClassUnsure, never cached: a second ask re-invokes the model
	assert.Equal(t, tools.ActionAllow, askAllow().Action)
	require.Eventually(t, func() bool { return bp.created.Load() >= 2 }, time.Second, time.Millisecond)
}

// askCancel drives one barrier asker call and returns its decision.
func askCancel(b *permit.Barrier, ctx context.Context, input string) tools.Decision {
	return b.Asker()(ctx, agent.ToolCall{ID: "c", Name: "bash", Input: []byte(input)}, tools.Decision{Action: tools.ActionAsk})
}

// TestControlInterruptCancelsStagedShell pins a turn mid-tool so ag.Running()
// holds, stages a real sleeping shell beside it, and drives one interrupt
// control through the loop: the interrupt must reach the staged run too, not
// only the turn.
func TestControlInterruptCancelsStagedShell(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		control tui.Control
	}{
		{"ctrl_c", tui.ControlInterrupt},
		{"escape", tui.ControlEscape},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inR, inW, err := os.Pipe()
			require.NoError(t, err)
			outR, outW, err := os.Pipe()
			require.NoError(t, err)
			t.Cleanup(func() {
				_ = inR.Close()
				_ = inW.Close()
				_ = outR.Close()
			})
			go func() { _, _ = io.Copy(io.Discard, outR) }() // status writes must not fill the pipe
			ui, err := tui.New(tui.Options{In: inR, Out: outW, Mode: tui.ModePlain})
			require.NoError(t, err)
			t.Cleanup(ui.Close)

			reg, err := tools.Builtins(tools.Options{Cwd: t.TempDir(), SessionID: "ctrl-staged"})
			require.NoError(t, err)
			release := make(chan struct{})
			entered := make(chan struct{})
			p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: mainToolTurn()}}}
			st := &agent.State{Model: llm.Model{ID: "test"}, Reasoning: llm.ReasoningConfig{}}
			ag := agent.New(st, agent.Options{
				Provider: func(llm.Model) (llm.Provider, error) { return p, nil },
				Tools:    singleToolSet{tool: holdBash{release: release, entered: entered}},
				Env:      agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
			})

			stager := command.NewStager(context.Background(), reg, agent.NopSink{})
			stager.Run("sleep 30", false)
			require.True(t, stager.Pending())

			quit := make(chan struct{})
			controls := make(chan tui.Control, 4)
			go controlLoop(context.Background(), ui, controls,
				newHintBoard(func(string, string) {}), ag,
				newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}), stager, nil, quit, nil)

			errCh := make(chan error, 1)
			go func() { errCh <- ag.Prompt(t.Context(), agent.Input{Text: "run it"}) }()
			<-entered // the turn is pinned in its tool call, so ag.Running() holds

			controls <- tc.control
			require.Eventually(t, func() bool { return !stager.Pending() },
				5*time.Second, time.Millisecond, "the staged shell must be cancelled")

			// the cancelled command still stages its partial result, marked interrupted
			msgs := stager.Flush(t.Context())
			require.Len(t, msgs, 1)
			tb, ok := msgs[0].Message.Content[0].(llm.TextBlock)
			require.True(t, ok)
			assert.Contains(t, tb.Text, "User Ran: sleep 30")
			assert.Contains(t, tb.Text, agent.InterruptedText)

			// the turn itself aborts cleanly
			close(release)
			select {
			case err := <-errCh:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("turn did not finish")
			}
		})
	}
}
