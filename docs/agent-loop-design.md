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
`NopSink` discards everything; a headless child agent uses it as an embedded
base and overrides only the events that feed its activity row (phase 13).

### Sink fan-out

`Options.Sinks` is a slice; more than one consumer can watch turns without
displacing the recorder. `New` resolves it once into a single field (`a.sink`):
no sinks become `NopSink`, one sink is used as-is, and two or more are wrapped
in a `fanoutSink` that forwards every method to each member in registration
order. `ToolStart` returns a closure that calls each member's done in order.
The loop therefore always emits on one field; fan-out costs nothing at the hot
path.

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

`assemble(state, transforms)` returns the message list for one request as a pure
function of `State`: each transform in the ordered chain (nil entries skipped)
rewrites the list and hands it to the next. It never mutates `State`. Compaction
and plan projection both work by transforming this assembled list rather than
rewriting `State`, so they coexist by registering two transforms.

### System prompt stays cache-stable

`buildSystem(state, env)` builds one cache-stable system block — identity,
guidelines, environment facts — where only a day-granular date changes between
requests, so the provider's prompt cache survives. Composition and wording are
specified in `prompt-design.md`.

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

The invariant behind the synthetic results: every cancelled or completed turn
leaves `State.Messages` with each `ToolCallBlock` matched by a
`ToolResultBlock`, which is what keeps the next request valid.

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
- An `Input.Delivered` hook fires once its steer actually lands in `State`, so a
  sender that must confirm delivery (the sub-agent notifier) can clear pending
  ids only on real arrival; one dropped by an interrupt is re-offered. Nil is the
  normal case.
- **`FollowUp`** queues input as a separate turn once the current one settles.

Both wait for the loop boundary; neither interrupts the stream. `Interrupt`
drops everything queued and cancels the running turn.

### Message observers and settled notifications

`Options.OnMessage` is a slice of callbacks, each invoked on the loop goroutine
per appended message in registration order — so more than one feature can watch
the transcript without displacing the session recorder (which keeps its own slot).
The turn boundary needs no new hook: `Sink.TurnEnd` already marks it, and sink
fan-out makes that event available to a second consumer.

`Options.OnSettled` is a slice of callbacks invoked on the loop goroutine once
the queues are drained, nothing is running, and the last turn did not error. They
run after the compaction hook at each real boundary; an observer that queues work
(such as another `Prompt`) keeps the same outer `Prompt` alive because `runTurns`
re-reads the queues before returning. An observer that always queues loops
forever, exactly like a self-queueing follow-up does today.

## Concurrency rules

- `State` is owned by the loop goroutine; the queue and `running` flag live
  under `a.mu`.
- All control methods (`Steer`, `FollowUp`, `Interrupt`, `Running`) take
  `a.mu`. They never block on a stream; they only mutate the queue or cancel.
- `Options.SystemSnippets` are extra system blocks appended after project
  instructions, an explicit input to `buildSystem` so sub-agents can inject their
  contract without touching cache-stable composition. Empty keeps the block
  byte-identical.
- A child agent (phase 13) is a separate `Agent` with its own single-owner loop:
  one goroutine per job, each owning its fresh `State`. The parent and its
  children never share an `Agent`, so there is no cross-loop ownership to reason
  about — the only shared surface is the child's ledger rolling into the parent's
  via `Accounting.Child()`.

## Child agents (phase 13)

The main agent can fan read-only investigation out to throwaway children whose
only return value is a final summary paragraph, so findings enter context as one
message instead of fifty tool results. A child is just another `Agent` with its
own single-owner loop; the full contract — fresh `State`, in-memory session,
structurally filtered read-only tools and an activity-row sink — lives in
`subagents-design.md`. The ownership rule that matters here is stated above: a
child never shares this agent, so there is no cross-loop state to reason about.

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
