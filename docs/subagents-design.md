# Research sub-agents

How `pkg/subagent` fans expensive read-only investigation out of the main
context into throwaway child agents whose *only* return value is a final summary
paragraph, why that keeps context small enough that compaction stays simple, and
the invariants (structural tool filtering, no idle turns, delivery confirmation)
that keep it safe. This is phase 13's spec; `docs/phases/13-subagents.md` records
its staged progress.

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
`pkg/command`, `pkg/session`, `pkg/permit`.** It imports only `agent`, `llm` and
`tokens`.

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
sink.go         childSink — one activity row per job, coalesced, cleared on turn end
prompt.go       childContract / continueNudge verbatim constants, taskPrompt assembly
```

### `manager.go` — lifecycle

```go
type Options struct {
    Provider  func(llm.Model) (llm.Provider, error)
    Model     func() llm.Model           // configured sub-agent model, else the session's
    Reasoning func() llm.ReasoningConfig // inherited from the parent verbatim
    Parent    func() *tokens.Accounting  // parent ledger; Child() per job
    Tools     ToolSource                 // nil disables a child's tool set entirely
    Env       agent.Environment
    ProjectInstructions []agent.ProjectInstruction

    Activity func(key, text string)   // nil disables activity rows
    Notice   func(msg string)         // keyed UI notice for completions
    Status   func(text, short string)
    Deliver  func(agent.Input) bool   // steer into a running parent turn; false when idle

    MaxConcurrent int           // 0 -> defaultMaxConcurrent (4)
    PollTimeout   time.Duration // 0 -> defaultPollTimeout (10m)
}

func New(opts Options) *Manager
func (m *Manager) Start(task, instructions string) string
func (m *Manager) Poll(ctx context.Context, id string) (Job, bool) // false = still running
func (m *Manager) List() []Job
func (m *Manager) Stop(id string) error    // accepts sub-2 or bare 2; finished jobs error
func (m *Manager) StopAll() int            // cancels every in-flight job
func (m *Manager) Flush()                  // re-offer pending completion steers
func (m *Manager) Tools() []agent.Tool     // agent_start / agent_poll / agent_list
func (m *Manager) Close()                  // cancel everything, wait briefly
```

Concurrency model:

- **One goroutine per job**, a cancellable context per job, and a buffered-channel
  semaphore sized `MaxConcurrent`. A queued job waits in `StatusQueued` until it
  takes a slot; `Start` returns the id immediately.
- The `sync.WaitGroup.Add(1)` happens **before** the spawn goroutine launches so
  `Close`'s wait never races a pending add. `wg.Done`, closing `j.done` and
  releasing the semaphore all run in one defer on the job goroutine.
- A mutex guards the `jobs map[string]*job` and the `pending []string` completion
  queue; per-job fields (status, timestamps, summary, pollers) sit under a second
  per-job lock so polls read snapshots while the owning goroutine mutates them.

**Shutdown.** `Close` calls `StopAll`, then waits on the waitgroup with a short
bound (`closeWait = 2s`) before clearing activity rows and the status segment, so
a job stuck on a slow provider cannot block exit forever. An interrupted turn is
cheaper: `agent_poll` selects over `j.done`, its timeout timer and the caller's
context, so cancelling the tool call releases the poll immediately while the job
itself keeps running.

### `job.go` — one investigation

```go
type Status uint8 // queued, running, done, error, aborted

type Job struct {   // public snapshot for List and Poll callers
    ID      string  // sub-1, sub-2, …
    Status  Status
    Task    string  // shortened single-line label
    Started time.Time
    Ended   time.Time
    Summary string
    Err     error
}
```

Ids are `sub-N` from a manager counter. The internal `job` carries the per-job
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
joining only `llm.TextBlock` content — thinking is excluded. This mirrors
`compact.go`'s `runSummary`.

**Empty-summary retry.** A reasoning model whose final message is thinking-only
returns no text. When the summary is blank and the stop reason is neither error nor
aborted, re-`Prompt` with `continueNudge`, bounded to `maxContinueAttempts = 2`,
then return the placeholder `(sub-agent produced no output)` rather than looping.

**Abort.** An aborted context yields `StatusAborted`, never a partial summary
mistaken for a completed investigation, and never a completion notification — a job
the user stopped is not reported as done.

### `toolset.go` — the structural filter

```go
type ToolSource interface {
    All() []agent.Tool      // unwrapped tools; a child runs no parent guards or dialogs
    ReadOnly(name string) bool
}
```

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
  is surfaced rather than collapsed. A single delta is only one word or character,
  so replacing rather than appending would show no real progress. Each scrolls per
  line: content after the last newline is kept and completed lines fall off the
  row. Switching streams (thinking → text) starts fresh instead of appending prose
  onto leftover reasoning. The row shows the child's most recent
  actual output rather than a static label; deltas coalesce to one republish per
  A current line that is blank or whitespace-only after trim publishes nothing,
  so empty streaming lines never flash. Deltas coalesce to one republish per
  `deltaFlush = 150ms` so streaming does not repaint per token.
- `TurnEnd` clears the row, and every terminal path in `Manager.spawn` also clears
  it — covering a job cancelled before it ever acquired its slot (no sink ran).

Rows are single lines with no width maths — `tui.SetActivity` elides to width and
never wraps, capped at `maxActivityRows = 3` plus a `+N more` indicator. They render
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
  act on. Accepts `sub-2` or bare `2`.
- **`agent_list()`** returns id/status/elapsed rows, or `(no sub-agents)`.

## Concurrency and notification

### Polling

`Poll` increments a per-job `pollers` count around the wait; an interrupted turn
releases it at once. The count is what suppresses the completion steer: if a poller
is already waiting, the result rides the poll response back and a duplicate context
message would waste tokens.

### Completion when nobody is polling

A job that finishes while no poll waits must still reach the model, or the work is
wasted. Two channels:

- **UI notice** — `Options.Notice("Sub-agent sub-N completed")`, keyed by the front
  end (`NotifyKeyed`) so consecutive notices collapse in place rather than stack.
  Suppressed for aborts.
- **Context steer** — on completion, if no poller is waiting, the id joins
  `pending` and the manager calls `Options.Deliver(agent.Input{Text: notice,
  Delivered: confirm})`. The front end's `Deliver` returns false when the parent is
  idle; ids stay pending.

**Delivery confirmation.** `Input.Delivered` fires only when the steer actually
lands in context (appended to `State.Messages`), so `confirm` clears exactly the ids
that message named. One dropped by an interrupt stays pending and is re-offered —
delivery is confirmed, not assumed.

**Never start a turn on an idle agent.** An agent that talks to the user unprompted
is a bug. The front end wires `Deliver` as `if !ag.Running() { return false };
return ag.Steer(in)`, and deliberately does **not** use `OnSettled` (which would let
a steer append inside the settled window). Pending completions reach the model on
the next *real* turn via `Flush()` called from a turn-start observer sink:
at that point `running` is true, so the steer queues and lands at step 1 without
ever starting a turn.

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
session model) and `maxConcurrent` (default 4), both bound for free through env
reflection. Here it sizes the manager's semaphore; `/settings` edits it.

## Front-end wiring (`main.go` / `console.go`)

The only wiring layer. The manager is built in `driver` after the tool registry and
barrier exist, with adapters:

- `Activity` → `ui.SetActivity`
- `Notice` → `ui.NotifyKeyed("subagent", msg, LevelInfo)`
- `Status` → `ui.SetStatusSegment({Key:"subagents"})`
- `Deliver` → the `Running()`-guarded `ag.Steer`, per above
- A tiny sink (`NopSink`, overriding only `TurnStart`) appended to `opts.Sinks`
  calls `mgr.Flush()`

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
- **Tool-set enforcement** — `agent_start` is absent from a child's resolved tools even
  when the source reports it read-only; `bash`, `write`, `edit` are unreachable;
  `find`/`grep`/`ls` reach despite being disabled in the parent registry.
- **Poll timeout payload** — elapsed and context usage against the child model window.
- **Notification** — a poll in flight suppresses the steer; with no poller a steer is
  delivered; a false `Deliver` leaves ids pending and later `Flush` re-offers;
  `Delivered` clears exactly the named ids. An idle parent never starts a turn.
- **Empty summary** — nudge → summary; two empty nudges → placeholder.
- **Activity rows** — one per job, coalesced/cleared on completion, never in history.
- **Accounting** — child spend appears in `ChildTotal()` and the parent's `Total()`;
  the parent's `Context()` is unchanged by a child.

## Invariants

1. A child has only read-only tools; `agent_*` (and so grandchildren) are impossible
   by construction, not instruction.
2. Nothing from a child ever reaches committed history — its sink feeds activity rows
   only.
3. Completion delivery is confirmed: ids clear only when the steer lands, and one
   dropped by an interrupt is re-offered.
4. A completion never starts a turn on an idle parent; `OnSettled` is unused for this.
5. Child spend rolls up into the parent's totals but never moves its context bar.
6. `Close` cancels everything and returns promptly even if a provider stalls.
