package agent

import (
	"bytes"
	"context"
	"runtime"
	"strconv"
	"sync"

	"github.com/jentfoo/ajent/pkg/llm"
)

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
		failed := err != nil
		a.mu.Lock()
		drained := len(a.steer) == 0 && len(a.follow) == 0
		running := a.running
		a.mu.Unlock()
		if failed || (drained && !running) {
			return err
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

	sink := a.opts.Sink
	if sink == nil {
		sink = NopSink{}
	}
	maxSteps := a.opts.MaxSteps

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
			sink.Notice("turn failed: "+err.Error(), LevelError)
			result.Err = err
			break
		}
		// usage rides with the assistant message it came from so a session records
		// per-message token accounting (user echoes and tool results carry none).
		a.append(MessageInfo{Message: msg, Stop: stop, Usage: usage})
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
			sink.Notice("turn hit the "+itoa(maxSteps)+" step limit", LevelWarn)
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
	if len(in) > 0 {
		a.appendSteer(in)
	}
}

// appendSteer adds queued steering inputs as user messages at a step boundary.
func (a *Agent) appendSteer(inputs []Input) {
	for _, in := range inputs {
		var blocks llm.BlockList
		if in.Text != "" {
			blocks = append(blocks, llm.TextBlock{Text: in.Text})
		}
		// extra content rides after the text when both are present (Input contract)
		blocks = append(blocks, in.Blocks...)
		if len(blocks) == 0 {
			continue // an empty steer would inject a blank user turn
		}
		a.append(MessageInfo{Message: llm.Message{Role: llm.RoleUser, Content: blocks}})
	}
}

// stream runs one model call and forwards every delta to the sink. It folds
// events into an Accumulator so rendering and assembly share a loop.
func (a *Agent) stream(ctx context.Context, sink Sink) (llm.Message, llm.Usage, llm.StopReason, error) {
	provider, err := a.opts.Provider(a.state.Model)
	if err != nil {
		return llm.Message{}, llm.Usage{}, 0, err
	}
	messages := assemble(a.state, a.opts.Transform)

	var tools []llm.ToolSchema
	if ts := a.opts.Tools; ts != nil && len(ts.Schemas()) > 0 {
		tools = ts.Schemas()
	}

	req := llm.Request{
		Model:     a.state.Model,
		System:    buildSystem(a.state, a.opts.Env),
		Messages:  messages,
		Tools:     tools,
		Reasoning: a.state.Reasoning,
	}

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
	for ev, ok := st.Next(); ok; ev, ok = st.Next() {
		a.forward(sink, ev)
		acc.Add(ev)
		if ctx.Err() != nil {
			break // nothing renders after an interrupt
		}
	}

	msg := acc.Message()
	stop := acc.StopReason()
	if ctx.Err() != nil {
		// interrupted mid-stream: the partial message records its real reason
		stop = llm.StopAborted
	}
	return msg, acc.Usage(), stop, streamErr(st, &acc)
}

// forward maps one event onto sink calls. Block boundaries come from the end
// events; deltas stream through so rendering and assembly share a loop.
func (a *Agent) forward(sink Sink, ev llm.Event) {
	switch ev.Type {
	case llm.EventThinkingDelta:
		sink.Thinking(ev.Text)
	case llm.EventTextDelta:
		sink.Text(ev.Text)
	case llm.EventThinkingEnd:
		sink.EndThinking()
	case llm.EventTextEnd:
		sink.EndText()
	case llm.EventUsage:
		sink.Usage(ev.Usage)
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
	tool, ok := a.opts.Tools.Get(call.Name)
	if !ok {
		return llm.ToolResultBlock{CallID: call.ID, IsError: true,
			Content: llm.BlockList{llm.TextBlock{Text: "unknown tool"}}}
	}

	var buf bytes.Buffer
	out := &sinkWriter{sink: sink, id: call.ID}
	res, err := tool.Execute(ctx, call, out)
	if res.Content == nil {
		res.Content = llm.BlockList{}
	}
	if err != nil && !res.IsError {
		res.IsError = true
	}
	buf.WriteString(out.String())
	if buf.Len() > 0 && len(res.Content) == 1 {
		if tb, ok := res.Content[0].(llm.TextBlock); ok {
			tb.Text += "\n" + buf.String()
			res.Content[0] = tb
		}
	} else if buf.Len() > 0 {
		res.Content = append(res.Content, llm.TextBlock{Text: buf.String()})
	}
	if res.IsError && len(res.Content) == 1 {
		if tb, ok := res.Content[0].(llm.TextBlock); ok && tb.Text == "" {
			tb.Text = err.Error()
			res.Content[0] = tb
		}
	}
	return llm.ToolResultBlock{CallID: call.ID, Content: res.Content, IsError: res.IsError}
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
func (a *Agent) append(info MessageInfo) {
	a.state.Messages = append(a.state.Messages, info.Message)
	if h := a.opts.OnMessage; h != nil {
		h(info)
	}
}

// callFrom adapts a model tool-call block into the agent's ToolCall.
func callFrom(c llm.ToolCallBlock) ToolCall {
	return ToolCall{ID: c.ID, Name: c.Name, Input: c.Input}
}

// allParallel reports whether every requested tool allows parallel execution.
func allParallel(ts ToolSet, calls []llm.ToolCallBlock) bool {
	for _, c := range calls {
		t, ok := ts.Get(c.Name)
		if !ok || !t.Parallel() {
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

// itoa formats an int for notices.
func itoa(n int) string { return strconv.Itoa(n) }

// sinkWriter forwards tool output deltas to the sink as they are written.
type sinkWriter struct {
	sink Sink
	id   string
	buf  bytes.Buffer
}

func (w *sinkWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	w.sink.ToolOutput(w.id, string(p))
	return len(p), nil
}

func (w *sinkWriter) String() string { return w.buf.String() }
