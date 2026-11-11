# Agents

The surfaces that extend one session beyond a single model with a single tool
set: sub-agents that offload read-only research and the `/plan` workflow that
splits a conversation across two models. MCP servers are a surface of their
own in `mcp.md`. Permission modes live in `permissions.md`, the `subagent`
config keys and their `AJENT_*` bindings in `config.md`, every other slash
command in `commands.md`.

## Sub-agents

Sub-agents are read-only research jobs the model delegates so the main context
stays small. Instead of fifty tool results entering the conversation, one
summary paragraph does. They are enabled by default and toggle as one `subagents`
row in `/tools`. Calling them is itself a non-mutating act, so read-only
permission modes run them without approval (`permissions.md`).

The model drives the loop itself:

1. `agent_start(task, instructions?)` spawns an isolated child and returns a
   job id (`sub-1`, `sub-2`, ... in the order the model asked) immediately.
   Several may start in one batch.
2. The model keeps working while children run in the background.
3. `agent_poll(id)` waits for a job to finish and returns the child's final
   summary as its entire result. On an error or abort that is reported instead.
4. `agent_list` lists every job with id, status and elapsed time.

A child has no session context: pass file paths and key facts, not content. It
can read files itself. A poll with no wait left reports still-running, or
queued with its wait time when the job is behind the concurrency cap, so the
model knows when to come back. Ids accept `sub-2` or bare `2`.

### What a child can do

A child's tool set is fixed by ajent and cannot be widened by any instruction:
the built-in readers (`read`, `grep`, `find`, `ls`), the four `git_*` history
readers when the session sits inside a git work tree, plus any MCP tool marked
read-only. There is no `bash`, `write`, `edit` or `agent_*` in a child, so a
child can never spawn a child. Nothing in a child asks for approval.

A tool you disabled in the parent still reaches a child: a child without shell
needs `find`/`grep`/`ls` more than the parent's `/tools` selection does.

### Jobs, statuses and elapsed

Every job carries one of `queued`, `running`, `done`, `error`, `aborted`.
Elapsed is queue wait for a job that never took a concurrency slot, and active
runtime otherwise, frozen when it ends. Ids, activity rows and `/agents` all
read in submission order, however the parallel starts raced.

Concurrency caps parallel jobs with the rest queued behind them in order, and
a child's turn is bounded by a step limit; a child that hits the cap gets one
wrap-up turn to summarise what it found. Both limits are configurable
(`config.md`).

When a job finishes while nobody is polling, a keyed notice lands and the
summary reaches the model as context at the next turn boundary, never starting
a turn on its own. A job the user stopped reports `aborted` and is never
presented as a completed investigation.

### Configuration and cost

The child model, concurrency cap, poll wait and step limit are configurable;
`config.md` defines the keys, defaults and startup-read rules. A `/settings`
edit of the child model reaches the next job at spawn. `/usage` reports what
delegation cost separately from session spend: child tokens roll into the
session totals but never move the context bar.

### /agents and Esc

`/agents` prints a table of every job (id, status, elapsed, task).
`/agents stop <id>` cancels one job, `/agents stop all` cancels every
in-flight one, both reporting the count. Esc is not that gesture: Esc
interrupts the turn and releases any waiting poll, while jobs keep running
because they are independent investigations and re-running them is expensive.
Cancelling jobs is `/agents stop` only.

## /plan

`/plan` splits one conversation into two models and four phases: a **planner**
drafts, the **user approves** the plan, an **implementor** builds against that
plan alone, and the planner **reviews** the result. It exists because deciding
what to build and doing it need different context.

`/plan [goal]` starts the workflow. The goal argument prefills the editor, so
`/plan fix the login bug` opens with that text ready to submit. Starting needs
an interactive terminal with a model picker: serve mode and plain render mode
refuse it, as does starting while a turn runs (`Esc` first) or with no active
model (`/model` first). A model picker opens for the planner, and the model in
use at `/plan` is the implementor.

### Phases

| Phase | Who | What happens |
|---|---|---|
| planning | planner | chat on the current branch, inheriting everything before `/plan`. Calls `dev_implement` with the finished plan |
| plan gate | the user | the plan lands in the editor to read, edit, rewrite or abandon. The next typed submission starts implementation and is the plan of record |
| implementing | implementor | a fresh context containing only the approved plan (plus revision instructions on later rounds), building with the tool set saved at `/plan` |
| reviewing | planner | verifies the implementation against the plan and the git state, then calls `dev_complete` (accept) or `dev_revise` (another round) |

The plan gate is the point of the workflow: `dev_implement` hands off to
nobody. Only a typed submission counts as approval, so injected context never
starts work, and a queued prompt waits at the gate instead of slipping past it.

Phases are real branches of the session transcript, so the token ledger, the
context bar, `/compact` and the rewind picker all measure what the model
actually gets. Planning continues the current branch. Each implementation round
starts a fresh root, so the implementor never sees the planning conversation.
Review forks from the plan hand-off, so the reviewer sees the goal, the plan and
its own earlier findings, never implementation chatter. Nothing is deleted and
every phase stays reachable from the rewind picker.

### Tools per phase

| Phase | Tools |
|---|---|
| planning | `read grep find ls bash git_status git_log git_show git_diff`, `agent_*` when registered, `ask_user`, `dev_implement` |
| implementing | the tool set saved at `/plan`, plus `dev_review` |
| reviewing | as planning, with `dev_revise` + `dev_complete` instead of `dev_implement` |

The planner and reviewer get no `write` or `edit`. They do keep `bash`, so
"read-only" is a property of the tool set rather than a guarantee: shell writes
are still gated by the permission barrier exactly as anywhere else
(`permissions.md`). `ask_user` is enabled during planning and reviewing so the
planner can put a genuine design fork to the user instead of guessing.

The control tools (`dev_implement`, `dev_review`, `dev_revise`,
`dev_complete`) are registered as one `/tools` group on `/plan` and
unregistered on every exit, so they never exist outside a workflow. They never
need approval: a rejected control call is an error result the model corrects
inside the same turn, and only a successful one ends it.

### Rounds, stopping and resume

Revisions are capped at 4 rounds. An implementation turn that errored or hit a
token/step limit is retried up to 3 times, then pauses in place for the user to
nudge or stop by hand. `/plan-stop` stops the workflow directly, restoring what
`/plan` saved and aborting a running turn first. Esc during a phase turn is
just the ordinary turn interrupt: an aborted turn does not advance the phase,
and an implementation turn cut that way is retried like any other failure.

Workflow state persists across restarts: resume puts the workflow back in its
phase, and a crash while parked at the gate puts the draft plan back in the
editor so the next arbitrary prompt cannot silently become the plan of record.

### Status display

Phase transitions surface a divider and a notice, and a `plan` status segment
shows the phase and round (like `plan r2/4`). `/plan-status` reports the phase,
round and both model keys on one line. Phase changes change what the *model*
sees, never the screen: nothing is redrawn or replayed. Automatic compaction
inside a phase keeps that phase's focus, so a summary of an implementation
round stays about implementation.

## See also

- `permissions.md` - permission modes, approval dialogs, safe/denied matching
- `mcp.md` - mcp.json configuration, read-only marking, `/mcp` and `/tools`
- `commands.md` - every slash command and the input-line grammar
- `config.md` - every config.json key, its layer, and `AJENT_*` bindings
- `models.md` - models.json providers, discovery, model declarations
- `sessions.md` - naming, resume matching, rewind/fork, compaction
