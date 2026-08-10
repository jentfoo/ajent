# Agent loop design

How `pkg/agent` turns a model provider into an interactive coding agent: the
prompt -> stream -> tool-call -> repeat loop, the event sink any front end can
adapt onto, and interruption as a first-class operation. It is the reference for
the turn loop; tools, sessions and compaction all build on it.

## Shape

`Agent` owns one `State`, which is the in-memory projection of the session:

```go
type State struct {
	Messages  []llm.Message      // the transcript, appended as turns progress
	Model     llm.Model          // what requests default to; /model switches this
	Reasoning llm.ReasoningConfig
	Tools     []string           // active tool names, in declaration order
}
```

`State` is owned by whatever goroutine runs a turn. The only fields guarded by
the agent's mutex are the queue and the running flag — never `Messages`, so the
loop can append without contending with steering.

```go
type Agent struct {
	opts  Options
	state *State

	mu      sync.Mutex
	running bool       // a turn owns the agent while true
	steer   []Input    // injected into the running turn at the next boundary
	follow  []Input    // run as separate turns once the current one settles
	cancel  context.CancelFunc // interrupts cancel this
}
```

`Prompt(ctx, input)` runs `input` to completion including any follow-up queued
while it ran, and returns when both queues are empty or the context ends. It is
single-owner: only one goroutine drives turns at a time (in `cmd/ajent`, that is
the main message loop). Mid-turn input goes through `Steer` / `FollowUp`, never
a concurrent second `Prompt`.

## The sink, not the UI

The agent emits events on a `Sink`; it never imports the TUI and the TUI never
imports it. That is what makes headless mode, a sub-agent and tests all possible
with one loop.

```go
type Sink interface {
	TurnStart(TurnInfo)
	Thinking(delta string)
	EndThinking()
	Text(delta string)
	EndText()
	ToolStart(call ToolCall) func(ToolResult)
	ToolOutput(callID, delta string)
	Diff(path, before, after string)
	Usage(llm.Usage)
	Notice(msg string, level Level)
	TurnEnd(TurnResult)
}
```

`cmd/ajent` provides a `tuisink` that maps these almost 1:1 onto `tui.UI`.
`NopSink` discards everything for sub-agents with no UI.

## The loop

```
drain follow-up queue -> for each turn:
    sink.TurnStart
    append the prompt and any pre-start steering as user messages
    for step := 0; step < maxSteps; step++ {
        drain new steering into state as user messages   (boundary)
        req = request(state, env, tools)                 assemble()
        msg, usage = a.stream(ctx, req)                  forwards deltas to sink
        append msg to state.Messages
        if ctx cancelled -> StopAborted, end turn
        calls := toolCalls(msg); if none -> break (end_turn)
        results := dispatch(calls)                       parallel or serial
        appendToolResults(msg, results)                  call order preserved
    }
    sink.TurnEnd
```

### Assembly is pure

`assemble(state, transform)` returns the message list for one request as a pure
function of `State`. It never mutates it. Compaction and plan projection both
work by transforming this assembled list rather than rewriting `State`.

### System prompt stays cache-stable

`buildSystem(state, env)` builds one neutral, focused system block: an
identity sentence naming the working directory, a short "how you help" line,
concise guideline bullets, then environment facts as clean structured lines
(working directory, platform, shell, date, git branch). It is cache-stable: only
a day-granular date changes between requests, so the provider's prompt cache
survives. The `Platform` and `Shell` lines are omitted when empty; the whole git
line drops when there is no branch (a dirty tree appends "(dirty)"). Git probes
fail silently to empty so an unreachable repo cannot stall or
break the turn.

### Step limit

A runaway tool loop must stop. `defaultMaxSteps = 100`; hitting it ends the turn
cleanly with a notice, not as an error.

## Streaming and cancellation

`a.stream` drives `llm.Stream.Next()` and folds each event into an
`llm.Accumulator` in the same loop, forwarding deltas to the sink so rendering
and assembly share one pass over the events:

| Event | Sink call |
|---|---|
| `ThinkingDelta` | `Thinking(delta)` |
| `TextDelta` | `Text(delta)` |
| `ThinkingEnd` | `EndThinking()` |
| `TextEnd` | `EndText()` |

Block boundaries come from the end events; deltas stream through. This keeps
`EndThinking` before the first `Text`, which is what lets the TUI render a clean
thinking block.

### Interrupt

Interruption is cancellation, not draining:

- `Prompt` derives a cancellable context and stores its cancel func under the
  agent mutex; `Agent.Interrupt()` calls it. Key handling in `cmd/ajent` never
  touches the agent's internals.
- `a.stream` starts one watcher goroutine that calls `stream.Close()` on
  `ctx.Done()`. Close abandons buffered events rather than draining them, so
  in-flight tokens are dropped at the boundary instead of being flushed out —
  the goal is zero incoming tokens after an interrupt.
- On abort the partial assistant message from the Accumulator is still appended,
  then every unanswered `ToolCallBlock` gets a synthetic error result reading
  "interrupted by user". Without this, a cancelled turn leaves a dangling
  `tool_use` and the next Anthropic request 400s — the single most common way an
  agent ends up permanently broken.
- The interrupted turn reports `TurnResult{Stop: StopAborted}` plus a visible
  notice. No adapter ever emits `StopAborted`; only the agent sets it.

## Tool dispatch

The tool surface is an interface, keeping the loop decoupled from any concrete
tool implementation and fully testable:

```go
type Tool interface {
	Schema() llm.ToolSchema
	Parallel() bool // a future ExecutionMode could widen this
	Execute(ctx context.Context, call ToolCall, out io.Writer) (ToolResult, error)
}

type ToolSet interface {
	Get(name string) (Tool, bool)
	Schemas() []llm.ToolSchema
}
```

`out` streams incremental tool output; the loop forwards it to
`sink.ToolOutput(callID, delta)`.

Dispatch rules:

- **Serial by default.** Parallel only when every call in the batch is a
  `Parallel()` tool and `state.Model.Caps.ParallelTools` is set.
- **Bounded parallelism** with `runtime.NumCPU()`, via a semaphore channel plus
  `sync.WaitGroup`. An `errgroup` was considered but `golang.org/x/sync` is not
  in the module, and no new dependencies were wanted.
- **Results appended in call order** regardless of completion order —
  reordering changes the prompt and breaks caching.
- **Tool errors are results, not failures.** An erroring tool produces a
  `ToolResultBlock{IsError: true}` and the loop continues. Only transport or
  context errors abort; they surface as a notice plus `TurnEnd{Err}`.

## Steering and follow-up

Two kinds of mid-turn input:

- **`Steer`** is injected into the running turn's context at the next step
  boundary (a message submitted mid-turn does not cancel the in-flight model
  call). It returns false when idle, so the caller knows to start a new turn.
- **`FollowUp`** queues input as a separate turn once the current one settles.

Both wait for the loop boundary; neither interrupts the stream. `Interrupt`
drops everything queued and cancels the running turn.

## The transcript stays well formed

Every cancelled or completed turn leaves `State.Messages` with each
`ToolCallBlock` matched by a `ToolResultBlock`. This invariant is what keeps the
next request valid, and it is asserted directly in the abort tests (`wellFormed`
checks that tool calls balance tool results).

## Concurrency rules

- `State.Messages`, `Reasoning`, `Model`, `Tools` are owned by the loop goroutine.
- The queue and `running` flag live under `a.mu`.
- All control methods (`Steer`, `FollowUp`, `Interrupt`, `Running`) take
  `a.mu`. They never block on a stream; they only mutate the queue or cancel.

## Testing

The suite follows the existing bar (90% coverage, no `time.Sleep`):

- Loop tests against `llm.ScriptedProvider`: text-only turn, one tool call,
  three parallel calls, tool error, unknown tool, step-limit trip, provider
  error mid-stream and before events. Assert result order matches call order.
- Abort tests: cancel between streamed events, cancel during tool execution,
  cancel with two unanswered parallel calls — assert every `ToolCallBlock` has a
  matching `ToolResultBlock` and `TurnResult.Stop == StopAborted`.
- A recording sink asserts the exact call sequence including that `EndThinking`
  precedes the first `Text`.
- Steering / follow-up tests pin the turn in flight with a blocking stub tool,
  then assert the queued input lands at the next boundary.
- `assemble` and `buildSystem` tests cover purity, cache stability across days,
  and silent git failure.
