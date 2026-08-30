# Research sub-agents

How `pkg/subagent` fans expensive read-only investigation out of the main
context into throwaway child agents whose *only* return value is a final summary
paragraph, why that keeps context small enough that compaction stays simple, and
the invariants (structural tool filtering, no idle turns, delivery confirmation)
that keep it safe.

## What it is

The main agent can delegate a focused question to an isolated child: the model
calls `agent_start`, keeps working (reading files, running tools), then blocks on
`agent_poll` for the summary. Findings enter the main context as one paragraph per
investigation instead of fifty tool results — the single biggest context-efficiency
win available, and why compaction can stay simple.

Each job is a fresh headless `pkg/agent.Agent`: its own `State`, model and ledger,
an in-memory session (no transcript), a filtered read-only tool set, and an
activity-row sink. The parent registers three parallel-safe tools —
`agent_start`, `agent_poll`, `agent_list` — and surfaces live rows above the prompt.

## Boundary rules

The dependency edge is load-bearing: **`pkg/subagent ↛ pkg/tools`, `pkg/tui`,
`pkg/command`, `pkg/session`, `pkg/permit`.** It imports only `agent`, `llm`,
`strutil` and `tokens`.

- The parent tool registry arrives as a narrow interface (`ToolSource`) declared
  here, so the package never imports `pkg/tools`.
- Every UI surface (activity rows, notices, status segment) arrives as func-typed
  `Options` fields supplied by `main.go`, exactly like `permit.Barrier` — so
  headless mode stays free and no TUI import is needed.
- There is **no permission guard on a child**: nothing to permit. That is why the
  read-only tool filtering must be structural (see `toolset.go`) rather than
  advisory.

A sub-agent never spawns a sub-agent: `agent_*` is barred structurally, applied
last in the filter so no configuration can reach past it.

## The package

```
manager.go      Options, Manager — lifecycle, concurrency, notification, status segment
job.go          Status enum, public Job snapshot, internal locked job per investigation
run.go          child agent construction + the summary contract / empty-summary retry
tools.go        agent_start / agent_poll / agent_list (ModeParallel)
toolset.go      ToolSource interface + the structural read-only filter and fixed view
sink.go         childSink — one activity row per job, coalesced, lives as long as the job
prompt.go       childContract / continueNudge verbatim constants, taskPrompt assembly
```

### `manager.go` — lifecycle

The manager is configured by func fields supplied by `main.go`, mirroring
`permit.Barrier`: how to build a provider and resolve the child model/reasoning,
the parent ledger (for per-job `Child()` spend), the read-only tool source, env
and project instructions; UI callbacks for activity rows, keyed notices and the
status segment; a delivery hook that steers into a running parent turn; and the
concurrency cap plus poll timeout. Its surface is lifecycle operations: construct,
start (returning an id immediately), poll by id (`false` while still running),
list, stop one or all, re-offer pending completion steers, expose the three tools,
and close.

Concurrency model:

- **One goroutine per job**, a cancellable context per job, and a buffered-channel
  semaphore sized `MaxConcurrent`. A queued job waits in `StatusQueued` until it
  takes a slot; `Start` returns the id immediately.
- The `sync.WaitGroup.Add(1)` happens **before** the spawn goroutine launches so
  `Close`'s wait never races a pending add. `wg.Done`, closing `j.done` and
  releasing the semaphore all run in one defer on the job goroutine.
- A mutex guards the `jobs map[string]*job` plus the delivery state — `pending`
  (completed ids awaiting a message), `inFlight` (ids a queued message names) and
  `noticeBatch` (completions since the last delivered steer) — while per-job fields
  (status, timestamps, summary, pollers) sit under a second per-job lock so polls
  read snapshots while the owning goroutine mutates them.

**Shutdown.** `Close` calls `StopAll`, then waits on the waitgroup with a short
bound (`closeWait = 2s`) before clearing activity rows and the status segment, so
a job stuck on a slow provider cannot block exit forever. An interrupted turn is
cheaper: `agent_poll` selects over `j.done`, its timeout timer and the caller's
context, so cancelling the tool call releases the poll immediately while the job
itself keeps running.

### `job.go` — one investigation

A job carries a status from a small enum — queued, running, done, error,
aborted — and the public snapshot callers read pairs that with an id (`sub-N`), a
shortened task label, start/end times, the summary paragraph and any terminal
error.

Ids are `sub-N` from a manager counter, handed out **in the order the model asked
for the agents**. `agent_start` is `ModeParallel`, so the dispatch goroutines would
otherwise race for the counter and the task submitted last routinely became `sub-1`.
`Manager.Reserve` runs from `agent.Options.OnToolBatch` — the loop calls it with one
step's calls in message order, before any of them runs — and reserves a number per
`agent_start` call id; `Execute` then claims its reservation by `ToolCall.ID`. A
start with no reservation (host-driven, or a call the batch never named) takes the
next number, and a new batch supersedes the previous one, so reservations left by an
interrupted turn are dropped and their numbers skipped. Because the id is also the
activity row's rank, the rows sort in submission order too.

The internal `job` carries the per-job
context/cancel/done channel, the child ledger (`*tokens.Accounting`, created at
Start so it is visible to poll payloads), and a `pollers int`. A finished job's
snapshot keeps its terminal state; jobs are never deleted from the map within a
session (they show up in `/agents` until shutdown).

### `run.go` — child construction and the summary contract

Each job builds a fresh agent:

- **State** — `agent.State{Model, Reasoning, Tokens}`. Model is `subagent.model`
  resolved through the registry when set, else inherited from the session —
  resolved at spawn so a `/settings` change applies to the next job. Reasoning is
  inherited verbatim: a user who dialled reasoning down meant it.
- **Ledger** — one child ledger per job via `Parent().Child()`, rolled into the
  parent as **child spend** (see accounting below). The parent's context bar never
  moves because a child ran.
- **Tools** — `&toolSet{tools: childTools(src)}` when a `ToolSource` is present,
  else no tools at all.
- **Options** — the shared provider, env and project instructions (same repo, same
  AGENTS.md), `SystemSnippets: []string{childContract}`, no recorder, no resume.

The prompt is `agent.Input{Text: taskPrompt(task, instructions)}`. After it
returns, the summary is read off the **last assistant message** in `State.Messages`,
joining only its non-empty `llm.TextBlock` content — thinking is excluded.

**Empty-summary retry.** A reasoning model whose final message is thinking-only
returns no text. When the summary is blank and the stop reason is neither error nor
aborted, re-`Prompt` with `continueNudge`, bounded to `maxContinueAttempts = 2`,
then return the placeholder `(sub-agent produced no output)` rather than looping.

**Abort.** An aborted context yields `StatusAborted`, never a partial summary
mistaken for a completed investigation, and never a completion notification — a job
the user stopped is not reported as done.

### `toolset.go` — the structural filter

The parent tool registry arrives as a narrow source with two operations: return
every declared tool **unwrapped** (a child runs no parent guards or dialogs), and
report whether a named tool is read-only.

A child's tool set is a fixed, structural subset of `Registry.All()`:

- Include when the name is one of the read-only built-ins (`read`, `grep`,
  `find`, `ls`) **or** `src.ReadOnly(name)` (MCP hints / config globs).
- Exclude unconditionally when the name starts with `agent_` — applied last, so
  nothing can configure a child into spawning grandchildren.
- Ignore parent enable state entirely: `find`/`grep`/`ls`, registered *disabled*
  by default in the parent, still reach a child that has no shell. `bash` is never
  included even when enabled — excluded **by policy**, not classification.

This filter, not the permission barrier or a prompt instruction, is what makes a
child read-only: there is no user at its end to approve anything else.

### `sink.go` — one activity row per job

`childSink` embeds `agent.NopSink` and overrides only the events that feed a row,
publishing through `Options.Activity(key, text)`. Nothing it emits reaches
committed history:

- `Start` publishes `<id>  <label>` immediately (see below), so a job is visible
  above the prompt while it is still queued on the semaphore, before its turn emits.
- `ToolStart(call, label)` publishes `<id>  <call name + first arg>`; a rich,
  multi-word provided label is kept whole, but built-in tools whose labels are bare
  words (`read`, `grep`) have their first argument appended so the row reads as real
  work rather than one token. Its done hook restores the prior line (thinking/idle
  fallback).
- `Thinking(delta)` and `Text(delta)` both accumulate streaming per-token deltas
  into the current in-progress line and publish `<id>  <one-lined text>` (capped
  at `maxBuf = 2048`, keeping only the head so a long stream never jumps its
  display to the tail) — most sub-agent activity is reasoning, so chain-of-thought
  is surfaced rather than collapsed. A single delta is one word or character,
  so replacing rather than appending would show no real progress; each scrolls per
  line (content after the last newline stays, completed lines fall off). Switching
  streams (thinking → text) starts fresh instead of appending prose onto leftover
  reasoning. A current line that is blank or whitespace-only after trim publishes
  nothing, so empty streaming never flashes. Deltas coalesce to one republish per
  `deltaFlush = 150ms` so streaming does not repaint per token.
- The row belongs to the **job**, not to a turn. `run` nudges a child that ended a
  turn without summary text, so `TurnEnd` only resets the streamed line back to the
  idle `thinking…` fallback; clearing it there would make a live, pollable job blink
  out of the list and then reappear at the end of it. Every terminal path in
  `Manager.spawn` is the single clear point — which also covers a job cancelled
  before it ever acquired its slot (no sink ran).
- Child tools are `ModeParallel`, so `ToolStart` counts calls in flight rather than
  capturing the previous row per call: an early finisher would otherwise wipe a
  sibling's label off the row. Only the last call out restores the idle line.

Rows are single lines with no width maths — `tui.SetActivityRanked` elides to width
and never wraps, capped at `maxActivityRows = 3` plus a `+N more` indicator. Each
row is published with the job number as its rank, so the list reads sub-1, sub-2,
sub-3 for the life of the jobs however the parallel `agent_start` calls raced to
publish, and the `+N more` overflow always hides the newest rather than an
arbitrary one. They render
dim on a subtle background (`Theme.Activity`) so live work stands apart from the
prompt area above which they sit.

The completion steer is marked `Input.Injected`, and the permission-barrier note
steer in main.go is too, so neither surfaces as a recallable prompt via Ctrl+R or
the up-arrow history — only messages the user actually typed do.

### `tools.go` — the three tools

All three are `agent.ModeParallel`. Their `Label(call)` methods read the call's
arguments so the committed tool header names the target — `agent_start` shows its
task (first line, elided to `maxLabelLen`) and `agent_poll` shows the normalized id
— falling back to a bare verb when args do not parse. This distinguishes parallel
starts/polls in the transcript instead of repeating `sub-agent: start` N times.
Their descriptions state the contract up front,
because a model that learns it by trial burns a round trip each: no session context
(pass file paths and key facts, not content), read-only (`read`, `grep`, `find`,
`ls` plus read-only MCP tools; anything needing write/edit/shell must be done
directly), final message is the entire return value.

- **`agent_start(task, instructions?)`** returns a job id immediately and tells the
  model to poll for it. Several may run in one batch.
- **`agent_poll(id)`** blocks until completion or `PollTimeout`. On completion it
  returns the summary (or error / `aborted`). **On timeout** it reports still-running
  *plus* elapsed and the child's context usage against its model window, read from
  the job ledger — a poll that only says "still running" gives the model nothing to
  act on. Accepts `sub-2` or bare `2`. Every outcome also carries
  `Details{"id","status"}` — invisible to the model, and the only supported way a
  host-driven poller tells a timeout from a terminal result. Matching the payload
  prose is not a contract; the status is.
- **`agent_list()`** returns id/status/elapsed rows, or `(no sub-agents)`.

All three set `ToolResult.Display` to the same text as `Content`, so history shows
the payload through the shared output-head rule (`tui-design.md`) instead of a bare
tool header: a poll timeout commits its elapsed/context line, a completed summary
gets head-plus-collapse treatment, and start/list render their rows.

One exception: `agent_poll` is `ModeParallel`, so a batch of polls commits every
`⏺ sub-agent: poll sub-N` header at dispatch and each payload only as its own job
finishes. The results then land in completion order, detached from the headers
naming them. `Manager.poll` reports whether a call shared its window with another
(`enterPoll`/`leavePoll` mark the whole overlapping group, and the mark clears once
the group empties), and `nameDisplay` heads a batched payload with
`sub-N results:`. Only the display copy is tagged — the model asked for that id and
reads `Content` — and the header costs one of the four `outputHeadLines`, so a
batched summary shows three lines before collapsing.

## Concurrency and notification

### Polling

`Poll` increments a per-job `pollers` count around the wait; an interrupted turn
releases it at once. The count is what suppresses the completion steer: if a poller
is already waiting, the result rides the poll response back and a duplicate context
message would waste tokens.

Two details keep that suppression from swallowing a result:

- The wait selects over `j.done`, the timeout and the turn context, and Go picks
  uniformly among ready cases. A job finishing in the same instant the timer fires
  would otherwise be reported as still running with its summary already in hand, so
  the timeout branch re-checks `j.finished()` and returns the result when it has one.
- `pollers > 0` means *a poll will carry this*, not *a poll did*. A poll that leaves
  empty-handed — its timer fired, or the turn was interrupted — may already have
  caused `onComplete` to skip the enqueue. So the deferred decrement re-checks: the
  last poller out with `consumed` still false calls `onComplete` again, which
  no-ops unless the job actually finished (`job.finished`). `enqueue` is idempotent
  on both `pending` and `noticeBatch` so the recovery can never double-name an id.

### A host as the poller

`/init` (see `command-design.md`) drives `agent_start` and `agent_poll` itself
rather than letting a model call them. Two consequences a caller must respect:

- **Register every poll before any child can finish.** Suppression of both the
  completion notice and the context steer is `pollers > 0` — it is not a mode.
  `/init` therefore issues all its starts, then polls every id concurrently. The
  window between the last `Start` and the first `Poll` is a narrow race, not a
  guarantee: a job finishing inside it still notifies and steers.
- **Poll until terminal.** A timeout is an ordinary outcome, so the caller loops on
  `Details["status"]` and keeps only the terminal pair, or the transcript
  accumulates still-running noise.
- **Cancel by id, not `StopAll`.** A host survey runs for minutes, during which the
  user may start a turn whose model spawns investigations of its own. `/init`
  records the ids it started (`projinit.Options.Started`) and stops exactly those,
  so aborting the survey never kills the model's work.

### Completion when nobody is polling

A job that finishes while no poll waits must still reach the model, or the work is
wasted. Delivery is **batched and decided late**: completions only accumulate;
the message naming them is built at the moment it lands, never before. Three
pieces:

- **UI notice** — fired at completion (`Options.Notice`), keyed by the front end
  (`NotifyKeyed`) so consecutive notices collapse in place rather than stack. The
  text names every completion since the last delivered steer, so a fan-out reads
  as one updating line (`Sub-agents sub-1, sub-2, sub-3 completed`), not one row
  per agent. Suppressed for aborts.
- **Context steer at a step boundary** — completed ids park in `pending`; the
  host chains `Manager.Boundary()` into `agent.Options.OnBoundary`, so on the
  loop goroutine — at the exact moment `drainSteer` appends the message — the
  manager filters `pending` down to ids with no poller and no consumption and
  returns **one** `Input` naming them all. This late decision is what prevents
  duplicate alerts: a batch that accumulated while the model streamed is
  re-checked when it lands, so ids the model already retrieved with `agent_poll`
  in the meantime are dropped and never named again.
- **Context steer at turn start** — `Manager.Flush()` offers pending ids through
  the `Options.Deliver` hook for completions that landed while no turn ran (the
  boundary hook only fires mid-turn). The front end's `Deliver` returns false
  when the parent is idle; ids stay pending.

Marks are per id: a boundary emits whatever is not already spoken for, so no
single stuck mark can block unrelated ids. `agent_poll` claiming a result sets
`consumed` and drops the id from every delivery list — `pending`, `inFlight` and
`noticeBatch` — so neither channel names it afterwards.

**Delivery confirmation.** `Input.Delivered` fires only when the steer actually
lands in context (appended to `State.Messages`), so it clears exactly the ids
that message named and starts a fresh UI notice batch. A steer dropped by an
interrupt never fires it: the front-end sink's `TurnEnd` reacts to a turn ending
`llm.StopAborted`, calling `Manager.Interrupted()` so the in-flight marks release
and the next `Flush` can re-offer — delivery is confirmed, not assumed.

**Never start a turn on an idle agent.** An agent that talks to the user unprompted
is a bug. `Boundary` runs only inside a live turn's `drainSteer`, and the front end
wires `Deliver` as `if !ag.Running() { return false }; return ag.Steer(in)`, and
deliberately does **not** use `OnSettled` (which would let a steer append inside
the settled window). Pending completions reach the model on the next *real* turn
via `Flush()` called from a turn-start observer sink: at that point `running` is
true, so the steer queues and lands at step 1 without ever starting a turn.

### Status segment

Recomputed on every transition — full form `subagents: N running (oldest Xs), M done`,
short form `sub N`, cleared when no jobs exist. The front end renders it as a keyed
`SetStatusSegment({Key:"subagents"})`.

## Accounting and configuration seams

### Child spend (`pkg/tokens`)

A child's ledger is created via the parent's `Accounting.Child()`; every job rolls
its usage into the parent through the existing `rollUp`. Two invariants:

- **`Total()` stays inclusive** — session spend counts children too.
- **`Context()` never moves for a child** — `rollUp` touches only totals, never
  context terms, so a fan-out of investigations does not corrupt the main context bar.

The delegated subset is exposed as `Accounting.ChildTotal()`, which `/usage` reads to
show what delegation cost; the per-model table already splits spend when the child
model differs.

### Configuration (`pkg/config`)

The `subagent` block lives in `config-design.md`: `model` (empty inherits the
session model) and `maxConcurrent` (compiled-in default from `pkg/config`), both
bound for free through env
reflection. Here it sizes the manager's semaphore; `/settings` edits it.

## Front-end wiring (`main.go` / `console.go`)

The only wiring layer. The manager is built in `driver` after the tool registry and
barrier exist, with adapters:

- `Activity` → `ui.SetActivity`
- `Notice` → `ui.NotifyKeyed("subagent", msg, LevelInfo)`
- `Status` → `ui.SetStatusSegment({Key:"subagents"})`
- `Deliver` → the `Running()`-guarded `ag.Steer`, per above
- `Boundary` → chained ahead of the steer queue's `q.pull` in
  `Options.OnBoundary`, so completion steers and queued user prompts land at the
  same step boundary with the injected notice ahead of the user's direction
- A tiny sink (`NopSink`) appended to `opts.Sinks` overrides `TurnStart` to call
  `mgr.Flush()` and `TurnEnd` to release in-flight marks whenever the turn ends
  `llm.StopAborted`, so every interrupt path — Esc/Ctrl+C, the plan controller's
  `/plan-stop` abort, headless — clears them through one seam instead of each call
  site remembering

Headless mode (`oneshot.go`) has no steer queue, so `Options.OnBoundary` is
`mgr.Boundary` directly and the `Deliver` hook drives the idle case.

The three tools are registered under source `builtin`, enabled by default, before
the permission barrier's guard attaches (so `/tools` ordering matches other sources),
and marked read-only so allow-read mode runs them free — the parent *calling*
`agent_start` is itself a non-mutating act. They are collapsed into one toggleable
`/tools` row via `Registry.RegisterGroup`: a single `subagents` label (grouped with
the builtins, ahead of MCP) enables or disables all three at once — see the registry
grouping in `tools-design.md`. A `defer sag.Close()` sits beside the MCP close.

**Esc semantics.** Esc interrupts the *turn*, releasing any in-flight poll; jobs keep
running because they are independent investigations and re-running them is expensive.
Cancelling jobs is `/agents stop [id|all]` only — there is no second-Esc-cancels-jobs
gesture.

## The prompt contract (`prompt.go`, `childContract`)

Every child gets a fresh system block built by the same cache-stable `buildSystem`,
with one extra snippet appended after project instructions via
`agent.Options.SystemSnippets`: `childContract`. It states the read-only constraints
(structural — the tool set is filtered before the model ever sees it) and that the
final assistant message **is** the entire return value. The text is quoted verbatim,
and owned, by `prompt-design.md`; there is deliberately no "Available tools" list in a
child's system block (the schema channel carries it).

## Testing

Per-file `_test.go`, table-driven, `llm.ScriptedProvider` throughout, no
`time.Sleep`. The bar matches the rest of the tree:

- **Lifecycle** — completes / errors / aborted / timeout-then-complete polling.
- **Concurrency** — eight starts against a semaphore of four; an atomic counter proves
  never more than four run at once and all complete. `Close` cancels running jobs and
  returns promptly.
- **Id order** — three `agent_start` calls in one message, dispatched in parallel
  through a real `agent.Agent`, number their jobs sub-1/sub-2/sub-3 in submission
  order; reservations claim out of order, ignore other tools, fall back to the next
  number when unreserved, and are dropped by the following batch.
- **Tool-set enforcement** — `agent_start` is absent from a child's resolved tools even
  when the source reports it read-only; `bash`, `write`, `edit` are unreachable;
  `find`/`grep`/`ls` reach despite being disabled in the parent registry.
- **Poll timeout payload** — elapsed and context usage against the child model window.
- **Batched poll display** — two overlapping polls each head their display copy with
  `sub-N results:` while `Content` stays bare; a lone poll is untagged; the group
  mark does not leak into a later lone poll.
- **Notification** — a poll in flight suppresses the steer; with no poller a steer is
  delivered; a false `Deliver` leaves ids pending and later `Flush` re-offers;
  `Delivered` clears exactly the named ids. An idle parent never starts a turn.
  Batching: completions merge into one boundary input and one accumulating keyed
  notice; ids polled before the boundary are never named; an interrupt releases
  the in-flight marks and the next `Flush` re-offers. A poll that both times out
  and finds the job finished returns the result; a poller that departs empty-handed
  after the job completed re-arms delivery, and `onComplete` on a still-running job
  queues nothing.
- **Empty summary** — nudge → summary; two empty nudges → placeholder.
- **Activity rows** — one per job, coalesced, published with the job number as its
  rank and cleared only at completion (a nudge turn must not drop it), never in
  history. Overlapping child tool calls restore the idle line once, when the last
  one ends.
- **Accounting** — child spend appears in `ChildTotal()` and the parent's `Total()`;
  the parent's `Context()` is unchanged by a child.

## Invariants

1. A child has only read-only tools; `agent_*` (and so grandchildren) are impossible
   by construction, not instruction.
2. Nothing from a child ever reaches committed history — its sink feeds activity rows
   only.
3. Completion delivery is confirmed: ids clear only when the steer lands, and one
   dropped by an interrupt is re-offered. A completion message never names an id
   a poll already delivered — membership is decided when the message lands.
4. A completion never starts a turn on an idle parent; `OnSettled` is unused for this.
5. Child spend rolls up into the parent's totals but never moves its context bar.
6. `Close` cancels everything and returns promptly even if a provider stalls.
