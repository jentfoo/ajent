# Compaction design

How `pkg/compact` keeps a long session inside the model's context window: cheap
structural reductions first, an LLM summary only when they are not enough. It
computes a reduction plan that `pkg/session` records and replays, so what the
model sees is always exactly what was measured. It builds on the agent loop
(`pkg/agent`), the ledger (`pkg/tokens`) and the transcript (`pkg/session`), and
its summaries use the prompts in `prompt-design.md`.

## What it is

Left alone, a tool-heavy session grows until the next request exceeds the window
and the provider refuses it. Compaction shrinks the context in stages, each
measured, stopping as soon as a target is met. The ordering is deliberate:
summarising costs a model call and loses detail, while a great deal of context is
recoverable for free by throwing away what is known to be worthless.

```
stage 1  failed / superseded / aborted   free, near-lossless
stage 2  age-tiered output elision        free, lossy at the margins
stage 3  thinking removal                 free, usually a no-op
stage 4  summarisation                    one model call, lossy
```

Stages 1-3 run at every trigger even when stage 4 will be needed anyway — they
shrink what stage 4 has to read, which makes the summary better and cheaper.

The target after any compaction is half the usable window budget (`Window` minus
the response reserve). The point at which an automatic compaction fires is the
per-model `compactThreshold` — see "Triggers".

Every before/after measure includes the fixed request overhead (system block +
AGENTS.md + tool schemas), supplied as `Options.Base`. A message-only measure would
under-report full usage and let stages 1-3 exit early while context still sits above
the target, since the bar counts that overhead too.

The measure is also taken **after `llm.Prepare`** with the session's model and
retain policy — the same normalisation pass every wire request goes through.
Without it the measure counts thinking blocks that retention would drop from
completed turns (nor do they reach the wire on a non-`RetainAll` policy), so every
number came out high and a "successful" compaction could still leave the estimate
far above what the next request actually sends. What is measured is what is sent.

## Where reductions live

A load-bearing invariant is *everything the model sees is reconstructible from the
transcript* (see `session-design.md`). So stages 1-3 cannot merely rewrite the
in-memory message list — that would vanish on resume. Instead the reduction plan
is recorded on the `compaction` entry and replayed on every context rebuild.

`pkg/session` owns the schema and the replay (it already owns assembly);
`pkg/compact` computes the plan. No import cycle: `compact -> session -> agent`.

The reduction plan records, per tool result to replace or elide by call id, a
**stub**: either `Text` swaps the content outright (e.g. "[read superseded by a
later read...]") or `Limit` elides it to that many bytes — exactly one of the
two is set. The plan as a whole carries those stubs, the message entry ids
dropped outright, whether thinking is stripped from the kept tail, and per-stage
stats for the notice.

A stub's limit rather than an inlined replacement string keeps the JSONL small:
assembly re-elides from the original bytes with `tools.Elide`.

Only the **newest** compaction is applied. `pkg/compact` therefore recomputes
every stage over the whole branch on each run, so the newest plan is always
cumulative. Reduction-replay is a session invariant; see "Compaction reductions
replay" in `session-design.md`.

## The one assembly function

One assembly function is shared by rebuild and measurement: given a branch and a
candidate compaction's data, it returns the messages that contribute to the next
request plus any warnings naming entries it could not use.

`session.State` calls it with the newest compaction; `pkg/compact` calls it with
candidate `CompactionData` values to measure each stage. One implementation, so a
measured saving is by construction the saving the next request gets.

Two things it fixes by construction:

- **The summary is a user message.** It wraps the summary in provenance framing
  ("The conversation history before this point was compacted...") inside
  `<summary>` tags, and emits it as `RoleUser`. The adapters skip `RoleSystem`
  messages outright (system is a top-level field), so a system-role summary would
  never reach the model. A user message also makes a mid-turn cut legal — the kept
  tail may start with an assistant message, and Anthropic requires the first
  non-system message to be user.
- **An empty `FirstKeptEntryID` means "reductions only, no cut"** — unless a
  `Summary` is set: that is a summary-only compaction, where the compaction
  entry's own position is the boundary and every message before it folds into
  the summary. That is how a manual compact can shrink a session with no older
  turn to keep. A non-empty id missing from the branch warns and keeps nothing.

## Stages 1-3 (free)

Applied in order, each measured through `ContextMessages`, with early exit once
the target is met.

**Stage 1 — failed, superseded, aborted.**
- Tool results marked `IsError` older than the last two turns become a one-line
  stub keeping the first line: `[bash failed: no such file...]`. The model already
  reacted to the failure; the bytes are dead weight. Barrier denial reasons
  survive under the same rule because the first line is kept.
- Superseded reads: the same canonical path read more than once keeps the newest
  and stubs the rest. Detected from the transcript, not the read tracker — each
  `read` call's `path` argument is canonicalised with `tools.PathPolicy.Resolve`
  so keys match the file tools.
- Superseded edits, position-aware: only an edit whose call precedes a *later*
  wholesale `write` to that path is stubbed. Each write records its branch index,
  so an edit that lands after the rewrite (building on it) is kept — ordering
  matters, not just presence of any write.
- Aborted assistant messages — no tool calls and no non-blank text — are dropped
  by entry id. A message carrying a `ToolCallBlock` is never dropped.

**Stage 2 — age-tiered elision.** Successful results shrink with age; the current
turn is live working set and is never touched.

| turn distance | budget |
|---|---|
| current (0) | untouched |
| 1 | 8 kB |
| 2-3 | 1 kB |
| 4+ | 512 B (collapses toward a shape line) |

Duplicate outputs keep the first and stub the rest. The duplicate key includes the
producing call's name and canonical path as well as its text, so two distinct
results that happen to share bytes (e.g. reading different files with identical
content) are not collapsed.

**Stage 3 — thinking removal.** Sets `StripThinking` so assembly drops thinking
blocks from the kept tail. Request-build retention already strips completed-turn
thinking unless the policy is `RetainAll`, so this stage only bites on `RetainAll`
sessions; otherwise it measures honestly and reports zero.

## Stage 4 (a model call)

When stages 1-3 do not reach the target, pick a cut point and summarise
everything before it.

### Cut point

Walk the kept region backwards accumulating estimated tokens until the recent-tail
budget is reached, then snap to the nearest valid turn boundary so the kept tail
stays well formed. A valid cut is a turn start (a user message that is not only
tool results) or an assistant message — keeping `[i:]` from an assistant keeps its
`tool_use` and the results that follow, and the dropped prefix always ends after a
complete result set. The snap prefers the boundary at-or-before the budget line so
it never under-keeps, and never splits a tool call from its result.

Where a previous compaction exists, the summarisable span restarts at *its*
`FirstKeptEntryID` rather than at the root — or just after the compaction entry
itself for a summary-only one — so messages that survived last time are
re-summarised and merged rather than nested, and folded-away messages never
re-enter a later plan. If that span is empty there is nothing new to summarise
and the cut alone is the plan.

A manual `/compact` forces stage 4 even when the context sits far below the
target: it keeps the current turn when there are older turns to fold, and cuts
past the end — a summary-only compaction — when there are not. Automatic triggers
never take the forced path: no usable cut means stages 1-3 alone are the plan and
no summariser call is spent.

### Summarisation

One call on the **active session model**. The request carries the dedicated summariser system
prompt, then one user message: `<conversation>...</conversation>` (the serialised
span, each result clipped), an optional `<previous-summary>` when merging, the
six-section format spec, and an optional `Additional focus:` line. That focus is
either the user's `/compact <instructions>` or, on an automatic run, a
caller-supplied default through `compactor.focus` — which is how a plan phase
keeps its summary on implementation work or review findings. An explicit
`/compact <instructions>` always wins. The exact wording lives in `prompt-design.md`.
`MaxTokens` is capped below the span's own size, so a runaway summary comes out
smaller than what it replaces; a blank response is a failure, not "nothing to
compact".

Serialising flattens history to text — `[User]:`, `[Assistant]:`,
`[Assistant tool calls]: name(args)`, `[Tool result]:` — which is what makes "do
NOT continue the conversation" hold and sidesteps tool-pairing validation on the
summarisation request entirely.

A single merged call covers `[priorSpanStart, cut)`. The two-call split-turn
design in `prompt-design.md` is deliberately not implemented: the merged
six-section format plus the `<summary>` user-message re-injection keeps a mid-turn
cut valid, and one call degrades better on the small local models `ajent`
supports.

The summariser's own usage folds into the session ledger so `/usage` counts it.
The stream is driven with `llm.Accumulator`, the same as the agent loop.

Nothing else is needed to make compaction phase-aware. It already plans against
the live head's branch, and under the plan workflow that branch *is* the phase
(see `plan-design.md`), so the cut point cannot wander outside it — no minimum
cut, no segment-aware entries, no summary that has to masquerade as a phase seed.

## Triggers

- **Manual.** `/compact` or `/compact <instructions>`, target 50%, refused while a
  turn streams (press Esc first). Unlike the automatic triggers it always runs
  stage 4, so a small or even single-turn session still compacts; a run whose
  summary cannot shrink the context reports "nothing to compact".
- **Automatic.** When `Used` crosses `tokens.CompactAt(model)` — the
  `compactThreshold` from `models.json`, a fraction of the window (default 0.8) or
  an absolute token count — the hook runs at the next turn boundary, never
  mid-turn and never between a tool call and its result. The hook decides whether
  the threshold is crossed; the agent stays dumb.
- **Emergency.** `llm.ErrContextOverflow` from a request compacts aggressively and
  retries the same step once. Nothing was appended for the failed call, so state
  and transcript stay in agreement. Without this, one oversized tool result bricks
  the session.

The agent cannot import `session` or `compact` (session already imports agent),
so the trigger is a func field on `agent.Options`, matching `Provider` and
`OnMessage`: it receives a small reason enum — manual, threshold-crossed or
overflow — and reports whether a compaction ran.

The context bar and `/usage` fill against `CompactAt`, so the bar reads full
exactly when an automatic compact would fire.

## Honesty

Every compaction reports real numbers and is persisted as a `notice` entry so it
replays on resume and marks the boundary in the transcript view:

```
compacted 142k -> 61k (dropped 18 failed tool results, truncated 6 outputs, summarised)
```

Clauses whose count is zero are omitted. A run that met the target on stages 1-3
says so and never mentions a summary. Silent compaction leaves users convinced the
agent "forgot" for no reason.

## Recovery

Nothing is deleted, so undo is a rewind: esc+esc onto the compaction row moves
`HEAD` to its parent and restores the full pre-compaction context
(`session.RewindTarget` maps a compaction row onto its parent). There is no
`/compact undo` command — that would duplicate the rewind path. See "Branches,
tips and rewinding" in `session-design.md`.

## Invariants

Load bearing; each exists because breaking it produced a real bug.

**0. A compaction is recorded only when it measurably shrinks the next request.**
`finish` compares the measured after/before and refuses anything else — a cut
that keeps almost everything and adds a summary on top can otherwise *grow*
the context while reporting success ("compacted 500 -> 688").

**1. Reductions are replayed, not rewritten.** The plan lives on the compaction
entry and `ContextMessages` applies it on every rebuild. Rewriting only the
in-memory list would vanish on resume.

**2. A measured saving is the saving the next request gets.** Every stage measures
through the same `ContextMessages` the rebuild uses and the same `Prepare` pass the
wire uses, never a separate estimate.

**3. A cut never orphans a `tool_use`.** Cuts land only on a turn start or an
assistant message; a tool call and its result always travel together. Fuzz-tested
over random tool interleavings.

**4. The summary reaches the model.** Emitted as `RoleUser`, because adapters
drop `RoleSystem` messages; this also makes a mid-turn cut legal (see "The one
assembly function").

**5. Only the newest compaction applies, and it is cumulative.** Each run
recomputes every stage over the whole branch.

**6. On rebuild, context and spend come from different places.** A compaction
rewrites what the branch sends, so the prompt sizes recorded against its surviving
messages describe a request that no longer exists — replaying them reported the
*pre*-compaction size, and reported it as exact, so the next turn compacted again
immediately. `CompactionData.rewritesHistory()` (a cut, a drop, a stub or a
thinking strip; a plan carrying only `Stats` changed nothing) decides between two
paths in `State`:

- **rewritten** — context is measured from the assembled messages,
  `Reseed(tokens.EstimateFor(model, retain, msgs))`, which also accounts the
  synthetic summary message that is not an entry of its own. Recorded usage counts
  toward spend only, via `Accounting.RecordSpend`, and it counts for **every**
  message entry including ones the cut removed: those tokens were billed whether or
  not they still occupy context. Only an entry carrying a report is a turn — the
  recorder persists user echoes and tool results as entries too.
- **untouched** — nothing rewrote the branch, so the recorded prompt plus output is
  still exactly what the next request carries and it is replayed as before, bar and
  all, with no `~`.

The same applies to a live compaction: it reseeds with `Reseed(res.After - base)` —
the reduced **messages** only, since `After` already counts base and the ledger adds
its own base on top at read time. The reseeded figure stays an estimate: the
calibrator's factor applies and the bar keeps its `~` marker, unlike `Rebase`,
which is reserved for exact tokenizer counts. The summariser call itself is
recorded spend-only (`Accounting.Spend`), so a failed compaction cannot leave the
bar at the summariser's (much larger) prompt size.

**7. A reduced context is reported to the host.** Reducing takes file content out
of context — a cut drops reads outright, `truncate` elides their results — while
`tools.Tracker` still records the process's reads. The compactor therefore calls
the same rebuild hook `switchState` uses (`sessRec.onSwitch`), which resets read
tracking so a later `@file` re-injects rather than deduping against content the
model no longer has. Any future path that replaces `State.Messages` owes the same
call.

**8. A model switch remeasures, never reads empty.** `SetModel` drops every context
term for the new window; it immediately reseeds from the actual in-memory messages,
so switching to a smaller window reflects real occupancy and lets threshold
auto-compaction fire on that model instead of waiting for an overflow.

**9. At most one overflow retry per turn.** A second overflow fails the turn
rather than compacting in a loop.

## Conventions

Same repository style as the rest (see `session-design.md` "Conventions"): godocs
describe inputs and outputs not mechanism, terse comments only where non-obvious,
stdlib `slices`/`maps`/`bulk` over manual loops, one `_test.go` per file with
table-driven subtests, `require` for setup and `assert` for assertions, never
`time.Sleep`.

## Extending

- **New reduction stage**: add a function that returns stubs/drops and a `Stats`
  count, wire it into `Compact` between the existing stages, and measure it
  through `ContextMessages` like the others.
- **A different summariser**: supply another `RunPrompt` (a cheaper model, a
  local one); nothing else changes.

## Known limits

- Stage 4 uses one merged call, not the two-call split-turn design, so a mid-turn
  cut's summary covers the partial turn with the same six-section format.
- Age-tier budgets are fixed constants, not tuned per model or per tool.
- The transcript keeps every pre-compaction entry; compaction shrinks the rebuilt
  context, never the file (see `session-design.md` "Known limits").
