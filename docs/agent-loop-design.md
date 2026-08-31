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
the agent's mutex are the queue and the running flag, never `Messages`, so the
loop can append without contending with steering. Everything else on the agent,
including the pending steer/follow queues and the interrupt cancel func, sits under that
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
writes each `Text` delta straight through to stdout, holding whitespace back
until content follows so the blank block a model emits before a tool call never
reaches the terminal. It puts tool progress and notices on stderr instead, so
the two streams stay separable. `jsonSink` buffers a block and writes one JSON
object per line. Both embed `NopSink` and both hold a mutex around their writer,
because `ToolStart`, `ToolOutput`, `Diff` and the done closure can all fire from
parallel tool goroutines.

`textSink` also shows what a partial front end gets from `ToolProgress`: the
loop has already resolved each call's target argument by name, so the drain can
name a tool whose own `Label` is just its name, without re-parsing arguments the
tool owns.

Two constraints a non-TUI drain has to respect:

- **`Prompt` returning nil does not mean success.** An aborted turn sets
  `TurnResult.Stop = llm.StopAborted` and returns nil, so the outcome must be
  read off `TurnEnd`. The drain doubles as the `turnRecorder` for that.
- **The final answer is not a sink event.** The sink sees text deltas per block,
  not which block was last. The answer comes off `State.Messages` once the loop
  is idle, the same way `pkg/subagent` reads a child's summary. A streaming
  front end therefore prints prose from the sink and reads state only to decide
  the exit code, since printing both would double the answer.

### Tool-call progress

`ToolProgress` reports a call the model is still *composing*, before it can run.
Without it a large `write` is silent for as long as its content streams. The
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
      (Before, the text, Delivered, After resolves, then Settled)
    for step := 0; maxSteps <= 0 || step < maxSteps; step++ {
        drain push-steers, then OnBoundary inputs         (step boundary)
        req = request(state, env, tools)                 assemble()
        msg, usage = a.stream(ctx, req)                  forwards deltas to sink
        append msg to state.Messages
        if ctx cancelled -> StopAborted, end turn
        calls := toolCalls(msg); if none -> break (end_turn)
        results := dispatch(calls)                       OnToolBatch, then parallel or serial
        appendToolResults(msg, results)                  call order preserved
    }
    sink.TurnEnd
```

### Assembly is pure

`assemble(state, transforms)` returns the message list for one request as a pure
function of `State`: each transform in the ordered chain (nil entries skipped)
rewrites the list and hands it to the next. It never mutates `State`. The chain
is an extension seam nothing registers today. Compaction records a `Reduce` plan
replayed on rebuild, and the plan workflow switches branches rather than
projecting, so what the model sees is always what the transcript holds.

### System prompt stays cache-stable

`buildSystem(state, env, proj, snippets)` builds one cache-stable system block.
It contains a domain-neutral opening sentence (no tool or domain claims),
guidelines some of which are derived from the enabled tools, environment facts,
project instructions and caller snippets. Only a day-granular date changes
between requests (plus instruction reloads and tool-set changes), so the
provider's prompt cache survives. Composition and wording are specified in
`prompt-design.md`.

### Fixed-overhead accounting (`base`) is replaced, not accumulated

The system block plus tool schemas ride with every request but carry no provider
report of their own. The ledger holds that as a single **replaced** bucket:
`SetBase(est)` overwrites it (never adds), and `Context()` folds it in only while
there is no exact prompt report. Once `promptExact` lands the base drops out,
since an exact count already includes system and schemas. Replacing rather than
accumulating keeps a fresh epoch from double-counting across steps.

Two callers seed it: `Agent.BaseEstimate(tools bool)` exposes what rides along so
the front end can paint an honest bar before the first turn, and `stream()` itself
calls `SetBase(EstimateFixed(req))` with the real built request, which
self-corrects whatever was seeded. Tool schemas join the base only once the tool
block is **committed**, since `/tools` can still narrow the set before the first
prompt; after that it can only widen, so the base grows and never shrinks. A
resumed branch with history has committed one already, and every context-tree jump
(rewind, fork) re-seeds the base itself, because the ledger `session.State` builds
carries none of its own.

A separate **submitted** bucket (`SetSubmit`) carries a sent prompt across the gap
between the editor clearing and its message landing in state. `Input.Settled`, not
`Delivered`, clears it. `Delivered` fires ahead of `After` so a queued batch's
reads render under its echo, and releasing the reserve there would drop those reads
for the repaint between the two. `Settled` fires once everything the input carries
has been appended, so `pending` already owns it.

A **staged** bucket (`SetStaged`) is its counterpart for `!` shell output waiting
on a prompt that has not been typed yet. The stager reports its total as each run
finishes and again at `Flush`, where it drops to zero and the submitted bucket
takes over, so the same bytes are never counted twice. Like `composing`, it
survives `Rebase`, `Reseed` and `SetModel`: staged output has not ridden any
request, so nothing the provider reports has anything to say about it.

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
  instead of being flushed out, keeping zero incoming tokens after an interrupt.
- Tools receive the cancelled turn context; a tool that observes cancellation (e.g.
  `bash`) records its partial output as an **error result** marked
  `interrupted by user`, appended in call order. `abortResults` keeps those real
results over the synthetic ones it fills in for calls that never returned.
- An overflow-compaction retry runs under the turn's own context, so an interrupt
  stops its model call and the turn ends `StopAborted` rather than surfacing
  `context canceled` as a failure. (Threshold-boundary compaction keeps the outer
  context and is not interruptible by decision.)
- On abort the partial assistant message from the Accumulator is still appended,
  then every unanswered `ToolCallBlock` gets a synthetic error result carrying the
  same `interrupted by user` marker. Without this, a cancelled turn leaves a dangling
  `tool_use` and the next Anthropic request 400s. That is the single most common way an
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
- **Prompts force serial.** A `ToolSet` may implement the optional `Serializer`
  interface (`MustSerialize(calls)`); when it reports true for a batch, dispatch runs
  serially even though every tool is parallel. block-all asks read-only tools too,
  and their dialogs must open in submission order — racing them across goroutines
  would scramble prompt order against the calls' message order.
- **Bounded parallelism.** A semaphore channel and a `sync.WaitGroup` cap
  in-flight calls at the host's CPU count. An `errgroup` was considered but
  `golang.org/x/sync` is not in the module, and no new dependencies were
  wanted.
- **Results appended in call order** regardless of completion order. Reordering
  changes the prompt and breaks caching.
- **Tool errors are results, not failures.** An erroring tool produces a
  `ToolResultBlock{IsError: true}` and the loop continues. Only transport or
  context errors abort; they surface as a notice plus `TurnEnd{Err}`.
- **A tool may end the turn** by returning `ToolResult{EndTurn: true}`: results
  are appended as usual and the loop stops with `StopEndTurn` instead of
  streaming another reply. It is how a control tool hands a phase to another
  model (see `plan-design.md`). Two rules make it safe. The signal rides on the
  **result**, so a tool that was never reached, whether denied by a guard or given bad
  arguments, cannot silence the model. And `runTool` reports
  `EndTurn && !IsError`, so a *rejected* control call always leaves the model
  free to correct itself in the same turn. An earlier attempt keyed this off a
  marker interface checked before `Execute` ran, and a rejected call silently
  ended the turn.

### Host-run tool calls

Not every tool call comes from the model. An `@` reference and `/init`'s project
survey both run a real tool and place the result in context as though the model
had called it. `InjectPair(ctx, tool, sink, call, label)` is that one path: it
executes the call through `NewOutput(sink, callID)` so output streams
and renders exactly as an agent-run tool does, turns a Go error into an error
`ToolResult` rather than losing it, and returns the assistant call and user result
messages for `Input.Before` or `Input.After` alongside the raw result (whose
`Details` the caller may read). Having one implementation is what keeps truncation
markers, read tracking and display order identical across all three callers.

The **call id is the caller's** and must be unique for the life of the session:
the pair is appended to `State` and persisted, and once `tool_use` ids repeat
Anthropic rejects every later request, permanently. A caller that can
run twice in one session numbers its runs. `/init` uses a per-`Runner` counter,
while `@` expansion uses a per-`Expander` one that `refs.Expander.Seed` raises above
every reference id already in a resumed or rewound context.

A staged `!` line is **not** an `InjectPair` caller: it stages a user-authored
text message (the command and its output, see `prompt-design.md`) via `Input.Before`,
not a synthetic tool-call pair, so no unique-id contract applies to it.

## Steering and follow-up

Two kinds of mid-turn input:

- **`Steer`** is injected into the running turn's context at the next step
  boundary (a message submitted mid-turn does not cancel the in-flight model
  call). It returns false when idle, so the caller knows to start a new turn.
- An `Input.Delivered` hook fires once its steer actually lands in `State`, so a
  sender that must confirm delivery (the sub-agent notifier) can clear pending
  ids only on real arrival; one dropped by an interrupt is re-offered. Nil is the
  normal case. It fires **before** `Input.After` resolves. The host clears its
  submit bucket there, and running the reads first would leave them counted in
  both that bucket and pending.
- An `Input.Injected` flag marks system-provided context (sub-agent completion
  steers, permission-barrier notes) rather than a typed prompt. It rides onto the
  appended `MessageInfo` and into the transcript so prompt recall (Ctrl+R / up
  arrow) can exclude it; injected messages still appear in assembled context. A
  `MessageInfo.Replayed` mark opts one back into restored scrollback. Staged `!`
  runs set it, so a resume redraws what the model is still carrying.
- **`Input.Before` and `Input.After`** frame one input. Before is context that
  predates the message, such as staged `!` output or `/init`'s survey. It is a
  `[]MessageInfo` appended ahead of it, so each entry carries its own marks
  (`appendSteer` stamps `Injected` and preserves `Replayed`). After is context the
  message *asked for*: a
  `func(ctx) []llm.Message` resolved once the user message has landed, appended
  behind it. The asymmetry is deliberate. `session.RewindTarget` rewinds a user
  prompt to its parent, so anything ahead of the prompt survives a rewind onto it
  and anything behind it does not. An `@` reference must therefore be dropped and
  re-read when its message is re-sent, never left stale in context.
- Every `Input.Before` and `Input.After` message is appended with the **injected**
flag set: by definition they carry system-staged context (`@` reads, `/init`'s
survey, staged shell results), never a typed prompt, so recall excludes all of it.
- An injected steer with visible text is echoed to the sink (`UserPrompt`) when it
  lands. It has no submission echo, unlike typed prompts. Tool-result folds render
  through their own path.
- **`FollowUp`** queues input as a separate turn once the current one settles.
- **`Options.OnBoundary`**, when set, is called on the loop goroutine at each
  step boundary just before the next model call, after push-steers have been
  drained. Returned inputs are appended as user messages at this same boundary,
  so a host can hand over queued prompts with zero extra step of latency and no
  drop window (append follows synchronously). It must be cheap and never block;
  nil disables it. `Input.Delivered` still fires per returned input, exactly as
  for push-steers.
- **`Options.OnToolBatch`**, when set, runs on the loop goroutine at the top of
  `dispatch`, before any call runs, with one step's calls in message order and the
  turn's context (cancelled on abort). The parallel path races the calls against each
  other, so this is a host's only ordered view of a batch: it reserves ordered identity
  (`sub-N` numbering) and starts ahead-of-dialog work such as permission classification
  (`permit.Prefetch`, `prompt-design.md`). It must be cheap and never block; nil disables.

Both wait for the loop boundary; neither interrupts the stream. `Interrupt`
drops everything queued (including any host inputs already handed over at a
boundary) and cancels the running turn.

### Message observers and settled notifications

`Options.OnMessage` is a slice of callbacks, each invoked on the loop goroutine
per appended message in registration order, so more than one feature can watch
the transcript without displacing the session recorder (which keeps its own slot).
The turn boundary needs no new hook: `Sink.TurnEnd` already marks it, and sink
fan-out makes that event available to a second consumer.

A front end that also needs the boundary reached on **errored** turns, as the plan
workflow's implementor-retry rule does, hooks the driver's own drain loop rather
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
  about. The only shared surface is the child's ledger rolling into the parent's
  via `Accounting.Child()`.

## Child agents

The main agent can fan read-only investigation out to throwaway children whose
only return value is a final summary paragraph, so findings enter context as one
message instead of fifty tool results. A child is just another `Agent` with its
own single-owner loop, fresh `State`, in-memory session, structurally filtered
read-only tools and an activity-row sink. The full contract lives in
`subagents-design.md`. The ownership rule that matters here is stated above: a
child never shares this agent, so there is no cross-loop state to reason about.

## Testing

The suite runs against `llm.ScriptedProvider` (see `providers-design.md`), so no
network and no `time.Sleep`. The distinctive harnesses:

- A **recording sink** asserts the exact event call sequence, including that
  `EndThinking` precedes the first `Text`, and doubles as the abort-path observer.
- A **blocking stub tool** pins a turn in flight so steering/follow-up can be asserted
to land at the next boundary rather than cancelling it.

The invariants they protect are the loop's load-bearing ones: results append in call
order, and on any abort every `ToolCallBlock` gets a matching `ToolResultBlock` with
`TurnResult.Stop == StopAborted`. Purity (`assemble`) and cache stability across days
(`buildSystem`) are asserted directly.
