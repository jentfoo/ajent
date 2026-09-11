package app

import (
	"context"
	"strings"
	"sync"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/refs"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tui"
)

type pumpLine struct {
	kind     command.Kind
	rest     string
	injected bool // non-typed input, excluded from transcript-derived prompt recall
	// input is an already-assembled prompt (the /init survey and its tool pairs)
	// re-entering the pump; it skips @ expansion and carries rest as a short label.
	input *agent.Input
	// onTurn runs as that input becomes a turn, so the sender can observe the turn
	// its work produced rather than guessing when it lands.
	onTurn func()
}

func submittedEcho(msg string) string {
	line := command.ParseLine(msg)
	if line.Kind == command.KindPrompt && strings.TrimSpace(line.Rest) != "" {
		return line.Rest
	}
	return ""
}

func submitPrompt(st *agent.State, editSinks []agent.Sink, est int, push func()) {
	if st.Tokens == nil || len(editSinks) == 0 {
		return
	}
	st.Tokens.SetSubmit(est)
	push()
}

func runPump(pump <-chan pumpLine, ag *agent.Agent, console *uiConsole, stager *command.Stager, expander *refs.Expander, recording bool, ui *tui.UI, started *bool, settled func(), q *steerQueue, gate *typingGate, st *agent.State, editSinks []agent.Sink, seedToolsOnce *sync.Once, pushContext func(), hooks planHooks) {
	for line := range pump {
		switch line.kind {
		case command.KindCommand:
			name, arg, ok := command.SplitCommand(line.rest)
			if !ok {
				console.Notify("unknown command /"+line.rest, tui.LevelWarn)
				continue
			}
			// MCP servers load eagerly so the pre-first-prompt /tools picker and /mcp
			// list already show them; LoadOnFirstMessage is idempotent (runs once).
			if console.mcp.m != nil && (name == "tools" || name == "mcp") {
				console.mcp.LoadOnFirstMessage(context.Background())
			}
			cmd, ok := console.commands.Get(name)
			if !ok {
				console.Notify("unknown command /"+name, tui.LevelWarn)
				continue
			}
			_ = cmd.Handler(context.Background(), arg, console)
		case command.KindPrompt:
			if line.input == nil && strings.TrimSpace(line.rest) == "" {
				continue
			}
			// connect every MCP server in full, once, so its tools exist before this
			// (the first) turn is assembled; /tools or /mcp changes made up to now hold
			if console.mcp.m != nil {
				console.mcp.LoadOnFirstMessage(context.Background())
			}
			// flush staged shell results ahead of the message, waiting for any
			// in-flight command to finish first
			before := stager.Flush(context.Background())

			in, echo, pending := promptInput(line, before, expander, func(n string) {
				console.Notify(n, tui.LevelWarn)
			})
			est := submitEstimate(in, pending)
			if q.offer(in, echo, est) {
				if gate != nil {
					gate.taken() // queued: pending() releases the hold; no handoff left to wait for
				}
				continue // queued as a dimmed row; the echo lands at delivery
			}
			// only reached with no drain running, so a workflow may branch here
			if hooks.beforePrompt != nil {
				if wrapped, ok := hooks.beforePrompt(context.Background(), in); ok {
					in = wrapped
					est = submitEstimate(in, pending)
				}
			}
			if echo != "" {
				ui.UserEcho(echo)
			}
			seedToolsOnce.Do(func() { st.Tokens.SetBase(ag.BaseEstimate(true)); pushContext() })
			submitPrompt(st, editSinks, est, pushContext)
			in.Settled = settled
			if gate != nil {
				gate.taken() // the submitted line starts its own turn; no handoff to wait for
			}
			startDrain(ui, recording, ag, q, in, started, hooks)
		}
	}
}

func promptInput(line pumpLine, before []agent.MessageInfo, expander *refs.Expander, warn func(string)) (agent.Input, string, int) {
	if line.input != nil {
		in := *line.input
		in.Before = append(before, in.Before...)
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
		Before:   before,
		After:    res.Run,
		Injected: line.injected,
	}, submittedEcho(line.rest), res.Est // "" unless a real prompt; commands and shell lines are not echoed here
}

func submitEstimate(in agent.Input, pending int) int {
	est := tokens.EstimateText(in.Text, tokens.KindProse) + pending
	if len(in.Before) > 0 {
		est += tokens.EstimateMessages(agent.BeforeMessages(in.Before))
	}
	return est
}

func startDrain(ui *tui.UI, recording bool, ag *agent.Agent, q *steerQueue, input agent.Input, started *bool, hooks planHooks) {
	ui.SetIdle(false)
	*started = true
	go func() {
		for {
			err := ag.Prompt(context.Background(), input)
			// a workflow owns the next turn when it says so, errored turns included:
			// its executor-retry rule depends on being reached after a failure.
			if hooks.advance != nil {
				if next, ok := hooks.advance(context.Background()); ok {
					input = next
					continue
				}
			}
			if err != nil {
				q.stopDrain() // leave items queued as rows; do not hammer a failing provider
				break
			}
			next, ok := q.take()
			if !ok {
				break
			}
			input = next
		}
		if recording {
			ui.SetIdle(true)
		}
	}()
}
