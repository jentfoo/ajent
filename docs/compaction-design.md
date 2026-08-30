# Compaction design

How `pkg/compact` keeps a long session inside the model's context window: it
keeps the most recent steps verbatim and folds everything older into one structured
checkpoint recorded on a compaction entry and replayed on rebuild. It builds on the
agent loop (`pkg/agent`), the ledger (`pkg/tokens`) and the transcript
(`pkg/session`), and its summaries use the prompts in `prompt-design.md`.

## What it is

Left alone, a tool-heavy session grows until the next request exceeds the window
and the provider refuses it. Compaction keeps the most recent work exactly as it
is and replaces everything older with one structured checkpoint:

```
the verbatim band   the newest steps, byte-exact, never reduced
the summary         everything before it, folded into a checkpoint
```

There is one path. A compaction always cuts and always summarises; nothing else
reduces context. Free structural reduction still happens, but only to the
transcript the summariser reads. Superseded reads, repeated output and failed
results become self-describing markers so the checkpoint is built from what is
worth reading rather than from raw bytes. None of that reaches the rebuilt
context, because the cut already removed everything it touches.

The point at which an automatic compaction fires is the
per-model `compactThreshold`. See "Triggers".

Every before/after measure includes the fixed request overhead (system block +
AGENTS.md + tool schemas), supplied as `Options.Base`. A message-only measure would
under-report full usage, since the bar counts that overhead too.

The measure is also taken **after `llm.Prepare`** with the session's model and
retain policy, the same normalisation pass every wire request goes through.
Without it the measure counts thinking blocks that retention would drop from
completed turns (nor do they reach the wire on a non-`RetainAll` policy), so every
number came out high and a "successful" compaction could still leave the estimate
far above what the next request actually sends. What is measured is what is sent.
An overflow run measures mid-turn with `Base` 0. The reseed that follows is
transient, replaced once the retried turn reports real usage.

## Where reductions live

A load-bearing invariant is *everything the model sees is reconstructible from the
transcript* (see `session-design.md`). So a compaction cannot merely rewrite the
in-memory message list. That would vanish on resume. Instead the cut and summary
are recorded on the `compaction` entry and replayed on every context rebuild.

`pkg/session` owns the schema and the replay (it already owns assembly);
`pkg/compact` computes the plan. No import cycle: `compact -> session -> agent`.

The reduction plan records, per tool result to replace or elide by call id, a
**stub**: either `Text` swaps the content outright for a marker or `Limit`
elides it to that many bytes, with exactly one of the two set. The plan as a
whole carries those stubs, the message entry ids dropped outright, whether
thinking is stripped from the kept region, and the message count for the notice.
New entries record none of the first three (see "The recorded plan is inert"),
but replay still honours them for transcripts written by older builds; the
per-reason stats fields are likewise legacy-replay-only (no producer writes
them, and the notice reads only the summarized count).

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
  (see `prompt-design.md`) inside `<summary>` tags, and emits it as `RoleUser`.
  The adapters skip `RoleSystem` messages outright (system is a top-level field),
  so a system-role summary would never reach the model. A user message also makes
  the cut legal. The band opens on an assistant message, and Anthropic requires
  the first non-system message to be user, so the summary is load-bearing rather
  than decorative.
- **An empty `FirstKeptEntryID` means "reductions only, no cut"**, unless a
  `Summary` is set. That is a summary-only compaction, where the compaction
  entry's own position is the boundary and every message before it folds into
  the summary. That is how a manual compact can shrink a session with no older
  turn to keep. A non-empty id missing from the branch warns and keeps nothing.

## The reduction pass (free)

`spanStubs` runs over the region still in context and returns replacements for the
summariser to read. Detection is wide and emission is narrow: rules look at the
whole region, including the verbatim band, but a replacement is only emitted for a
result *before* the band. Narrowing detection instead would blind the best rules,
since a read superseded by a later read inside the band would look like the newest
copy of that file. Narrowing emission is what keeps the band exact.

- **Failed results** become a one-line stub naming the tool and keeping the
  first line of the result. The model already reacted to the failure;
  the bytes are dead weight, and a barrier denial reason survives in that line.
  There is no age qualifier: the band already protects recent work.
- **Superseded reads** keep the newest and mark the rest when the same canonical
  path is read more than once. Detected from the transcript, not the read tracker;
  each `read` call's `path` is canonicalised with `tools.PathPolicy.Resolve` so
  keys match the file tools.
- **Superseded edits**, position-aware: only an edit whose call precedes a *later*
  wholesale `write` to that path is marked. Each write records its branch index, so
  an edit that lands after the rewrite (building on it) is kept. Ordering matters,
  not just the presence of a write.
- **Repeated output** keeps the first copy and marks the rest. The key includes the
  producing call's name and canonical path as well as its text, so two distinct
  results that happen to share bytes are not collapsed.

The markers are worded to explain themselves, naming what the elided result
was. The summariser then needs no instruction about what it is looking at.

Thinking is not stubbed; it is simply left out of the transcript, which costs a few
percent of the input and removes a whole class of confusion about whose reasoning
is being read.

## The cut and the summary

### The verbatim band

The unit is a **step**: an assistant message through to just before the next one,
carrying its tool calls and their results. Steps are stable whether a session has
fifty user prompts or one. This matters because an agentic session routinely has
one prompt and hundreds of steps, and any rule keyed to turns is keyed to nothing
there.

The band is the newest `compaction.minSteps` steps, kept whole however
large, extended backwards with older steps while it stays within
`compaction.verbatimFraction` of the compaction point in message
tokens. The floor outranks the ceiling: a band that shrank below the live work
would leave the agent re-reading what it just did, and every turn would compact
again immediately. The ceiling counts only the kept steps' own messages, not the
system block, tool schemas or the summary, because the cut is chosen before
anything that depends on it.

When the entry immediately before the band is a real user prompt (typed, not
system-injected), the band extends to include it. Otherwise a mid-turn compaction
folds the question the user just asked into prose while keeping the answer to it
verbatim. The prompt rides along whatever it weighs; it is half of the live
exchange, so the ceiling bounds the steps, not the prompt in front of them.

Opening the band on an assistant message keeps it well formed by construction. The
one shape a provider rejects is a `tool_result` with no matching `tool_use`, and
that cannot occur at a step boundary. The opposite direction, a `tool_use` whose
results were cut away, is repaired by `repairTurns`, which fills each unanswered
call with a placeholder result.

Where a previous compaction exists, the band never reaches earlier than *its* cut,
and the summarised span starts there rather than at the root, so messages that
survived last time are re-summarised and merged rather than nested, and folded-away
messages never re-enter a later plan. When the band already reaches the prior cut
there is nothing new to fold and the compaction declines.

Every trigger produces the same band. A manual `/compact` is a request to reduce
*now*, not a request to keep less recent work, so it does not reduce further than
an automatic run would; the plan is identical whether a compaction was asked for,
crossed the threshold, or followed an overflow. The only thing a trigger decides is
whether compaction runs at all.

### The recorded plan is inert

Everything before the cut is replaced by the summary and everything after it is
verbatim, so no stub could apply to anything that survives. The compaction entry
therefore records an empty reduction carrying only the count of messages folded.
This is what makes replay trivially correct: there is no plan to re-derive, no
marker that can outlive the content it referred to, and nothing for a later rebuild
to reason about beyond the cut itself.

### Summarisation

One call on the **active session model**. The request carries the dedicated
summariser system prompt, then one user message: the serialised span (each result
clipped), an optional prior summary when merging, the six-section format spec,
and an optional focus line. That focus is either the user's `/compact <instructions>`
or, on an automatic run, a caller-supplied default through `compactor.focus`.
That is how a plan phase keeps its summary on implementation work or review
findings. An explicit `/compact <instructions>` always wins. The exact wording
lives in `prompt-design.md`.
`MaxTokens` is sized as a fraction of what the call replaces, floored so a merged
checkpoint is never amputated, and capped by what the model can emit and by the
tokens the call replaces (the prior summary plus the new span). A blank response
is a failure, not a legitimate no-op, and so is a truncated one: the drain rejects
a max_tokens stop reason before any text escapes, so an incomplete checkpoint can
never be recorded or replayed.

Serialising flattens history to text, with one labelled line per message and tool
calls named by their argument and results inlined. This is what makes "do
NOT continue the conversation" hold and sidesteps tool-pairing validation on the
summarisation request entirely.

A single merged call covers the span from the prior cut to the band. One call
degrades better on the small local models `ajent` supports than any split-turn
scheme would, and the `<summary>` user-message re-injection is what keeps a cut
that lands mid-turn valid.

The summariser's own usage folds into the session ledger so `/usage` counts it.
The stream is driven with `llm.Accumulator`, the same as the agent loop; a
`max_tokens` stop reason ends the run with an error. Partial text is never
returned.

Nothing else is needed to make compaction phase-aware. It already plans against
the live head's branch, and under the plan workflow that branch *is* the phase
(see `plan-design.md`), so the cut point cannot wander outside it. There is no
minimum cut, no segment-aware entries, and no summary that has to masquerade as
a phase seed.

## Triggers

- **Manual.** `/compact` or `/compact <instructions>`, refused while a turn streams
  (press Esc first). A session with nothing older than the band to fold, or one
  whose summary cannot shrink the context, reports that there is nothing to compact.
- **Automatic.** When `Used` crosses `tokens.CompactAt(model)`, which is the
  `compactThreshold` from `models.json`, a fraction of the window or an absolute
token count. The hook runs at the next turn boundary, never
  mid-turn and never between a tool call and its result. The hook decides whether
  the threshold is crossed; the agent stays dumb. `compaction.auto: false` gates
  this trigger and only this one. Models that declare no `compactThreshold` of
  their own take `compaction.threshold`, applied by the registry so discovered
  models get it too, which keeps the trigger, the context bar and the band ceiling
  reading one number.
- **Emergency.** `llm.ErrContextOverflow` from a request compacts aggressively and
  retries the same step once. Nothing was appended for the failed call, so state
  and transcript stay in agreement. Without this, one oversized tool result bricks
  the session.

Recovery from overflow relies on tool-layer output limits keeping any single
result far below the window; when the bloat sits inside the verbatim band,
compaction declines by design (the newest steps are never reduced) and the turn
fails. A rewind or a model switch recovers.

The agent cannot import `session` or `compact` (session already imports agent),
so the trigger is a func field on `agent.Options`, matching `Provider` and
`OnMessage`: it receives a small reason enum (manual, threshold-crossed or
overflow) and reports whether a compaction ran.

The context bar and `/usage` fill against `CompactAt`, so the bar reads full
exactly when an automatic compact would fire.

## Honesty

Every compaction reports real numbers and is persisted as a `notice` entry so it
replays on resume and marks the boundary in the transcript view: the before and
after context sizes and how many messages were folded into the summary.

The notice names only what changed in context. The reduction pass also replaces
superseded and repeated results, but only in the transcript the summariser reads,
so reporting it would describe work the next request never sees. Silent compaction
leaves users convinced the agent "forgot" for no reason.

## Recovery

Nothing is deleted, so undo is a rewind: rewinding onto the compaction row
moves `HEAD` to its parent and restores the full pre-compaction context
(`session.RewindTarget` maps a compaction row onto its parent). There is no
`/compact undo` command. That would duplicate the rewind path. See "Branches,
tips and rewinding" in `session-design.md`.

## Invariants

Load bearing; each exists because breaking it produced a real bug.

**0. A compaction is recorded only when it measurably shrinks the next request.**
`finish` compares the measured after/before and refuses anything else. A cut
that keeps almost everything and adds a summary on top can otherwise *grow*
the context while reporting success.

**1. Reductions are replayed, not rewritten.** The plan lives on the compaction
entry and `ContextMessages` applies it on every rebuild. Rewriting only the
in-memory list would vanish on resume.

**2. A measured saving is the saving the next request gets.** Every stage measures
through the same `ContextMessages` the rebuild uses and the same `Prepare` pass the
wire uses, never a separate estimate.

**3. A cut never orphans a `tool_result`.** The band opens on an assistant message,
or on the real user prompt immediately before one, so a tool call and its result
always travel together. Fuzz-tested over random tool interleavings. The opposite
direction is repaired on the wire by `repairTurns`.

**4. The summary reaches the model.** Emitted as `RoleUser`, because adapters
drop `RoleSystem` messages; this also makes a mid-turn cut legal (see "The one
assembly function").

**4a. The verbatim band is never reduced.** Nothing inside it is stubbed, elided,
dropped or thinning-stripped, in any path. It is what the agent continues from, so
a marker there would point at content it can no longer read.

**4b. A recorded plan is inert.** The cut removes everything a stub could touch, so
the entry carries no stubs, no drops and no thinking strip. A plan that recorded
them would be dead weight that a later rebuild has to reason about, and stale
markers outliving their content is exactly how the duplicate-stub bug happened.

**5. Only the newest compaction applies, so it must carry the prior one forward.**
Assembly reads a single compaction entry, so a new plan that recorded no summary
and no cut would resurrect every message the prior one folded away. Each run seeds
its result from the prior plan and recomputes the stages over the region that is
actually in context; a summary-only prior is first resolved to an explicit
`FirstKeptEntryID`, or carrying it into a newer entry would re-cut at the new
entry's own position and drop everything between the two.

**5a. Before and after describe the real request.** Both are measured with the
prior compaction applied, never against the raw branch. Measuring against the raw
branch made invariant 0 unenforceable: a plan that reopened folded history still
compared favourably to a context the session had not sent in a long time.

**6. On rebuild, context and spend come from different places.** A compaction
rewrites what the branch sends, so the prompt sizes recorded against its surviving
messages describe a request that no longer exists. Replaying them reported the
*pre*-compaction size, and reported it as exact, so the next turn compacted again
immediately. `CompactionData.rewritesHistory()` (a cut, a drop, a stub or a
thinking strip; a plan carrying only `Stats` changed nothing) decides between two
paths in `State`:

- **rewritten**: context is measured from the assembled messages,
  `Reseed(tokens.EstimateFor(model, retain, msgs))`, which also accounts the
  synthetic summary message that is not an entry of its own. Recorded usage counts
  toward spend only, via `Accounting.RecordSpend`, and it counts for **every**
  message entry including ones the cut removed: those tokens were billed whether or
  not they still occupy context. Only an entry carrying a report is a turn. The
  recorder persists user echoes and tool results as entries too.
- **untouched**: nothing rewrote the branch, so the recorded prompt plus output is
  still exactly what the next request carries and it is replayed as before, bar and
  all, with no `~`.

The same applies to a live compaction: it reseeds with `Reseed(res.After - base)`,
using only the reduced **messages**, since `After` already counts base and the
ledger adds its own base on top at read time. The reseeded figure stays an estimate:
the calibrator's factor applies and the bar keeps its `~` marker, unlike `Rebase`,
which is reserved for exact tokenizer counts. The summariser call itself is
recorded spend-only (`Accounting.Spend`), so a failed compaction cannot leave the
bar at the summariser's (much larger) prompt size.

**7. A reduced context is reported to the host.** Reducing takes file content out
of context, because a cut drops reads outright. Meanwhile `tools.Tracker` still
records the process's reads. The compactor therefore calls
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

**10. A truncated summary is never recorded.** The driver's drain turns a
`max_tokens` stop reason into an error before any text is returned; a checkpoint
cut mid-section silently drops history nothing else preserves, so the run fails
loudly and the transcript is left untouched. Detection relies on the provider
reporting the stop reason.

## Conventions

The house style in `AGENTS.md` applies (see also the package-specific notes under
`session-design.md` "Conventions"). The distinctive test here is the frozen-corpus
case that proves a plan replayed through the same `ContextMessages` measures to its
recorded size; see "The frozen corpus" below.

## Extending

- **New reduction rule**: add it to `spanStubs`. Detect over the whole region and
  emit only before the band, or the band stops being verbatim.
- **A different summariser**: supply another `RunPrompt` (a cheaper model, a
  local one); nothing else changes.

## Known limits

- The summary is one merged call, so a mid-turn cut's summary covers the partial
  step with the same six-section format.
- A session whose last `minSteps` steps alone exceed the ceiling cannot compact
  below them. That is deliberate: the alternative is degrading the work the agent
  is in the middle of. When overflow bloat sits inside that band, compaction also
  declines by design and the turn fails. A rewind or a model switch recovers;
  recovery otherwise relies on tool-layer output limits keeping any single result
  far below the window.
- Every compaction spends a summariser call. There is no cheap structural-only
  path; it was unreachable in practice and left sessions half full.
- The transcript keeps every pre-compaction entry; compaction shrinks the rebuilt
  context, never the file (see `session-design.md` "Known limits").
- Transcripts written by older builds still carry stub plans and `Limit` elisions,
  and drop/strip-thinking reductions. `pkg/session` replays them unchanged, but
  only until the session's next compaction, whose inert plan replaces them (context
  can step up again and a later run may then decline). Under `RetainAll`, thinking
  inside the band is no longer stripped anywhere; older plans that did strip it are
  honoured as written.
