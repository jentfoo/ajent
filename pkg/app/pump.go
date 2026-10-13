package app

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/refs"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tui"
	tuisink "github.com/jentfoo/ajent/pkg/tui/sink"
)

type pumpLine struct {
	kind command.Kind
	rest string
	// blocks are image payloads captured from clipboard tokens in rest, appended
	// to the turn's input after the text.
	blocks []llm.Block
	// input is an already-assembled prompt (the /init survey and its tool pairs)
	// re-entering the pump, and it skips @ expansion and carries rest as a short label.
	input *agent.Input
	// onTurn runs as that input becomes a turn, so the sender can observe the turn
	// its work produced rather than guessing when it lands.
	onTurn func()
	// wake flushes queued state writes without dispatching, sent at drain exit
	wake bool
}

// submittedEcho returns a dispatched prompt line's echo: blank input stays
// silent (an image-only turn renders its own row), everything else echoes
// verbatim. Commands and shell lines never reach the pump as prompts, so no
// re-classification happens here.
func submittedEcho(msg string) string {
	if strings.TrimSpace(msg) == "" {
		return ""
	}
	return msg
}

func submitPrompt(st *agent.State, editSinks []agent.Sink, est int, push func()) {
	if len(editSinks) == 0 {
		return
	}
	st.Tokens.SetSubmit(est)
	push()
}

// startedFlag marks whether a prompt has been dispatched this run, so tool
// schemas may have committed to context. The pump flips it once; command
// handlers and the rewind path read it from other goroutines.
type startedFlag struct {
	v atomic.Bool
}

func newStartedFlag(v bool) *startedFlag {
	s := &startedFlag{}
	s.v.Store(v)
	return s
}

func (s *startedFlag) mark()        { s.v.Store(true) }
func (s *startedFlag) marked() bool { return s.v.Load() }

// runPump serves submitted lines until ctx (appRoot) is canceled. The pump
// channel is never closed, so senders never race a close.
func runPump(ctx context.Context, pump <-chan pumpLine, ag *agent.Agent, console *uiConsole, stager *command.Stager, expander *refs.Expander, recording bool, ui *tui.UI, started *startedFlag, settled func(), q *steerQueue, gate *typingGate, st *agent.State, editSinks []agent.Sink, seedToolsOnce *sync.Once, pushContext func(), hooks planHooks, drain *drainWaiter) {
	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-pump:
			if !ok {
				return
			}
			console.applyDeferred() // a dispatch or drain exit lands queued writes
			if line.wake {
				continue
			}
			switch line.kind {
			case command.KindCommand:
				name, arg, ok := command.SplitCommand(line.rest)
				if !ok {
					console.Notify("unknown command /"+line.rest, tui.LevelWarn)
					continue
				}
				// MCP servers load eagerly so the pre-first-prompt /tools picker and /mcp
				// list already show them, and LoadOnFirstMessage is idempotent (runs once).
				if console.mcp.m != nil && (name == "tools" || name == "mcp") {
					console.mcp.LoadOnFirstMessage(ctx)
				}
				cmd, ok := console.commands.Get(name)
				if !ok {
					console.Notify("unknown command /"+name, tui.LevelWarn)
					continue
				}
				_ = cmd.Handler(ctx, arg, console)
			case command.KindPrompt:
				if line.input == nil && strings.TrimSpace(line.rest) == "" && len(line.blocks) == 0 {
					continue
				}
				// connect every MCP server in full, once, so its tools exist before this
				// (the first) turn is assembled, and /tools or /mcp changes made up to now hold
				if console.mcp.m != nil {
					console.mcp.LoadOnFirstMessage(ctx)
				}
				in, echo, pending := promptInput(line, expander, func(n string) {
					console.Notify(n, tui.LevelWarn)
				})
				est := submitEstimate(in, pending)
				if q.offer(in, echo, est) {
					if gate != nil {
						gate.taken() // queued: pending() releases the hold, no handoff left to wait for
					}
					continue // queued as a dimmed row; staged output rides its delivery, not its submit
				}
				// direct dispatch: delivery is now, so staged shell output flushes ahead
				// of the message, waiting for any in-flight command to finish first
				before := stager.Flush(ctx)
				in.Before = append(before, in.Before...)
				est = submitEstimate(in, pending)
				// only reached with no drain running, so a workflow may branch here
				captured := false
				if hooks.beforePrompt != nil {
					if wrapped, ok := hooks.beforePrompt(ctx, in); ok {
						in = wrapped
						est = submitEstimate(in, pending)
						captured = true
					}
				}
				// parked at a user gate: only the captured submission clears it, so
				// anything else joins the queued rows and delivers once the gate passes
				if !captured && hooks.parked != nil && hooks.parked() {
					q.hold(in, echo, est)
					if gate != nil {
						gate.taken()
					}
					continue // held as a dimmed row, the echo lands at delivery
				}
				if echo != "" {
					ui.UserEcho(echo)
				}
				if imgs := tuisink.Images(line.blocks); len(imgs) > 0 {
					ui.UserImages(imgs)
				}
				seedToolsOnce.Do(func() { st.Tokens.SetBase(ag.BaseEstimate(true)); pushContext() })
				submitPrompt(st, editSinks, est, pushContext)
				in.Settled = settled
				if gate != nil {
					gate.taken() // the submitted line starts its own turn, no handoff to wait for
				}
				startDrain(ctx, ui, recording, ag, q, []agent.Input{in}, started, hooks, drain)
			}
		}
	}
}

func promptInput(line pumpLine, expander *refs.Expander, warn func(string)) (agent.Input, string, int) {
	if line.input != nil {
		in := *line.input
		in.Prepared = true // assembled by its sender, the append seam must not re-expand
		if line.onTurn != nil {
			line.onTurn() // this turn writes, not the one running when the sender finished
		}
		return in, line.rest, 0
	}
	res := expander.Expand(line.rest)
	for _, n := range res.Notices {
		warn(n)
	}
	return agent.Input{
		Text:     res.Text,
		Blocks:   line.blocks, // clipboard images ride after the text
		After:    res.Run,
		Prepared: true, // expanded here, the append seam must not re-expand
	}, submittedEcho(line.rest), res.Est // blank stays silent: an image-only turn renders its own row
}

func submitEstimate(in agent.Input, pending int) int {
	est := tokens.EstimateText(in.Text, tokens.KindProse) + tokens.EstimateBlocks(in.Blocks) + pending
	if len(in.Before) > 0 {
		est += tokens.EstimateMessages(agent.BeforeMessages(in.Before))
	}
	return est
}

// drainWaiter tracks the live drain goroutine so shutdown can wait for its
// abort fill before teardown reads the transcript. Only one drain runs at a
// time (offers queue while one drains), so the newest channel covers all.
type drainWaiter struct {
	mu   sync.Mutex
	done chan struct{} // closed at drain exit, nil when none ever ran
	// wake fires at drain exit, so the pump lands queued state writes without
	// waiting for the next submitted line. Set before the first launch
	wake func()
}

// launch runs fn as the drain goroutine, closing its exit channel on return
// and firing wake after fn settles.
func (d *drainWaiter) launch(fn func()) {
	ch := make(chan struct{})
	d.mu.Lock()
	d.done = ch
	wake := d.wake
	d.mu.Unlock()
	go func() {
		defer close(ch)
		fn()
		if wake != nil {
			wake()
		}
	}()
}

// wait blocks until the drain goroutine exits or timeout passes. Returns at
// once when none ran, so an idle exit costs nothing.
func (d *drainWaiter) wait(timeout time.Duration) {
	d.mu.Lock()
	ch := d.done
	d.mu.Unlock()
	if ch == nil {
		return
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-ch:
	case <-t.C:
	}
}

// startDrain runs inputs as the turn goroutine, its exit observable on drain.
func startDrain(ctx context.Context, ui *tui.UI, recording bool, ag *agent.Agent, q *steerQueue, inputs []agent.Input, started *startedFlag, hooks planHooks, drain *drainWaiter) {
	ui.SetIdle(false)
	started.mark()
	drain.launch(func() {
		for {
			err := ag.PromptBatch(ctx, inputs)
			// a workflow owns the next turn when it says so, errored turns included:
			// its executor-retry rule depends on being reached after a failure.
			if hooks.advance != nil {
				if next, ok := hooks.advance(ctx); ok {
					inputs = []agent.Input{next}
					continue
				}
			}
			// parked at a user gate: queued steering never passed beforePrompt, so
			// delivering it here would run an ungated turn. It holds until the gated
			// submit starts the next one.
			if hooks.parked != nil && hooks.parked() {
				q.stopDrain()
				break
			}
			if err != nil {
				q.stopDrain() // leave items queued as rows, do not hammer a failing provider
				break
			}
			next, ok := q.take(ctx)
			if !ok {
				break
			}
			inputs = next
		}
		if recording {
			ui.SetIdle(true)
		}
	})
}
