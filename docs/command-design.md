# Command design

How `pkg/command` and `pkg/refs` own everything the user does that is not "send
a message to the model": slash commands through a registry, direct `!` shell
execution with staged results (`!!` runs excluded from context), `@`-path
expansion with auto-read, and the completion driving both. It is the single
dispatch path for submitted lines, and extensions and MCP register into the same
registry, so a built-in `/tools` and a third-party `/plan` are the same
mechanism.

## What it is

Three packages sit above the agent, tools and TUI: `pkg/command` (commands,
the registry, the `Console` interface and shell staging) plus its completion,
and `pkg/refs`, which owns `@`-reference expansion and the per-directory path
index. The UI never imports command, and the host adapts `Console` onto `*tui.UI`, so
headless mode and a sub-agent drive the same commands behind a different front end.

## Dispatch

A submitted line is classified once into a small kind set (prompt, command or
shell), then routed by the host's loop.

`ParseLine(s)` covers every escape hatch in the spec:

| Input | Kind | Rest |
|---|---|---|
| `hello world` | prompt | `hello world` |
| `/model gpt` | command | `model gpt` |
| `//literal` | prompt | `/literal` |
| ` /notacommand` | prompt | `/notacommand` (leading space escapes) |
| `!go test ./...` | shell | `go test ./...` (staged onto context) |
| `!!echo hi` | shell | `echo hi`, excluded from context (`Excluded`) |
| ` !echo hi` | prompt | `!echo hi` (leading space is the only literal-`!` escape) |
| multi-line submission opening with `/name args` | prompt (`Multiline`) | verbatim, plus a notice (a paste is never consumed by one command dispatch) |

Unknown `/foo` still parses as a command so dispatch can notice it rather than
prompting the model: a typo should not cost tokens. One exception guards pasted
content: a submission carrying a newline and opening with `/name args` falls
back to one verbatim prompt (`Multiline`, surfaced with a notice) whether or not
the name is known, so a command dispatch never consumes a paste. Multi-line `!`
is unaffected and stages the whole block as one run. The host's loop then routes
by kind: shell lines go straight to the `Stager` (non-blocking), while command
and prompt lines go to a single **prompt pump** goroutine that owns ordering.

### The prompt pump

`pkg/app`'s loop is a thin classifier feeding one pump channel:

- `ParseLine` routes shell lines to `Stager.Run` (non-blocking), while command and prompt
  lines go to the single pump goroutine that owns ordering.
- A prompt flushes staged shell results, expands `@` refs, then either queues
  onto the steer queue (rendering as a dimmed row with its echo deferred until it
  lands) or starts the drain directly and echoes immediately.
- Console state writes (`/model`, `/reasoning`, a compaction-threshold edit)
  queue while a drain goroutine owns State. The pump lands them at its next
  dispatch, and a drain exit sends a **wake line** so they apply even when no
  further input arrives. Writes are pump-goroutine confined, which keeps them
  ordered against turn starts and each other without locking `State`.

The steer queue turns mid-turn prompts into **steering messages**: they deliver
as newline-joined messages (one per provenance run, see below) at the agent's
next step boundary via `Options.OnBoundary`, or (if no boundary comes first) as
the next turn's prompt. Esc/Ctrl+C during a turn
recovers every queued item back into the editor (collapsed with newlines)
before interrupting. Alt+Up recalls the newest queued message. Staged shell
output is the stager's until a prompt delivers (see "Delivery timing"), so
recall and abort never strand it. Image blocks ride along with whatever is
queued. Their slots
were consumed at submit, so recall and abort refile each one under a fresh
token and the recovered line still names its `[image #N]`. See
`agent-loop-design.md` for the boundary contract. While the plan workflow parks
at its gate, delivery holds instead (see `plan-design.md`).

The pump supplies the **typing hold** (`Options.AwaitInput`, specified in
`agent-loop-design.md`): a mid-turn submit delivers into the **current** step
rather than behind another model call, instead of waiting out the boundary. The
same gate holds approval dialogs while a draft is composed.

A workflow that needs to act on a submission before it becomes a turn hooks the
pump through `planHooks.beforePrompt`, consulted only when the steer queue
declines the input, so mid-turn typing still steers normally and the hook only ever fires
with the agent idle, where switching branches is legal. It may rewrite the input
(the estimate is recomputed) or leave it alone. The matching `planHooks.advance`
runs after every turn. Both are documented in
`agent-loop-design.md` and used by `plan-design.md`.

Submissions therefore stay in order, the UI never stalls, and "the turn is held
until the pending command finishes" falls out of `Flush` blocking the pump. Only
prompts flush: `!ls` followed by `/model` leaves the stage pending for the next
real message. The CLI seed (`ajent "explain @pkg/app"`) goes through the pump
too, so its references expand like any other prompt. The headless one
(`ajent -p …`) expands in `runHeadless` instead, once the tool scope has
settled.

Joining a batch joins its resolvers too: `steerQueue.join` chains every queued
item's `After` into one, run in submit order behind the joined message. The
batch splits into **one input per contiguous provenance run**: items sharing an
`Injected` value merge into a single newline-joined message, while a provenance
change starts a new input. A batch mixing user text with system-injected
context therefore keeps per-item attribution on the transcript rows, so recall
excludes exactly the injected messages and replay renders them as such, instead
of one OR-ed flag labelling the whole batch.

## Commands

A command is a named, described handler: its name and one-line description for
`/help`, an optional usage hint, an optional argument-completion func, and a
handler that runs the parsed arguments against the `Console` view, returning an
error.

`Complete` is for *argument* completion: past the first space the matched
command supplies its candidates. `/model` supplies model keys and aliases,
`/reasoning` supplies `llm.Levels()`, and a command with no completer offers
nothing.

### Registry

The registry holds commands in registration order. Register is add-or-replace so
it can be widened to cover more behaviour. The registration order is preserved
for `/help` and command completion, and lookup by name plus listing are the only
read paths.

### Console

`Console` is a command's view of the world: an interface, not a struct, so the
extension host can back it with its own protocol. It exposes notices and a
markdown printer (`/help` renders through it), blocking interaction methods
(pick from items, select from options, confirm yes/no, free-text input), a
clipboard writer (the one write path behind `/copy`, see
`clipboard-copy-feature.md`), read handles onto the model registry, agent
state, tool registry, command registry and resolved config, settings writers
that save to a layer or record a session override, mutators that apply and
persist model, reasoning and tool-set changes, plus dispatch-state reporting
and exit.

`pkg/app` implements it once (`uiConsole`) over the objects the driver already
holds. Every mutator applies to the live object **and** records what resume
needs (a transcript entry or a session override), so a change made through a
command survives restart and shows in `/settings` as session-scoped.
`SetModel` recomputes only the live effective reasoning level for display,
leaving the stored override untouched so intent survives switching back. A
state mutator typed mid-turn **queues** rather than refusing: the pump holds
the write while a drain goroutine owns State, lands it in submission order once
the drain exits, and announces the pending change with a notice. Nothing about
a change is recorded until it applies. `/copy` reads through `SettledMessages`,
the one read seam over that same gate: the live transcript at rest, or the copy
the last turn end settled while one streams, so a partial turn is never copied.
The permissions-mode row applies its parsed mode to the live barrier for this
run only and records no `setting_change`, so every start uses the resolved
config default.

`Dispatched()` is owned by the pump and flips true on this run's first prompt,
never on a command or a `!`. It stays false on a resumed session until then,
so `/tools` selection opens free despite existing history.

### Built-ins

| Command | Behaviour |
|---|---|
| `/help` | markdown list of commands and keybindings through `Console.Print` |
| `/usage` | render the session's token ledger as markdown (turns, input/output, per-model breakdown; sub-agent spend called out when nonzero) |
| `/copy` | copy the last agent response to the clipboard through the shared writer (see `clipboard-copy-feature.md`), including text plus tool call/result JSON verbatim. Mid-turn it copies the last settled response, never a partial stream. No agent messages yet is a notice |
| `/model [name]` | resolve by name, or open the picker; `SetModel` announces the change as a notice and records a model-change entry, then the key is saved to the user config so a fresh start keeps it; a no-op when the key is unchanged (so nothing is rewritten), and its picker runs silent so only that one line lands (see `tui-design.md`) |
| `/reasoning [level]` | report, or set/clear the level for capable models. A model with fewer than two options reports its situation inline and never opens a picker |
| `/tools` | multi-select, grouped by source. Free selection before this run's first prompt (a resumed session preselects its recorded set), widen-only after |
| `/session [name]` | report the session's name, or set it; an invalid or conflicting name is a notice (see `session-design.md`) |
| `/settings [section]` | two-level menu of rows showing value + source layer, each edited in place with save-to-layer (see `config-design.md`). Rows come in three kinds: enum from a fixed set, model picker, and integer with min/max validation for sub-agent settings. A currently impossible choice (Reasoning on a single-option model) renders grayed out with the reason, as does Reset overrides when nothing is overridden |
| `/agents [list\|stop <id>\|all]` | list every running/finished investigation, or cancel one by id (the bare number works) / all; unknown verbs warn. Esc never cancels jobs; this is the only stop path |
| `/update` | reinstall ajent from the latest release in the background: resolves `@latest`, reinstalls when it differs, and reports updated-from/to, up-to-date or an error as a notice. The result is UI-only, never written to the transcript nor sent to the model (see below) |
| `/plan [goal]` | start the two-model plan → implement → review workflow: pick a planner, the active model implements (see `plan-design.md`) |
| `/plan-stop` | end the workflow and restore the model and tool set `/plan` found |
| `/plan-status` | report the phase, round and both models |
| `/init` | survey the project and write `AGENTS.md` (see below) |
| `/exit` | quit |

`/help`, `/model`, `/reasoning`, `/usage`, `/copy`, `/compact`, `/tools`, `/session`,
`/mcp`, `/agents`, `/settings`, `/update` and `/exit` are the built-ins;
`/plan*` and `/init` are feature commands the driver adds on top.

Registration goes through one helper in `pkg/app` that always calls
`RegisterBuiltins` first and then any feature commands. Feature registration
must never replace the built-in call, since doing so silently drops every
built-in command, and a `pkg/app` test asserts the built-in set survives with
and without workflow commands.

`command.PickModel(ctx, console, title, current, opts)` is the shared picker
behind `/model` and `/settings`, exported so a feature can offer its own model
choice under its own title rather than reimplementing the list.

### `/init`: the project survey

`/init` is the one command whose work outlives its handler. `initController`
(`init.go`) starts the survey on its own goroutine and returns immediately, so a
run that takes minutes never stalls the pump. It is refused while a turn streams
and while another survey is in flight. `Esc`/`Ctrl+C` cancels one, stopping the
children **it** spawned by id via `Options.Started`, never `StopAll`, because
the user may have started a turn during the survey whose own investigations must
survive (a returning `Poll` does not end a job). A duration ceiling bounds the
whole run so a wedged child cannot hold the single survey slot until restart. A
spent ceiling reports `ErrSurveyTimedOut`, which the controller treats like an
abort: stop the survey's remaining children by id, then tell the user.

**Slicing.** The agent count scales with the tree size, and top-level
directories are bin-packed largest-first into the least loaded buckets, so a
repository whose code all lives under `pkg/` still divides its work instead of
handing one agent everything. A directory larger than one slice's share is
replaced by its own children first. Files already read by stage 1 and the build
agent are dropped, and loose files left at a level collapse into one named unit.

`pkg/projinit` does the work and spends no model tokens: it runs the real
`read`, `agent_start` and `agent_poll` tools through `Registry.Lookup`,
collecting their genuine call + result pairs (the same `agent.InjectPair`
mechanism `@` references use). One deliberate exception tempers that Lookup:
sub-agent tools are checked against the enabled set before anything spawns.
Unlike a staged shell line, `/init` implicitly spends provider tokens on child
turns, so a user who disabled the "subagents" group gets those children reported
as disabled rather than spawned anyway. What comes back is an `agent.Input`
whose `Before` is that survey and whose `Text` is the distillation instruction
(`prompt-design.md` owns the wording). The controller hands it to the pump as a
pre-assembled input, which skips `@` expansion and uses its rest field as a
short echo label, and everything after is the ordinary prompt path: steer queue,
plan hooks, ledger seeding, turn start. The model then writes `AGENTS.md` with
the `write` tool, so the permission barrier gates it exactly as it gates any
other write, with no `tools.WithUserInitiated`, which would bypass the gate. A
survey that cannot run reports *why*: a missing `read`, missing or disabled
`agent_*`, spawns that were refused, and an existing `AGENTS.md` that could not
be read are distinct errors, never one "unavailable".

**Call ids carry a run number.** The uniqueness contract for host-run tool
calls and what a repeated id costs are specified in `agent-loop-design.md`.
Re-running `/init` is the advertised regenerate path, so a fresh controller per
process seeds its runner above any survey ids replayed from the transcript,
mirroring how `@` references are seeded.

Instructions are read once at startup, so a freshly written file applies on the
next start. `initWatch` is armed by the pump (arming it from the survey
goroutine could be consumed by a turn already running) and reports once the
file's stamp actually moves, which also covers coarse-mtime filesystems. It
stays armed across turns that wrote nothing, so a survey queued behind a
running turn still reports, and stays quiet when the write was denied.

### `/update`: self-update

The handler spawns one goroutine and returns immediately so a slow network
fetch never blocks the pump, and `Notify` is safe from any goroutine, so the
result lands as a notice when it finishes. A repeat while one runs just starts
another install of the same latest version. Shared logic lives in
`pkg/version` next to the build version it compares against: resolve latest,
install only when it differs, report failure as an error notice.

The `--update` flag is the opposite surface: it runs the same update in the
**foreground** at startup and exits after printing the result, never opening a
TUI or starting a session. Both surfaces report through `UpdateResult.Notice`,
and neither writes to the transcript nor reaches the model, because an update
is a build concern, not conversation content.

### `/tools` and the enabled set

The set only ever *widens* once this run has dispatched a prompt, because the
tool block the model has already seen is not retractable, since history may hold
calls to a tool, and dropping it would leave the transcript describing a tool
that no longer exists:

- **Before this run's first dispatched prompt** the picker lists every
  registered tool (`Registry.All`) and the selection is free: enable or disable
  anything. A resumed session sits in this window too, preselected with its
  recorded frame through the same restore paths a resume uses (see
  `session-design.md`). Any change records a fresh `tools.enabled`, so the next
  resume takes the reshaped set.

The plan workflow is the deliberate exception: a phase scope *narrows* the set
by calling `Registry.SetEnabled` directly. The widen-only rule is a
command-layer policy protecting the user from a tool set shrinking under them, and a
plan scope is explicit, user-initiated, and restored on every exit path.

Widening the set changes the tool block and therefore busts the prompt cache
(worth a one-line notice). `/tools` groups rows by `Registry.Source` (builtin,
an MCP server name, an extension name) with a dim header row between groups.
Picker key handling is specified in `tui-design.md`.

## Shell mode (`!`)

`command.Stager` owns staged runs: constructed over the tool registry and a
sink, it starts a command immediately (non-blocking), reports whether any run is
pending, cancels every in-flight run, and flushes, waiting for each included run
to finish then returning them as one user message each.

`Run(cmd, excluded)` resolves `bash` through `Registry.Lookup` (state-independent,
so disabling `bash` for the agent never blocks the human's own shell). Only a
registry without bash at all is a notice. It mints a call id, opens the display
with `sink.ToolStart` and executes on a goroutine through `agent.NewOutput`. That
reuses the whole `bash` path (streaming, ANSI stripping, output cap, spill file,
process-group kill) with no second implementation. It wraps its context in
`tools.WithUserInitiated`, so a staged `!` line is exempt from every permission
mode: this is the human's own shell, not the model's. It also never inherits
the model-facing timeout default: the stager's ceiling is its own (zero runs
uncapped), so a long human command like a build is not silently killed. The
goroutine executes through `agent.RecoverTool`, the same helper an agent-run
tool takes, so a panic still stages its fault text as the run's output instead
of wedging the row.

The display differs from an agent bash call in one way: because the output is
the user's own, it must be shown whole. The stager calls `ToolStart` with `full`
set, so the sink puts the output head in full mode and every line reaches
history instead of collapsing under the head-plus-summary rule (see
`tui-design.md`). The model-side bytes are unchanged: context still carries
whatever the bash tool returns, cap marker included.

**Staging onto context.** The command text and its result are *not* sent to the
model immediately. They sit in the pending slot ahead of the next user message.
Running `!` never interrupts an in-flight turn, and it only becomes visible to the
model when the user next speaks. It is not free, though, and the context bar
says so: as each run finishes the stager reports the staged total upward. That
term behaves like the editor buffer (`composing`): it survives a rebase or a
model switch, because staged output has not ridden any request yet. Excluded (`!!`)
runs and still-running ones count zero: the former never reaches context, the
latter has no result to measure.

**Discarding.** A rewind switches to a different branch point, so anything
staged against the branch just left must not ride the new one's first prompt.
`rewind` calls `Stager.Discard` after `switchState` succeeds: it cancels every
in-flight run, drops the staged set and reports zero back to the bar.

**Delivery timing.** Staged output only leaves the stager when a prompt
delivers, never when it is submitted. A direct dispatch (no turn running, no
workflow gate parked) flushes at submit, since submit and delivery are the same
moment. A queued submit consumes nothing, and a submit held at a workflow gate
queues as a row rather than delivering: it consumes nothing too. Finished
results attach at delivery, so an Esc or Alt+Up that recovers the prompt to the
editor leaves staged output untouched, still counted on the context bar, waiting
for whichever prompt delivers next. A queued batch first waits out still-running commands, so output
the user watched finish rides the batch it was meant for, and an abort during
that wait consumes nothing. Multiple staged commands flush together, in
submission order, each as its own **user** message. A run stays tracked for
interruption until consumed, so an included command still running when delivery
begins can be cancelled rather than freezing the front end.

**Representation.** A completed run lands as a single `llm.RoleUser` text
message, not a synthetic tool-call pair: it reads as the human's own action
rather than an agent-initiated bash call. The body is built verbatim from the
real `bash` tool's result text (`shellUserMessage`), with a placeholder when
there is no output (the shape is owned by `prompt-design.md`). It is honest
because it reuses the same bytes the model would see from a real call, with the
exit-status, interruption and truncation markers already baked in by the tool
itself. The message is appended as **injected** context (see
`agent-loop-design.md`), so prompt recall excludes it, and dropping the synthetic
pair also removes the Anthropic duplicate-`tool_use`-id hazard for staged runs.
It is also marked **replayed**, so a resume or rewind redraws it above the
prompt it fed rather than leaving the model holding something the user can no
longer see. That mark is stamped on every injected text message at the append
site (see `agent-loop-design.md`), so staged runs and steered notices restore
alike. Once flushed, `Flush` reports the staged total back to zero: the
submission's estimate owns those tokens from then on.

**Excluded runs (`!!`).** `Run(cmd, true)` executes and displays exactly like a
staged run (same bash path, same streaming sink, same barrier exemption), but
its output is never staged onto model context or the transcript. Because that
output goes nowhere, `Flush` does not wait on an excluded run: finished ones are
dropped, still-running ones stay tracked so `Pending()`/`Cancel()` keep working
while they stream to the sink on their own goroutine.

**Error behaviour.** A non-zero exit code is an ordinary staged result, not a
turn failure: the model sees the failure (with the exit code) at the next
message. `!` alone or an empty command produces a notice and runs nothing. A
literal line beginning with `!` leads with whitespace (` !echo hi`), and a literal
`!!x` prompt is ` !!!x`.

**Cancellation.** Each staged run owns a cancellable context, released the
moment its result is captured: a finished run must not stay registered as a
dead child of the app root, or every `!`/`!!` line leaks one until process
exit. `Cancel` cancels every still-running run; cancellation is idempotent, so
`Discard`/`Cancel` firing on an already-released run is a no-op. The `bash`
tool kills the whole process group when its
derived context ends. A cancelled command still stages (if included): its
partial output plus the killed-status the bash tool records, so the model's view
matches what the user saw. Excluded runs cancel identically but their partial
output goes nowhere. `Ctrl+C`/Esc cancels an in-flight staged command through
the host's interrupt path. Included runs are not removed from tracking until
Flush consumes them, so this holds even while a flush is blocked on one.

> Staged `!` results live only in memory until the next prompt flushes them. If
> the process exits before that flush, the staged results are lost: they are
> not part of the transcript until the model sees them.

## Completion

Two presentations, one mechanism. `CompleteStyle` picks between them per cursor
position, so a source decides how each of its contexts reads:

| Context | Presentation |
|---|---|
| `/` at a line start, and a command's arguments | **menu** — opens as you type, `↑`/`↓` pick, `Tab`/`Enter` accept |
| `@` anywhere, and a `!` / `!!` line | **Tab-driven** — nothing until `Tab`, which fills the common prefix |

The split is the size of the vocabulary. A command set is small, closed and
worth advertising, so its menu is how you learn what exists. The filesystem is
neither, and a directory that pops a list on every keystroke was what made the
arrows unusable for editing.

The completion model pairs a replacement text with an optional label, detail and
score. The `Completer` interface answers one query: given the buffer and cursor,
it returns where accepted text should start replacing from plus the candidate
items. It must not call back into the UI.

A path or shell query may take time (a slow directory listing, a subprocess), so
`CompleteStyle.Async` routes it off the key loop and applies the result when it
lands. A result is dropped when a newer Tab superseded it or the buffer moved on
while it ran, so a slow listing can never rewrite text the user has since
edited. Menu contexts are synchronous by contract.

### Menu rules

A menu is re-queried on every keystroke under the UI lock, so `Menu` is a
promise that the source answers promptly and never re-enters the UI. A source
that cannot must use `Async` instead. Key handling for an open menu (cycling,
accept versus submit-as-typed so an open menu never swallows a send,
dismissing) is specified in `tui-design.md`.

### Tab rules

The tab-driven overlay holds no selection and consumes no keys beyond `Tab`.
`Tab` fills in the candidates' longest common prefix and stops there, never
guessing past an ambiguity (bash's behaviour), so a lone candidate completes in
full. A `Tab` that cannot advance lists the remaining candidates as passive dim
rows instead of inserting anything, and everything else reaches the editor
untouched.

### `command.Completer`

The aggregating completer sources commands from the registry and paths from
`refs.Index`. `/` at line start offers command names, and past the first space it
delegates to the matched command's `Complete`. `@` anywhere offers workspace
paths. A nil path index disables path completion (plain mode).

One `classify` call decides which of those the cursor is in, and both `Complete`
and `Style` dispatch on its result, so how a context presents can never disagree
with the query that then runs. It returns a `completeCtx` carrying the grapheme
cells, the clamped cursor and where the context begins, which every branch
slices rather than rescanning the buffer.

`NewPathCompleter` is the narrow sibling: an `@`-token-only source for free-text
surfaces that take no commands or shell line, such as the approval dialog's
allow/deny reason. It answers through the same `atComplete` core on the same
`refs.Index` as the prompt completer, and a parity test pins the two equal on
every `@` shape (workspace-relative, `../`, `~/` and absolute), so a free-text
surface can never answer differently than the prompt would.

### Shell completion

A leading `!` routes the whole line to shell completion, so an `@` inside it stays
the literal text bash will receive rather than a workspace ref.

- **Command names.** The first word of the line, and of each piped or grouped
  segment, completes against what a non-interactive `bash -c 'compgen -abck'`
  reports (builtins, keywords and PATH), looked up once with a timeout. A
  multi-line `!` block stages as one run, so every line of it gets the same
  treatment: a newline starts a command like any separator. An empty token offers
  nothing rather than every command on the system, and an unavailable bash yields
  no command names.
- **Paths.** Everything else, plus any first word containing `/`, completes through
  the same index used for `@` refs, so one read-per-step listing serves both
  surfaces. Quoting, escapes and variable expansion are not handled: the token
  under the cursor completes as-is.

## `@` file references

### Parse (`refs/parse.go`)

`Parse(text)` returns `Ref{Path, Start, End, Note}`. `Note` carries any existing
size/line measurement absorbed into the token, and that absorption is what makes
re-expansion idempotent.

`@` matches only at a word boundary (start of line, or after whitespace, `(`,
`[`, a backtick or a quote) so `email@example.com` is prose. The path token
stops at whitespace or trailing punctuation, though a final `.` and `:` stay
inside the token because dotted names and `x:y` need them. Whether they ended
a sentence is decided at expand time. The annotation shape is matched
strictly: a trailing `(...)` is absorbed only when it contains a digit or a
known kind word (`binary`, `image`, `dir`, `lines`) and is made of the allowed
tokens (digits, `b`/`kb`/`mb`, the kind words), so ordinary parenthetical prose
after a path is never eaten.

### Expand (`refs/expand.go`)

`Expander.Expand(text)` **plans**: it does not read. It returns the rewritten
text, an `Est` sizing what the plan will add, notices, and a `Run` closure for
`Input.After`. Per distinct resolved path (via the tool `PathPolicy`):

| Case | Outcome |
|---|---|
| wildcard pattern (contains `*`, `?` or `[`) | `ls` pair planned via `Registry.Lookup`, listing the matching files so the model sees which paths matched before choosing what to read; the pattern stays literal in prose |
| missing (retried once without a trailing `.` or `:`, see below) | literal, warning notice naming the written token |
| directory | `ls` pair planned via `Registry.Lookup` (ignores enabled state); skipped when the tracker's listing is unchanged (`UnchangedDir`) |
| already read whole and unchanged (`Tracker.Unchanged`) | nothing planned, literal |
| a path the same message already named | planned once; the repeat stays literal |
| text file within `RefInject` and under the running `RefTotal` cap | `read` pair planned, stale annotation stripped |
| image on a vision model | `read` pair planned, so the image arrives through the ordinary read path |
| image without vision, large, binary, or over the cap | annotation replaced in place, cap trim adds a notice |

Resolution is existence-aware about sentence punctuation: because Parse keeps
a trailing `.` or `:` inside the token, a written form that names nothing real
is retried without it. The trimmed reading wins only by existing, so `./x`,
dotted names and `x:y` resolve exactly as written. Prose, notices and
annotations keep the written token while read/ls receive the trimmed path.

The executed plan runs in document order, so injected reads appear in the same
order as the prose that named them.

Because planning no longer reads, the tracker cannot dedupe within one expansion
(nothing has been observed yet), so **the plan dedupes by resolved path itself**
and `Run` re-checks `Unchanged` per injection before executing it. Dedupe runs
before the `RefTotal` budget, so the cap counts only newly planned reads: an
unchanged or repeat mention reserves nothing and is never annotated by the
total cap (only by the per-file `RefInject` limit, which is about the file's
shape, not the budget). The first guard
keeps one message from naming a path twice (which would duplicate its content
*and* its call id), and the second keeps a joined batch from re-reading what an earlier
item in the same batch just read. `Run` also stops on a cancelled context rather
than appending error pairs for the rest of the batch.

### Reads land behind the message

`Run` is `Input.After`, so the pairs are appended **after** the user message
that named them. That placement is what makes rewinding onto an `@` message
drop its reads with it, and it fixes the display: the tools run when the
message lands, after its echo, so what is on screen is the order replay renders
(the asymmetry behind both is specified in `agent-loop-design.md`,
`Input.After`).

That makes re-injection routine, so ids must not repeat. Each `Expand` takes a
run number and mints a unique reference id for it (naming its tool too, so read
and listing references can never collide), and `Expander.Seed(msgs)` raises the
counter above every id already present on rebuild. The same rebuild calls
`tools.Tracker.Reset`, since tracker records describe the process rather than
the context (`tools-design.md`): without it a rewound-away read would still
count as "already in context" and the resubmit would inject nothing. Both hang
off the one rebuild hook every path that replaces `State.Messages` owes (see
`session-design.md`). Headless seeds its own expander from the rebuilt state,
since `--continue` reopens a transcript that already holds ref ids.

Injected tools run through the same sink as the stager, so the display order
matches the transcript order and therefore matches what replay renders.

### Idempotence

Annotation is idempotent. The annotated text is what lands in history, so a
rewind that refills the editor, or any other resubmission of an earlier message,
sends text that already carries an annotation. `Parse` absorbs the existing
annotation into `Note`, and `Expand` *replaces* it with the fresh measurement
rather than appending a second one.

### Index (`refs/index.go`)

`Index` never walks the workspace tree: every `@` query lists only the single
directory under the cursor, so completion is one cheap `ReadDir` however large
or slow the filesystem. A bare `@` (or a partial name like `@ma`) offers the
root's immediate children. Drilling through a trailing `/` (`@src/`, `@pkg/r…`)
descends one level at a time, never a recursive walk, which is what made `@`
block input after typing in a large repo. The dependency/VCS skip list still
applies per directory. A `~` or `~/…` query completes within the user's home
directory instead, offered back with the leading `~` kept in each candidate so
accepting a home path inserts one that expands. A `./` query keeps its prefix,
and an absolute `/…` path completes anywhere on the filesystem. All three keep
their leading form so accepting inserts a usable path. Because a listing may
still take time on a slow mount, `@` queries run off the key loop (see
Completion): a Tab never stalls the editor and a stale result is dropped.
Ranking is (a) already in the conversation (`Tracker.Records()`), (b) recent
mtime, (c) fuzzy score (reusing `tui.MatchScore` rather than a second
implementation). Directories complete with a trailing `/` and keep completing.

### Measure

Annotation measurement is bounded by construction: stat first, count lines only
for small text files, report bytes plus kind above the ceiling without reading
content. Annotating a giant file never reads it whole.

## Pasted content

Large pastes (above a size threshold) are stored on the UI and the editor
receives a placeholder naming the paste. The placeholder is what `editor.Submit`
records in history (that is the point), and `applyKey` expands placeholders back
to their content just before sending. The map is kept for the session so
recalling a pasted line from history still expands.

## Editor history

The editor's line history is owned by `pkg/session`, not here: main appends
every submitted message (prompt, `/cmd` and `!shell`, multi-line pastes included
as one unit) to the workspace store at submit time, before `ParseLine` dispatch,
so it persists whether or not a transcript/recorder is active. Ctrl+R walks the
workspace-wide recall store (`session.RecallIndex`); plain ↑/↓ scope to the open
session's own transcript (`session.TranscriptIndex`), falling back to the
Ctrl+R scope until it has prompts. The storage format, secret-marker exclusion
and compaction are documented in "Editor history" under `session-design.md`.
