package agent

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
)

// contextThrottle is the minimum Used movement that triggers a Context emit, and
// contextInterval the longest we go without one while a stream moves at all. The
// pair keeps a streaming response from repainting on every tiny delta while still
// advancing visibly as tokens land.
const (
	contextThrottle = 32
	contextInterval = 80 * time.Millisecond
)

// errTurnRunning is returned by Recount while a turn owns the agent.
var errTurnRunning = errors.New("agent: turn running; recount refused")

// runTurns drains the follow-up queue, running one turn per queued input until
// nothing is left. Prompt hands in a single initial input; steering and
// follow-ups arrive on the queues while it runs.
func (a *Agent) runTurns(ctx context.Context, first []Input) error {
	a.mu.Lock()
	if a.running {
		a.follow = append(a.follow, first...)
	} else {
		a.steer = append(first[:0:0], first...) // the initial message is part of turn one
	}
	a.mu.Unlock()

	for {
		var input Input
		a.mu.Lock()
		if len(a.steer) > 0 {
			input = a.steer[0]
			a.steer = a.steer[1:]
		} else if len(a.follow) > 0 {
			input = a.follow[0]
			a.follow = a.follow[1:]
		}
		empty := input.Text == "" && len(input.Blocks) == 0
		idle := !a.running
		a.mu.Unlock()

		if empty {
			if idle {
				return nil // both queues drained and no turn in flight
			}
			continue
		}
		err := a.runTurn(ctx, input)
		// a real turn boundary: the hook decides whether an automatic compact fires.
		// It must not run mid-stream or between a tool call and its result.
		if err == nil && a.opts.Compact != nil {
			a.mu.Lock()
			idle := !a.running
			a.mu.Unlock()
			if idle {
				_, _ = a.opts.Compact(ctx, CompactThreshold)
			}
		}
		failed := err != nil
		a.mu.Lock()
		drained := len(a.steer) == 0 && len(a.follow) == 0
		running := a.running
		a.mu.Unlock()
		if failed {
			return err // observers are not told about errored turns
		}
		if drained && !running {
			// fully settled: notify observers, then re-read in case one queued work.
			// An observer that always queues loops forever, like a self-queueing follow-up.
			a.mu.Lock()
			a.settling++ // Steer/FollowUp accept here though no turn is running
			a.mu.Unlock()

			for _, obs := range a.opts.OnSettled {
				if obs != nil {
					obs(ctx)
				}
			}

			a.mu.Lock()
			a.settling--
			drained = len(a.steer) == 0 && len(a.follow) == 0
			running = a.running
			a.mu.Unlock()
			if drained && !running {
				return nil
			}
		}
	}
}

// runTurn drives one user turn: stream, dispatch tools, repeat until the model
// stops, the context ends, or the step limit is hit.
func (a *Agent) runTurn(ctx context.Context, input Input) error {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return nil // a turn owns the agent; queued inputs drain after it
	}
	a.running = true
	turnCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.running = false
		a.cancel = nil
		a.mu.Unlock()
		cancel()
	}()

	sink := a.sink
	// mirror the enabled set into state so the transcript records what this turn
	// could call; buildSystem derives its search hint from it.
	if ts := a.opts.Tools; ts != nil {
		a.state.Tools = ts.Names()
	} else {
		a.state.Tools = nil
	}
	maxSteps := a.opts.MaxSteps

	var overflowRetried bool // at most one compaction retry per turn
	result := TurnResult{Stop: llm.StopUnknown, Usage: llm.Usage{}}

	a.mu.Lock()
	promptInputs := append([]Input(nil), input)
	if len(a.steer) > 0 {
		// steering queued before the turn began rides along with the prompt
		promptInputs = append(promptInputs, a.steer...)
	}
	a.steer = nil
	a.mu.Unlock()

	sink.TurnStart(TurnInfo{Model: a.state.Model, Input: input})

	for step := 1; ; step++ {
		// the turn's own prompt lands once, before any stream
		if len(promptInputs) > 0 {
			a.appendSteer(promptInputs)
			promptInputs = nil
		}
		// steering submitted while running drains here at this step boundary
		a.drainSteer()

		msg, usage, stop, err := a.stream(turnCtx, sink)
		result.Usage.Add(usage)

		if err != nil {
			// an oversized request: compact aggressively and retry the same step once.
			// Nothing was appended for the failed call, so state stays in agreement.
			if llm.IsOverflow(err) && !overflowRetried && a.opts.Compact != nil {
				did, cerr := a.opts.Compact(ctx, CompactOverflow)
				if did && cerr == nil {
					overflowRetried = true
					continue // retry with the reduced context
				}
			}
			sink.Notice("turn failed: "+err.Error(), LevelError)
			result.Err = err
			break
		}
		// usage rides with the assistant message it came from so a session records
		// per-message token accounting (user echoes and tool results carry none).
		recount := needsRecount(a.state.Model.Caps, usage)
		a.append(MessageInfo{Message: msg, Stop: stop, Usage: usage})
		if recount {
			// a provider that reported no usage snaps to the local tokenizer's exact
			// count of what the next request (including this message) will send. A
			// failure falls back to the estimate append already recorded.
			_, _ = a.recount(turnCtx)
			// record the turn even though there is no provider report, so /usage's
			// count and estimated-footnote reflect what actually ran.
			if t := a.state.Tokens; t != nil {
				t.EstimatedTurn(a.state.Model.Key())
			}
		}
		a.syncContext(true)
		result.Steps = step

		if turnCtx.Err() != nil {
			// interrupted mid-stream: the partial assistant message stands,
			// and any partially-built tool_use is answered with a synthetic result
			a.appendToolResults(msg, nil)
			sink.Notice("interrupted by user", LevelInfo)
			result.Stop = llm.StopAborted
			break
		}

		calls := toolCalls(msg)
		if len(calls) == 0 {
			result.Stop = stop
			break
		}

		if step >= maxSteps {
			sink.Notice("turn hit the "+strconv.Itoa(maxSteps)+" step limit", LevelWarn)
			result.Stop = llm.StopMaxTokens // loop ran out, treat as a hard stop
			// answer this message's tool_use so the next request stays well formed
			a.appendToolResults(msg, nil)
			break
		}

		results := a.dispatch(turnCtx, sink, calls)
		a.appendToolResults(msg, results)

		if turnCtx.Err() != nil {
			// interrupted during tool execution; abortResults filled the gaps
			sink.Notice("interrupted by user", LevelInfo)
			result.Stop = llm.StopAborted
			break
		}
	}

	sink.TurnEnd(result)
	return result.Err
}

// drainSteer moves any new steering into the context as user messages, called
// at each step boundary so mid-turn input lands before the next stream.
func (a *Agent) drainSteer() {
	a.mu.Lock()
	in := a.steer
	a.steer = nil
	a.mu.Unlock()

	// host-queued prompts land after push-steers: they are the user's next
	// direction, read after this step's injected context
	if a.opts.OnBoundary != nil {
		in = append(in, a.opts.OnBoundary()...)
	}
	if len(in) > 0 {
		a.appendSteer(in)
	}
}

// appendSteer adds queued steering inputs as user messages at a step boundary.
// Input.Before is appended first, ahead of the user's text, so synthetic
// tool-call + result pairs land in transcript order before what the user said.
func (a *Agent) appendSteer(inputs []Input) {
	for _, in := range inputs {
		for _, m := range in.Before {
			a.append(MessageInfo{Message: m})
		}
		var blocks llm.BlockList
		if in.Text != "" {
			blocks = append(blocks, llm.TextBlock{Text: in.Text})
		}
		// extra content rides after the text when both are present (Input contract)
		blocks = append(blocks, in.Blocks...)
		if len(blocks) == 0 {
			continue // an empty steer would inject a blank user turn
		}
		a.append(MessageInfo{Message: llm.Message{Role: llm.RoleUser, Content: blocks}, Injected: in.Injected})
		if in.Delivered != nil {
			in.Delivered() // delivery is confirmed only when the message actually lands
		}
	}
}

// stream runs one model call and forwards every delta to the sink. It folds
// events into an Accumulator so rendering and assembly share a loop.
func (a *Agent) stream(ctx context.Context, sink Sink) (llm.Message, llm.Usage, llm.StopReason, error) {
	provider, err := a.opts.Provider(a.state.Model)
	if err != nil {
		return llm.Message{}, llm.Usage{}, 0, err
	}
	req := a.buildRequest()
	predicted := tokens.EstimateRequest(req)
	// the system prompt and tool schemas are built into every request, so they
	// must occupy context from the very first turn — not just after an exact report.
	if t := a.state.Tokens; t != nil {
		t.SetBase(tokens.EstimateFixed(req)) // replaced, so it self-corrects any seeded floor
	}

	// thinking deltas move the bar only when retention keeps them in the next request
	keepThink := llm.ResolveRetain(a.state.Reasoning.Retain, a.state.Model.Caps) != llm.RetainNone

	st, err := provider.Stream(ctx, req)
	if err != nil {
		return llm.Message{}, llm.Usage{}, 0, err
	}
	defer func() { _ = st.Close() }()

	// watch for cancellation and close the stream so a blocked Next unblocks.
	// Close abandons buffered events rather than draining them, which is what
	// stops in-flight tokens after an interrupt instead of flushing them out.
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = st.Close()
		case <-watchDone:
		}
	}()
	defer close(watchDone)

	var acc llm.Accumulator
	var prog toolProgress
	for ev, ok := st.Next(); ok; ev, ok = st.Next() {
		a.forward(sink, keepThink, &prog, ev)
		acc.Add(ev)
		if ctx.Err() != nil {
			break // nothing renders after an interrupt
		}
	}
	for _, p := range prog.clear() { // an aborted stream must not strand a row
		sink.ToolProgress(p)
	}

	msg := acc.Message()
	stop := acc.StopReason()
	usage := acc.Usage()
	if ctx.Err() != nil {
		// interrupted mid-stream: the partial message records its real reason
		stop = llm.StopAborted
	}
	// snap the exact terms to this response's report, unless a provider that
	// reported nothing needs the local tokenizer recount instead (done after append).
	if t := a.state.Tokens; t != nil && !needsRecount(a.state.Model.Caps, usage) {
		t.Response(a.state.Model.Key(), usage, predicted)
	}
	return msg, usage, stop, streamErr(st, &acc)
}

// buildRequest assembles the llm.Request the next provider call will receive,
// shared by the estimator, the recount hook and streaming.
func (a *Agent) buildRequest() llm.Request {
	messages := assemble(a.state, a.opts.Transforms)
	var tools []llm.ToolSchema
	if ts := a.opts.Tools; ts != nil {
		tools = ts.Schemas() // cached by the registry, so cheap per step
	}
	reasoning := a.state.Reasoning
	reasoning.Level = llm.ClampLevel(a.state.Model, reasoning.Level)
	var used int
	if t := a.state.Tokens; t != nil {
		used = t.Context().Used // 0 when the ledger is nil or empty
	}
	return llm.Request{
		Model:     a.state.Model,
		System:    buildSystem(a.state, a.opts.Env, a.opts.ProjectInstructions, a.opts.SystemSnippets),
		Messages:  messages,
		Tools:     tools,
		Reasoning: reasoning,
		MaxTokens: llm.MaxOutputFor(a.state.Model, used),
		SessionID: a.opts.SessionID,
	}
}

// forward maps one event onto sink calls and ledger updates. Block boundaries
// come from the end events; deltas stream through so rendering and assembly share
// a loop.
func (a *Agent) forward(sink Sink, keepThink bool, prog *toolProgress, ev llm.Event) {
	t := a.state.Tokens
	switch ev.Type {
	case llm.EventToolCallStart:
		sink.ToolProgress(prog.start(ev.ToolCallID, ev.Index, ev.ToolName))
	case llm.EventToolCallDelta:
		// arguments stream as partial JSON; report the size so a long write shows
		// movement rather than nothing until the call is complete
		if p, ok := prog.delta(ev.ToolCallID, ev.Index, ev.Text); ok {
			sink.ToolProgress(p)
		}
		if t != nil {
			t.Stream(tokens.EstimateText(ev.Text, tokens.KindProse))
		}
		a.syncContext(false)
	case llm.EventToolCallEnd:
		if p, ok := prog.end(ev.ToolCallID, ev.Index); ok {
			sink.ToolProgress(p)
		}
		a.syncContext(true)
	case llm.EventThinkingDelta:
		sink.Thinking(ev.Text)
		if t != nil && keepThink {
			t.Stream(tokens.EstimateText(ev.Text, tokens.KindProse))
		}
		a.syncContext(false) // the live bucket grew; repaint when it moves enough
	case llm.EventTextDelta:
		sink.Text(ev.Text)
		if t != nil {
			t.Stream(tokens.EstimateText(ev.Text, tokens.KindProse))
		}
		a.syncContext(false)
	case llm.EventThinkingEnd:
		sink.EndThinking()
		a.syncContext(true) // block end repaints unconditionally
	case llm.EventTextEnd:
		sink.EndText()
		a.syncContext(true)
	case llm.EventUsage:
		sink.Usage(ev.Usage)
		if t != nil {
			t.Partial(ev.Usage)
		}
	}
}

// streamErr resolves the error for a drained stream: the provider's final error,
// or none after a deliberate Close.
func streamErr(st llm.Stream, acc *llm.Accumulator) error {
	if err := st.Err(); err != nil {
		return err
	}
	return acc.Err()
}

// dispatch runs tool calls in parallel where every call is Parallel and the
// model supports it, otherwise serially, appending results in call order.
func (a *Agent) dispatch(ctx context.Context, sink Sink, calls []llm.ToolCallBlock) []llm.ToolResultBlock {
	parallel := a.state.Model.Caps.ParallelTools && allParallel(a.opts.Tools, calls)
	out := make([]llm.ToolResultBlock, len(calls))

	if parallel {
		sem := make(chan struct{}, runtime.NumCPU())
		var wg sync.WaitGroup
		for i, c := range calls {
			wg.Add(1)
			go func(i int, c llm.ToolCallBlock) {
				defer wg.Done()
				sem <- struct{}{}
				out[i] = a.runTool(ctx, sink, callFrom(c))
				<-sem
			}(i, c)
		}
		wg.Wait()
		return out
	}

	for i, c := range calls {
		if ctx.Err() != nil {
			break // a cancelled batch leaves the rest unanswered; abort fills them
		}
		out[i] = a.runTool(ctx, sink, callFrom(c))
	}
	return out
}

// runTool executes one tool and streams its output to the sink. A malformed or
// erroring tool is still a result, not a turn failure.
func (a *Agent) runTool(ctx context.Context, sink Sink, call ToolCall) llm.ToolResultBlock {
	if a.opts.Tools == nil {
		return llm.ToolResultBlock{CallID: call.ID, IsError: true,
			Content: llm.BlockList{llm.TextBlock{Text: "no tools configured"}}}
	}
	tool, ok := a.opts.Tools.Get(call.Name)
	if !ok {
		return llm.ToolResultBlock{CallID: call.ID, IsError: true,
			Content: llm.BlockList{llm.TextBlock{Text: "unknown tool"}}}
	}

	out := NewOutput(sink, call.ID)
	done := sink.ToolStart(call, tool.Label(call))
	res, err := tool.Execute(ctx, call, out)
	if res.Content == nil {
		res.Content = llm.BlockList{}
	}
	if err != nil && !res.IsError {
		res.IsError = true
	}
	// an erroring tool with empty content still hands the model something to see
	if res.IsError && err != nil {
		switch {
		case len(res.Content) == 0:
			res.Content = llm.BlockList{llm.TextBlock{Text: err.Error()}}
		case len(res.Content) == 1:
			if tb, ok := res.Content[0].(llm.TextBlock); ok && tb.Text == "" {
				tb.Text = err.Error()
				res.Content[0] = tb
			}
		}
	}
	done(res)
	return llm.ToolResultBlock{
		CallID: call.ID, ToolName: call.Name, Content: res.Content, IsError: res.IsError,
		Display: res.Display, Details: res.Details,
	}
}

// appendToolResults appends one user message holding every tool result in the
// order of its calls.
func (a *Agent) appendToolResults(msg llm.Message, results []llm.ToolResultBlock) {
	results = abortResults(msg, results)
	if len(results) == 0 {
		return
	}
	content := make(llm.BlockList, len(results))
	for i, r := range results {
		content[i] = r
	}
	a.append(MessageInfo{Message: llm.Message{Role: llm.RoleUser, Content: content}})
}

// append records one message in state and notifies the OnMessage hook so a
// session can persist it as the loop appends it. Called on the loop goroutine,
// so ordering matches the transcript exactly.
// modelOrigin returns the Origin stamp for an assistant message produced by the
// current model.
func (a *Agent) modelOrigin() *llm.Origin {
	return &llm.Origin{Provider: a.state.Model.Provider, Dialect: a.state.Model.Caps.Dialect, Model: a.state.Model.ID}
}

func (a *Agent) append(info MessageInfo) {
	// an assistant response is stamped with its producing model so cross-model history
	// can be degraded correctly on the next request; user and tool messages stay bare.
	if info.Message.Role == llm.RoleAssistant && info.Stop != llm.StopUnknown {
		n := info.Message
		n.Origin = a.modelOrigin()
		n.Stop = info.Stop
		info.Message = n
	}
	a.state.Messages = append(a.state.Messages, info.Message)
	for _, h := range a.opts.OnMessage {
		h(info)
	}
	// messages without a provider report occupy context only as an estimate. An
	// assistant response that reported usage is already counted in outputExact.
	if t := a.state.Tokens; t != nil && info.Usage == (llm.Usage{}) {
		t.Add(tokens.EstimateMessages([]llm.Message{info.Message}))
	}
}

// syncContext emits the ledger's context to the sink, throttled so a streaming
// response does not repaint on every delta. force bypasses the throttle for block
// ends and turn boundaries.
func (a *Agent) syncContext(force bool) {
	t := a.state.Tokens
	if t == nil {
		return
	}
	cs := t.Context()
	moved := cs.Used - a.ctxLast
	if moved < 0 {
		moved = -moved
	}
	now := time.Now()
	// emit when forced, when Used moved past the threshold, or when enough wall
	// clock elapsed since the last emit so even slow streams advance regularly.
	if !force && moved < contextThrottle && now.Sub(a.ctxLastAt) < contextInterval {
		return
	}
	a.ctxLast = cs.Used
	a.ctxLastAt = now
	a.sink.Context(cs)
}

// needsRecount reports whether a provider reported no usage and has an exact
// local tokenizer, which is when the loop recounts after appending instead of
// trusting an estimate.
func needsRecount(caps llm.Capabilities, u llm.Usage) bool {
	return tokens.Zero(u) && caps.Tokenizer == llm.TokenizerRemoteTokenize
}

// callFrom adapts a model tool-call block into the agent's ToolCall.
func callFrom(c llm.ToolCallBlock) ToolCall {
	return ToolCall{ID: c.ID, Name: c.Name, Input: c.Input}
}

// allParallel reports whether every requested tool allows parallel execution.
func allParallel(ts ToolSet, calls []llm.ToolCallBlock) bool {
	for _, c := range calls {
		t, ok := ts.Get(c.Name)
		if !ok || t.Mode() != ModeParallel {
			return false
		}
	}
	return len(calls) > 0
}

// toolCalls returns the tool-call blocks in an assistant message.
func toolCalls(msg llm.Message) []llm.ToolCallBlock {
	var out []llm.ToolCallBlock
	for _, b := range msg.Content {
		if tc, ok := b.(llm.ToolCallBlock); ok {
			out = append(out, tc)
		}
	}
	return out
}

// NewOutput returns an Output that forwards tool output to sink under callID,
// so a host-run tool (a staged ! command or an injected @ read) streams through
// the same path an agent-run tool uses.
func NewOutput(sink Sink, callID string) Output {
	return &sinkWriter{sink: sink, id: callID}
}

// sinkWriter forwards tool output deltas to the sink as they are written. It is
// safe for concurrent use so a tool can stream stdout and stderr from two goroutines.
type sinkWriter struct {
	sink Sink
	id   string
	mu   sync.Mutex
}

func (w *sinkWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sink.ToolOutput(w.id, string(p))
	return len(p), nil
}

// Diff forwards a rendered file change to the sink.
func (w *sinkWriter) Diff(path, before, after string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sink.Diff(path, before, after)
}
