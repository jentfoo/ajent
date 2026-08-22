# Agent loop design

How `pkg/agent` turns a model provider into an interactive coding agent: the
prompt -> stream -> tool-call -> repeat loop, the event sink any front end can
adapt onto, and interruption as a first-class operation. It is the reference for
the turn loop; tools, sessions and compaction all build on it.

## Shape

`Agent` owns one `State`, which is the in-memory projection of the session: it
holds the transcript messages, the model requests default to (and `/model`
switches), the reasoning configuration and the active tool names in declaration
order.

`State` is owned by whatever goroutine runs a turn. The only fields guarded by
the agent's mutex are the queue and the running flag — never `Messages`, so the
loop can append without contending with steering. Everything else on the agent —
the pending steer/follow queues and the interrupt cancel func — sits under that
same lock.

`Prompt(ctx, input)` runs `input` to completion including any follow-up queued
while it ran, and returns when both queues are empty or the context ends. It is
single-owner: only one goroutine drives turns at a time (in `cmd/ajent`, that is
the main message loop). Mid-turn input goes through `Steer` / `FollowUp`, never
a concurrent second `Prompt`.

## The sink, not the UI

The agent emits events on a `Sink`; it never imports the TUI and the TUI never
imports it. That is what makes headless mode, a sub-agent and tests all possible
with one loop.

A sink receives one callback per thing the user might want to watch: turn start,
a submitted prompt's text, streaming thinking and reply deltas (each with an end
marker), tool starts plus their incremental output and progress while a call is
still being composed, rendered diffs, usage and context-state reports, notices,
and the final `TurnEnd` outcome.

`cmd/ajent` provides a `tuisink` that maps these almost 1:1 onto `tui.UI`.
`NopSink` discards everything; a headless child agent uses it as an embedded
base and overrides only the events that feed its activity row.

### The one-shot drain

`-p` proves the seam: `oneshot_sink.go` is a second front end over the same
loop, chosen instead of `tuisink` before `tui.New` is ever called. `textSink`
writes each `Text` delta straight through to stdout — holding whitespace back
until content follows, so the blank block a model emits before a tool call never
reaches the terminal — and puts tool progress and notices on stderr, so the two
streams stay separable; `jsonSink` buffers a block and writes one JSON object
per line. Both embed `NopSink` and both hold a mutex around their writer,
because `ToolStart`, `ToolOutput`, `Diff` and the done closure can all fire from
parallel tool goroutines.

`textSink` also shows what a partial front end gets from `ToolProgress`: the
loop has already resolved each call's target argument by name, so the drain can
name a tool whose own `Label` is just its name, without re-parsing arguments the
tool owns.

Two constraints a non-TUI drain has to respect:

- **`Prompt` returning nil does not mean success.** An aborted turn sets
  `TurnResult.Stop = llm.StopAborted` and returns nil, so the outcome must be
  read off `TurnEnd` — the drain doubles as the `turnRecorder` for that.
- **The final answer is not a sink event.** The sink sees text deltas per block,
  not which block was last. The answer comes off `State.Messages` once the loop
  is idle, the same way `pkg/subagent` reads a child's summary. A streaming
  front end therefore prints prose from the sink and reads state only to decide
  the exit code — printing both would double the answer.

### Tool-call progress

`ToolProgress` reports a call the model is still *composing*, before it can run.
Without it a large `write` is silent for as long as its content streams — the
loop's only tool-visible event was `ToolStart`, which fires after the arguments
are complete. `forward` folds `EventToolCall{Start,Delta,End}` into a
`toolProgress` tracker and reports the argument bytes and lines accumulated so
far, which the TUI renders as an activity row.

Constraints that keep it honest:

- Arguments stream as **partial JSON**, so a newline inside an argument is the
  two-character `\n` escape, not a byte. `lineEscapeCounter` counts those escapes
  and carries its escape state across fragment boundaries, so a chunk may end
  mid-escape and `\\n` is not miscounted as a newline.
- Events are paired by **block index**, not by id: providers repeat `ToolCallID`
  on some events but always carry `Index` (see `event.go`). Resolving on id first
  and index second keeps parallel calls apart either way.
- The target is looked up **by argument name** (`path`, `file_path`, …), never by
  position: a marshalled Go map sorts its keys, so a write's long `content`
  commonly streams ahead of its `path`. It stays empty until a complete value has
  arrived.
- Republishing is throttled by an argument-byte step, not a timer, so the row
  cannot repaint per token and the behaviour stays deterministic under test.
- A stream that ends with calls still in flight (an interrupt) reports `Done` for
  each, so an aborted turn never strands a row.

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
    for step := 0; maxSteps <= 0 || step < maxSteps; step++ {
        drain push-steers, then OnBoundary inputs         (step boundary)
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
rewrites the list and hands it to the next. It never mutates `State`. The chain
is an extension seam nothing registers today — compaction records a `Reduce` plan
replayed on rebuild, and the plan workflow switches branches rather than
projecting, so what the model sees is always what the transcript holds.

### System prompt stays cache-stable

`buildSystem(state, env, proj, snippets)` builds one cache-stable system block — a
domain-neutral opening sentence (no tool or domain claims), guidelines some of which are
derived from the enabled tools, environment facts, project instructions and caller
snippets — where only a day-granular date changes between requests (plus instruction
reloads and tool-set changes), so the provider's prompt cache survives. Composition and
wording are specified in `prompt-design.md`.

### Fixed-overhead accounting (`base`) is replaced, not accumulated

The system block plus tool schemas ride with every request but carry no provider
report of their own. The ledger holds that as a single **replaced** bucket:
`SetBase(est)` overwrites it (never adds), and `Context()` folds it in only while
there is no exact prompt report — once `promptExact` lands the base drops out,
since an exact count already includes system and schemas. Replacing rather than
accumulating keeps a fresh epoch from double-counting across steps.

Two callers seed it: `Agent.BaseEstimate(tools bool)` exposes what rides along so
the front end can paint an honest bar before the first turn (system-only at
startup, tool schemas joining on the first prompt), and `stream()` itself calls
`SetBase(EstimateFixed(req))` with the real built request — which self-corrects
whatever was seeded. A separate **submitted** bucket (`SetSubmit`) carries a sent
prompt across the gap between the editor clearing and its message landing in
state; `Input.Delivered` clears it once `pending` owns the text, so it is never
counted twice.

### Step limit

A runaway tool loop is bounded by the context window and compaction, not an
arbitrary cap: the default is **unlimited** (`Options.MaxSteps <= 0`, the zero
value), so a legitimate long turn is never cut short. A positive `MaxSteps`,
when set, ends the turn cleanly with a notice once hit, not as an error. The
cap is configurable: the `agent.maxSteps` config key (see config-design.md) is
read once at startup; absent or non-positive means unlimited.

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
  `ctx.Done()`, via the shared `llm.CloseOnDone` helper. Close abandons buffered
  events rather than draining them, so in-flight tokens are dropped at the boundary
  instead of being flushed out — the goal is zero incoming tokens after an interrupt.
- Tools receive the cancelled turn context; a tool that observes cancellation (e.g.
  `bash`) records its partial output as an interrupted **error result** beginning
  `interrupted by user` in call order. `abortResults` keeps those real results over
  the synthetic ones it fills in for calls that never returned.
- An overflow-compaction retry runs under the turn's own context, so an interrupt
  stops its model call and the turn ends `StopAborted` rather than surfacing
  `context canceled` as a failure. (Threshold-boundary compaction keeps the outer
  context and is not interruptible by decision.)
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
tool implementation and fully testable. A `Tool` declares its schema (what the
model may call), whether it may run in parallel with siblings, and executes a
call against an output writer that streams incremental tool output; the loop
forwards those bytes to `sink.ToolOutput(callID, delta)`. The agent reads tools
through a narrow view that resolves one by name and returns the full schema set,
so the registry stays behind one seam.

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
- **A tool may end the turn** by returning `ToolResult{EndTurn: true}`: results
  are appended as usual and the loop stops with `StopEndTurn` instead of
  streaming another reply. It is how a control tool hands a phase to another
  model (see `plan-design.md`). Two rules make it safe. The signal rides on the
  **result**, so a tool that was never reached — denied by a guard, given bad
  arguments — cannot silence the model; and `runTool` reports
  `EndTurn && !IsError`, so a *rejected* control call always leaves the model
  free to correct itself in the same turn. An earlier attempt keyed this off a
  marker interface checked before `Execute` ran, and a rejected call silently
  ended the turn.

## Steering and follow-up

Two kinds of mid-turn input:

- **`Steer`** is injected into the running turn's context at the next step
  boundary (a message submitted mid-turn does not cancel the in-flight model
  call). It returns false when idle, so the caller knows to start a new turn.
- An `Input.Delivered` hook fires once its steer actually lands in `State`, so a
  sender that must confirm delivery (the sub-agent notifier) can clear pending
  ids only on real arrival; one dropped by an interrupt is re-offered. Nil is the
  normal case.
- An `Input.Injected` flag marks system-provided context (sub-agent completion
  steers, permission-barrier notes) rather than a typed prompt. It rides onto the
  appended `MessageInfo` and into the transcript so prompt recall (Ctrl+R / up
  arrow) can exclude it; injected messages still appear in assembled context.
- An injected steer with visible text is echoed to the sink (`UserPrompt`) when it
  lands — it has no submission echo, unlike typed prompts. Tool-result folds render
  through their own path.
- **`FollowUp`** queues input as a separate turn once the current one settles.
- **`Options.OnBoundary`**, when set, is called on the loop goroutine at each
  step boundary just before the next model call, after push-steers have been
  drained. Returned inputs are appended as user messages at this same boundary,
  so a host can hand over queued prompts with zero extra step of latency and no
  drop window (append follows synchronously). It must be cheap and never block;
  nil disables it. `Input.Delivered` still fires per returned input, exactly as
  for push-steers.

Both wait for the loop boundary; neither interrupts the stream. `Interrupt`
drops everything queued (including any host inputs already handed over at a
boundary) and cancels the running turn.

### Message observers and settled notifications

`Options.OnMessage` is a slice of callbacks, each invoked on the loop goroutine
per appended message in registration order — so more than one feature can watch
the transcript without displacing the session recorder (which keeps its own slot).
The turn boundary needs no new hook: `Sink.TurnEnd` already marks it, and sink
fan-out makes that event available to a second consumer.

A front end that needs the boundary reached on **errored** turns too — the plan
workflow's implementor-retry rule does — hooks the driver's own drain loop rather
than the agent: `main.go` consults its `planHooks.advance` after every
`ag.Prompt` return, reading the last `TurnResult` from a `turnRecorder` sink
(an `agent.NopSink` embed overriding `TurnEnd`). The agent is idle at that point,
so `WithState` is legal and a returned input continues the same drain loop with
no extra latency.

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
- A child agent is a separate `Agent` with its own single-owner loop:
  one goroutine per job, each owning its fresh `State`. The parent and its
  children never share an `Agent`, so there is no cross-loop ownership to reason
  about — the only shared surface is the child's ledger rolling into the parent's
  via `Accounting.Child()`.

## Child agents

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
