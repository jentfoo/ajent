# Plan workflow design (`pkg/plan`)

`/plan` splits one conversation into two models and four phases: a **planner**
drafts, the **user approves**, an **implementor** builds against that plan alone,
and the planner **reviews** the result. It exists because deciding what to build
and doing it need different context, and sharing one ever-growing message list
serves neither.

The reference implementation spends most of its length compensating for a host
with no notion of phases:
synthetic messages tagged with a magic string and re-parsed out of the list, a
hand-written context projector re-derived on every request, and compaction
intercepted so the cut point stays inside a phase. **ajent needs none of that**,
and that is the whole point of this design.

## Phases are branches, not projections

The transcript is already a tree. `session.Branch(entries, head)` walks
`ParentID` to the root and is the only read path; `session.State(branch, resolve)`
rebuilds `agent.State` from it. So a phase switch is a **head switch**:

```
root ─ prior chat ─ /plan ─ planning turns ─ dev_implement ─ P
                                                             └─ review r1 ─ review r2   ← HEAD ends here
(new root) ─ impl r1 kickoff ─ implementation turns
(new root) ─ impl r2 kickoff ─ implementation turns
```

- **Planning** continues on the current branch, so it inherits every message that
  came before `/plan` by construction. `/plan` can be run at any point in a
  conversation.
- **Implementing** appends with `ParentID == ""`, a **new root**. `Branch` stops
  there, so the rebuilt state is that round's kickoff and its own work. Nothing
  else exists to leak.
- **Reviewing** forks from `P`, the hand-off tip, on round 1 and continues
  linearly from the previous review tip afterwards. The reviewer sees the goal,
  the planning conversation and its own earlier findings, never implementation
  chatter.
- **Completion** leaves HEAD at the review tip.

Because these are real branches, the token ledger, the context bar, `/compact`,
`/usage` and the rewind picker all measure what the model actually gets. A
projection transform would leave every one of them reporting the wrong list.
`agent.Transform` stays unused by this feature.

## Invariants

1. **A phase switch never mutates a prior branch.** Nothing is deleted; every
   phase is reachable from the rewind picker afterwards.
2. **Only a successful control call ends a turn.** `dev_*` tools set
   `agent.ToolResult.EndTurn` on their success path only; a rejected call is an
   `IsError` result the model corrects inside the same turn. The loop enforces
   this too. `runTool` reports `EndTurn && !IsError`, so a tool cannot silence
   a model by erroring.
3. **Every exit path restores scope.** Completion, `/plan-stop`, `Esc` and the
   revision cap all reach one `stopLocked`, which forks back to the live branch
   with the saved model, restores the saved tool set, unregisters the control
   tools and writes a terminal state entry.
4. **The reviewer reviews the plan the user approved**, not the one the planner
   drafted. The approved text is the submission that started implementation.
5. **Transition ordering is fixed**: capture the tip, `Fork`, apply the tool
   scope, persist, then return the kickoff. The `model_change` entry `Fork`
   writes is therefore the first entry of a new root, which is what lets
   `session.State` resolve that branch's model on resume.

## The `Host` seam

`pkg/plan` imports only `pkg/agent` and `pkg/llm`, never `session`, `tools`,
`tui` or `command`. Everything else arrives as func fields on `Host`, supplied by
`main.go` (`plan.go`), the same shape `pkg/subagent` uses. A nil field disables
that capability rather than panicking, and the whole workflow is unit-testable
against a fake `Host` with no UI, transcript or registry in scope.

`Fork(head string, m llm.Model) error` is the interesting one. `main.go`
implements it as `(*sessRec).forkTo`, built on the branch-switch half of `rewind`
(`switchState`) **without** rewind's `ui.Reset()` + `Replay`: the screen is never
reset, because a phase switch changes what the *model* sees, not what the user
sees. An empty head starts a new root. It deliberately does not go through
`console.SetModel`, whose same-model early return would leave a branch with no
model entry.

## Phases and transitions

| From | Trigger | To | Action |
|---|---|---|---|
| — | `/plan` | Planning | save model + tool set, register `dev_*`, fork in place onto the planner |
| Planning | `dev_implement(plan)` | AwaitingPlan | record the plan tip, put the plan in the editor, **start no turn** |
| AwaitingPlan | the user submits | Implementing | the submitted text is the plan of record; new root, implementor scope |
| Implementing | the turn ends, `dev_review` or not | Reviewing | fork the review tip (else the plan tip), planner scope, git state |
| Implementing | the turn errored or hit a token/step limit | Implementing | retry a few times, then pause in place |
| Reviewing | `dev_revise(instructions)` | Implementing | record the review tip, new root seeded with plan + instructions |
| Reviewing | `dev_complete()` | Done | restore scope |
| Reviewing | stopped with no verdict | — | ask the user: revise / accept / keep reviewing |
| any | `Esc`, `/plan-stop`, revision cap | Done | restore scope and report |

The user gate at **AwaitingPlan** is the point of the workflow: `dev_implement`
hands off to nobody. The plan lands in the editor to read, edit, rewrite or
abandon, and the next submitted prompt is what the implementor receives.

**The implementor's report always reaches the reviewer.** The reviewer sees none
of the implementation branch, so `dev_review`'s `summary` is required. An empty
one is rejected like any other bad control call, in-turn. A round that ends
*without* calling `dev_review` at all still has to be reported, so the
implementor's closing assistant message stands in as the summary
(`Host.LastText`, read before the fork replaces the context). Without that
fallback a stopped implementor reaches review as silence, and the reviewer has
only `git status` to go on.

Both the revision and the retry counts are bounded by small constants.

An implementation turn that ends in a provider error **or** in `StopMaxTokens`
(the step limit, or an output cut short) stopped before the work was done, so it
is retried rather than reviewed. Reviewing it would judge an implementation that
never finished. Only a clean stop advances to review.

A failed `Fork` or `Persist` is reported, never swallowed: branching is what
isolates a phase, so continuing against stale state would quietly break the
guarantee the workflow exists to provide.

## Per-phase scope

| Phase | Model | Tools |
|---|---|---|
| Planning | planner | `read grep find ls bash` + `agent_*` when registered + `ask_user` + `dev_implement` |
| Implementing | implementor (the model in use at `/plan`) | the tool set saved at `/plan`, minus anything the workflow owns, + `dev_review` |
| Reviewing | planner | as planning, minus `dev_implement`, plus `dev_revise` + `dev_complete` |

The implementor gets the user's own working set plus exactly one tool to signal
completion, all present from the first message of its context; an empty captured
set falls back to the core read/write tools rather than handing it `dev_review`
alone.

The reviewer has **no `write` or `edit`**, which is structural rather than
instructed. It does keep `bash`, so "read-only" is a property of the tool set,
not a guarantee: a shell command that writes is possible and is gated by the
permission barrier exactly as it is anywhere else. The review kickoff also tells
it not to edit.

The control tools are registered lazily on `/plan` under source `"plan"` as one
`/tools` group and unregistered on every exit, so they never exist outside a
workflow. They and `ask_user` are marked read-only, so the permission barrier
runs them free without being widened for anything else. The barrier itself is
unchanged, and `block-all` still prompts for them, which is what `block-all`
means.

A phase scope narrows the enabled set, which `/tools` refuses to do after the
first prompt. That rule is a command-layer policy protecting the user from a
model-visible tool set shrinking under them; a plan scope is explicit,
user-initiated and restored on every exit path, so it calls
`Registry.SetEnabled` directly.

## Display

Phase transitions emit a divider plus a keyed notice naming phase, model and
round, and update the `plan` status segment with the phase and round. A fork
that does not move (for example `/plan` itself, which only records the planner on the
current head) draws no divider, since nothing was divided. The
kickoff message itself is **not** echoed live: the user just read the plan in the
editor, and the review kickoff's `git status` reads better in the reviewer's own
output. It is recorded in the transcript, so `session.Replay` does show it after
a resume. That is a deliberate asymmetry.

Prompts the user types while a workflow turn runs still reach that turn through
`OnBoundary`/`q.pull`. User steering can therefore enter the implementor's
otherwise-pristine context, by design: it is the user's agent.

## Persistence and resume

One latest-wins `custom` entry, `customType: "plan"`, written at every
transition on the branch that transition creates. `session.LatestCustom`
reverse-scans the resumed branch for it. `PhaseDone`/`PhaseIdle` is terminal, so
a later resume never resurrects a finished run, and a model that no longer
resolves abandons the restore with a notice. Tool sets are runtime-only, so a
resume re-applies the phase scope rather than reconstructing it from
`setting_change` entries.

Because ending a workflow leaves HEAD at the review tip while the file tail is an
implementation entry, resume must prefer the live head over the tail
(`resumeHead`); the same divergence already existed after a rewind.

## Compaction

Compaction already runs over the live head's branch, which *is* the phase. There is no
minimum cut, no segment-aware entries, no summary masquerading as a phase seed.
The only addition is a per-phase focus (`Controller.Focus`) fed to
`compact.Options.Instructions` for automatic runs; an explicit
`/compact <instructions>` still wins.
