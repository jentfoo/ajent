# Command design

How `pkg/command` and `pkg/refs` own everything the user does that is not "send
a message to the model": slash commands through a registry, direct `!` shell
execution with staged results (`!!` runs excluded from context), `@`-path
expansion with auto-read, and the completion driving both. It
is the single dispatch path for submitted lines; extensions and MCP register into
the same registry, so a built-in `/tools` and a third-party `/plan` are the same
mechanism.

## What it is

Three packages sit above the agent, tools and TUI:

- `pkg/command` — `Command`, the registry, the `Console` interface, the
  built-in commands, the shell `Stager`, and the aggregating `Completer`.
- `pkg/refs` — `@` parsing, the inject-or-annotate expander, and the
  gitignore-aware path index for completion.
- completion in `pkg/tui` — a live menu for commands, and a Tab-driven fill for
  paths that leaves the arrows to the editor.

`pkg/command` imports `pkg/tui`, `pkg/agent`, `pkg/llm`, `pkg/tools` and
`pkg/session`; it never imports `pkg/refs` (the host wires the two together
through the `Completer`). The UI does not import the command package — the host
adapts `Console` onto `*tui.UI`, so headless mode and the sub-agent
can drive the same commands with a different front end.

## Dispatch

A submitted line is classified once into a small kind set — prompt, command or
shell — then routed by the host's loop.

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

Unknown `/foo` still parses as a command so dispatch can notice it rather than
prompting the model — a typo should not cost tokens. The host's loop then routes
by kind: shell lines go straight to the `Stager` (non-blocking); command and
prompt lines go to a single **prompt pump** goroutine that owns ordering.

### The prompt pump

`main.go`'s loop is a thin classifier feeding one pump channel:

- loop: `ParseLine` → shell to `Stager.Run` (non-blocking); command and prompt
  lines to the pump; the `quit` case is unchanged. The echo of a submitted
  prompt now lives in the pump, so a mid-turn submission renders as a queued row
  instead of an immediate echo.
- pump: a command runs its handler (pickers block only the pump); a prompt
  `Stager.Flush` → `refs.Expand` → queue-or-start via the **steer queue**
  (`queue.go`). Flush yields `Input.Before`; expansion yields `Input.After`, whose
  reads run only when the message lands. A `pumpLine` carrying an already-assembled
  `input` (the `/init` survey) re-enters here with its `Before` intact, skipping
  expansion and the echo so the same ordering, accounting and hook path still
  apply. Idle, it spawns the single drain goroutine with this input;
  busy, it queues the item (rendering as a dimmed row) and defers its echo to
  delivery.

The steer queue turns mid-turn prompts into **steering messages**: they deliver
as ONE newline-joined user message at the agent's next step boundary via
`Options.OnBoundary`, or — if no boundary comes first — as the next turn's prompt
drained by `startDrain`. Esc/Ctrl+C during a turn recovers every queued item back
into the editor (collapsed with newlines) before interrupting; Alt+Up recalls the
newest queued message. See `agent-loop-design.md` for the boundary contract.

A workflow that needs to act on a submission before it becomes a turn hooks the
pump through `planHooks.beforePrompt`, consulted **after** `q.offer` returns
false — so mid-turn typing still steers normally and the hook only ever fires
with the agent idle, where switching branches is legal. It may rewrite the input
(the estimate is recomputed) or leave it alone. The matching `planHooks.advance`
runs in `startDrain` after every turn; both are documented in
`agent-loop-design.md` and used by `plan-design.md`.

Submissions therefore stay in order, the UI never stalls, and "the turn is held
until the pending command finishes" falls out of `Flush` blocking the pump. Only
prompts flush — `!ls` followed by `/model` leaves the stage pending for the next
real message. The CLI seed (`ajent "explain @main.go"`) goes through the pump
too, so its references expand like any other prompt; the headless one
(`ajent -p …`) expands in `runHeadless` instead, once the tool scope has settled.

Joining a batch joins its resolvers too: `steerQueue.join` chains every queued
item's `After` into one, run in submit order behind the joined message.

## Commands

A command is a named, described handler: its name and one-line description for
`/help`, an optional usage hint, an optional argument-completion func, and a
handler that runs the parsed arguments against the `Console` view, returning an
error.

`Complete` is for *argument* completion: past the first space the matched
command supplies its candidates. `/model` supplies model keys and aliases;
`/reasoning` supplies `llm.Levels()`; a command with no completer offers nothing.

### Registry

The registry holds commands in registration order. Register is add-or-replace so
it can be widened to cover more behaviour; the registration order is preserved
for `/help` and command completion, and lookup by name plus listing are the only
read paths.

### Console

`Console` is a command's view of the world — an interface, not a struct, so
the extension host can back it with its own protocol:

`Console` is a command's view of the world — an interface, not a struct, so
the extension host can back it with its own protocol. It exposes notices and a
markdown printer (`/help` renders through it), the blocking interaction methods
(pick one or many from items, select from options, confirm yes/no, free-text
input for numbers and limits), read handles onto the model registry, agent state,
tool registry, command registry and resolved config, settings writers (save to a
layer, record a session override), and mutators that apply and persist a model or
reasoning change, announce tool-set changes, report whether any prompt has been
sent this session, and exit.

`main.go` implements it once (`uiConsole`) over the objects the driver already
holds. `SetModel` records a `model_change` entry the old bespoke switch never
did, persists the selection to the user config so a fresh start keeps it (see
config-design.md's Model section), and recomputes only the live effective reasoning
level for display — leaving the stored override untouched so intent survives switching back; `ToolsChanged` records a `setting_change("tools.enabled", names)` (dotted
config key) so resume keeps the set. The three interaction methods are one-line
forwarders to `tui.SelectContext`/`ConfirmContext`/`InputContext`, and the two
settings methods delegate to the resolved `*config.Set`. Each mutator also calls
`SetSession` with its config key so `/settings` reports the change as `(session)`.
`SetSessionSetting` applies a dotted session override *and* records it via
`recorder.SettingChange`, closing the old gap where a row called `SetSession`
directly and the override silently vanished on resume (auto-compaction, tool
limits). The bespoke `SetModel`/`SetReasoning` mutators keep their own paths. A
`permissions.mode` key additionally applies its parsed mode to the live permission
barrier and republishes the status segment — this is what makes `/settings`'s
Permissions enum row take effect rather than sitting inert, and it is also how a
Shift+Tab cycle records itself for resume.

`Started()` is owned by the pump: it flips true when the first prompt is
dispatched, never on a command or a `!`.

### Built-ins

| Command | Behaviour |
|---|---|
| `/help` | markdown list of commands and keybindings through `Console.Print` |
| `/model [name]` | resolve by name, or open the picker; `SetModel` announces `! model: <key>`, records a model-change entry and saves the key to the user config — a no-op when the key is unchanged, and its picker runs silent so only that one line lands (see tui-design) |
| `/reasoning [level]` | report, or set/clear the level for capable models |
| `/tools` | multi-select, grouped by source; widens the enabled set |
| `/settings [section]` | two-level menu of rows showing value + source layer; each row edits and offers save-to-layer (`see config-design.md`); generic `enumRow` (string key from a fixed set), `modelRow` (key through the model picker) and `intRow` (numeric key with min/max validation — sub-agent concurrency, since an enum stores a string that won't unmarshal into an int field) builders cover permission modes and sub-agent settings |
| `/agents [list\|stop <id>\|all]` | list every running/finished investigation as a markdown table (id, status, elapsed, task), or cancel one (`sub-2`, bare `2`) / all; unknown verbs warn. Esc never cancels jobs — this is the only stop path |
| `/plan [goal]` | start the two-model plan → implement → review workflow: pick a planner, the active model implements (see `plan-design.md`) |
| `/plan-stop` | end the workflow and restore the model and tool set `/plan` found |
| `/plan-status` | report the phase, round and both models |
| `/init` | survey the project and write `AGENTS.md` (see below) |
| `/exit` | quit |

`/help`, `/model`, `/reasoning`, `/usage`, `/compact`, `/tools`, `/mcp`,
`/agents`, `/settings` and `/exit` are the built-ins; `/plan*` and `/init` are
feature commands the driver adds on top.

Registration goes through one `registerCommands` helper in `main.go` that always
calls `RegisterBuiltins` first and then any feature commands. A feature that
registers its own must not be written where the built-in call lives: an earlier
plan-workflow attempt replaced that line and silently lost every built-in
command. A root-package test asserts the built-in set survives with and without
workflow commands.

`command.PickModel(ctx, console, title, current, opts)` is the shared picker
behind `/model` and `/settings`, exported so a feature can offer its own model
choice under its own title rather than reimplementing the list.

### `/init` — the project survey

`/init` is the one command whose work outlives its handler. `initController`
(`init.go`) starts the survey on its own goroutine and returns immediately, so a
run that takes minutes never stalls the pump. It is refused while a turn streams
and while another survey is in flight; `Esc`/`Ctrl+C` cancels one, stopping the
children **it** spawned — by id via `Options.Started`, never `StopAll`, because the
user may have started a turn during the survey whose own investigations must
survive (a returning `Poll` does not end a job).

**Slicing.** The agent count scales with the tree (under 50 files → 1, then 2, 3,
4), and the slices themselves are top-level directories bin-packed largest-first
into the least loaded bucket. A directory holding more than one slice's share is
replaced by its own children first, repeatedly — otherwise a repository whose code
all lives under `pkg/` hands one agent everything and the others nothing. Files
stage 1 and the build agent already read (`README*`, `AGENTS.md`, `Makefile`,
`CONTRIBUTING.md`, licences, lockfiles) are dropped, and the loose files left at a
level collapse into one named unit rather than a scatter of singletons.

`pkg/projinit` does the work and spends no model tokens: it runs the real `read`,
`agent_start` and `agent_poll` tools through `Registry.Lookup`, collecting their
genuine call + result pairs — the same `agent.InjectPair` mechanism `@` references
use. What comes back is an `agent.Input` whose `Before` is that survey and whose
`Text` is the distillation instruction (`prompt-design.md` owns the wording). The
controller hands it to the pump as `pumpLine{input: &in}`; `promptInput` takes that
branch, skipping `@` expansion and using `rest` as a short echo label, and
everything after is the ordinary prompt path — steer queue, plan hooks, ledger
seeding, `startDrain`. The model then writes `AGENTS.md` with the `write` tool, so
the permission barrier gates it exactly as it gates any other write; no
`tools.WithUserInitiated`, which would bypass the gate. A survey that cannot run
reports *why* — a missing `read`, missing `agent_*`, or spawns that were refused
are three different errors, never one "unavailable".

**Call ids carry a run number** (`init-<run>-read-1`). `Input.Before` is appended
to `State` and persisted, so a second `/init` in one session would otherwise
replay the first run's `tool_use` ids — and a duplicate id makes every later
Anthropic request fail permanently. Re-running is the advertised regenerate path,
so this is not theoretical.

Instructions are read once at startup, so a freshly written file applies on the
next start. `initWatch` (an `opts.Sinks` member) is armed by the pump — not by the
survey goroutine, whose arming a turn already running would consume — and reports
the change once the file's mtime actually moves. It stays armed across turns that
wrote nothing, so a survey queued behind a running turn still reports, and stays
quiet when the write was denied.

### `/tools` and the enabled set

The set only ever *widens* within a session, because the tool block the model
has already seen is not retractable — history may hold calls to a tool, and
dropping it would leave the transcript describing a tool that no longer exists:

- **Before the first prompt** the picker lists every registered tool
  (`Registry.All`) and the selection is free: enable or disable anything.
- **After the first prompt** the picker lists only `Registry.Disabled()`;
  selecting enables them via `Enable` (additive, vs `SetEnabled`'s replacement).
  Nothing can be turned off again for the rest of the session.

The plan workflow is the deliberate exception: a phase scope *narrows* the set
by calling `Registry.SetEnabled` directly. The widen-only rule is a command-layer
policy protecting the user from a tool set shrinking under them; a plan scope is
explicit, user-initiated, and restored on every exit path.

Widening the set changes the tool block and therefore busts the prompt cache
(worth a one-line notice). `/tools` groups rows by `Registry.Source` (builtin,
an MCP server name, an extension name), with a dim header row when the group
changes. Space or Tab toggles the highlighted row; typed text narrows the filter
(a space never becomes part of it) and Enter confirms.

## Shell mode (`!`)

`command.Stager` owns staged runs: constructed over the tool registry and a
sink, it starts a command immediately (non-blocking), reports whether any run is
pending, cancels every in-flight run, and flushes — waiting for each included run
to finish then returning them as one user message each.

`Run(cmd, excluded)` resolves `bash` through `Registry.Get` (enabled-only, so a
disabled `bash` is a refusal notice), mints a call id, opens the display with
`sink.ToolStart` and executes on a goroutine through `agent.NewOutput`. That
reuses the whole `bash` path — streaming, ANSI stripping, output cap, spill
file, process-group kill — with no second implementation. It wraps its context in
`tools.WithUserInitiated`, so a staged `!` line is exempt from every permission
mode: this is the human's own shell, not the model's.

The display differs from an agent bash call in one way: because the output is
the user's own, it must be shown whole. The stager type-asserts its sink for a
`ToolStartFull` capability (implemented by the TUI sink) and uses it when present,
failing back to plain `ToolStart`. That sets full mode on the output head so every
line reaches history instead of collapsing past four — see tui-design.md. The
model-side bytes are unchanged: context still carries whatever the bash tool
returns, cap marker included.

**Staging onto context.** The command text and its result are *not* sent to the
model immediately. They sit in the pending slot ahead of the next user message.
Running `!` never interrupts an in-flight turn — it only becomes visible to the
model when the user next speaks. It is not free, though, and the context bar says
so: as each run finishes the stager reports the staged total through its
`SetOnChange` hook, which main feeds to `Accounting.SetStaged`. That term behaves
like the editor buffer (`composing`) — it survives a rebase or a model switch,
because staged output has not ridden any request yet. Excluded (`!!`) runs and
still-running ones count zero: the former never reaches context, the latter has no
result to measure.

**Discarding.** A rewind switches to a different branch point, so anything staged
against the branch just left must not ride the new one's first prompt. `rewind`
calls `Stager.Discard` after `switchState` succeeds: it cancels every in-flight run,
drops the staged set and reports zero back to the bar.

**Flush timing.** On the next normal submit the pump first `Flush`es the stager
— which waits for every included staged command to finish, in submission order —
then injects each result ahead of the user message. The model therefore sees "here
is a shell invocation I ran and its output" together with whatever the user just
said. Multiple staged commands flush together, in submission order, each as its
own **user** message.

**Representation.** A completed run lands as a single `llm.RoleUser` text message,
not a synthetic tool-call pair — it reads as the human's own action ("User Ran: <cmd>")
rather than an agent-initiated bash call. The body is built verbatim from the real
`bash` tool's result text (`shellUserMessage`): `User Ran:` line, blank line,
`Output:`, then the combined stdout+stderr with any exit-status / interruption /
truncation marker already baked in by the tool itself — honest because it reuses
the same bytes the model would see from a real call. Empty output renders
`(no output)`. The message is appended as **injected** context (see
agent-loop-design), so prompt recall excludes it; dropping the synthetic pair also
removes the Anthropic duplicate-`tool_use`-id hazard for staged runs. It is also
marked **replayed**, the one kind of injected context that belongs in restored
scrollback: a resume or rewind redraws it above the prompt it fed rather than
leaving the model holding something the user can no longer see (see
session-design). Once flushed, `Flush` reports the staged total back to zero — the
submission's estimate owns those tokens from then on.

**Excluded runs (`!!`).** `Run(cmd, true)` executes and displays exactly like a
staged run — same bash path, same streaming sink, same barrier exemption — but its
output is never staged onto model context or the transcript. Because that output
goes nowhere, `Flush` does not wait on an excluded run: finished ones are dropped,
still-running ones stay tracked so `Pending()`/`Cancel()` keep working while they
stream to the sink on their own goroutine.

**Error behaviour.** A non-zero exit code is an ordinary staged result, not a
turn failure — the model sees the failure (with the exit code) at the next
message. `!` alone or an empty command produces a notice and runs nothing. A
literal line beginning with `!` leads with whitespace (` !echo hi`); a literal
`!!x` prompt is ` !!!x`.

**Cancellation.** Each staged run owns a cancellable context. `Cancel` cancels
every still-running run; the `bash` tool kills the whole process group when its
derived context ends. A cancelled command still stages (if included): its partial
output plus the killed-status the bash tool records, so the model's view matches
what the user saw. Excluded runs cancel identically but their partial output goes
nowhere. `Ctrl+C`/Esc cancels an in-flight staged command through the host's
interrupt path.

> Staged `!` results live only in memory until the next prompt flushes them. If
> the process exits before that flush, the staged results are lost — they are
> not part of the transcript until the model sees them.

## Completion

Two presentations, one mechanism. `CompleteStyle` picks between them per cursor
position, so a source decides how each of its contexts reads:

| Context | Presentation |
|---|---|
| `/` at a line start, and a command's arguments | **menu** — opens as you type, `↑`/`↓` pick, `Tab`/`Enter` accept |
| `@` anywhere, and a `!` / `!!` line | **Tab-driven** — nothing until `Tab`, which fills the common prefix |

The split is the size of the vocabulary. A command set is small, closed and worth
advertising, so its menu is how you learn what exists. The filesystem is neither,
and a directory that pops a list on every keystroke was what made the arrows
unusable for editing.

The completion model pairs a replacement text with an optional label, detail and
score; the `Completer` interface answers one query — given the buffer and cursor,
it returns where accepted text should start replacing from plus the candidate
items. It must not call back into the UI.

A path or shell query may take time (a slow directory listing, a subprocess), so
`CompleteStyle.Async` routes it off the key loop and applies the result when it
lands. A result is dropped when a newer Tab superseded it or the buffer moved on
while it ran, so a slow listing can never rewrite text the user has since edited.
Menu contexts are synchronous by contract.

### Menu rules

- The top candidate is highlighted from the moment the menu opens. **The marker
  is Tab's target, not Enter's** — Tab always accepts it, Enter only does once
  `↑`/`↓` have moved the selection.
- `↑`/`↓` cycle the highlight and mark the selection *moved*.
- `Tab` accepts the highlighted candidate; a following `Enter` just submits the
  result (e.g. `/model` then Enter runs it).
- `Enter` with a moved selection accepts it and submits; with the list merely
  offered it submits the line as typed, so an open menu never swallows a send
  the user meant. That is why the highlight cannot arm Enter on its own: the
  menu opens unbidden while typing, and a send must not change meaning because
  a list happened to appear.
- Any key that reaches the editor — a character, Backspace, or a caret move —
  drops the *moved* state, so Enter sends what was typed rather than re-applying
  a stale highlight.
- `Esc` dismisses without inserting.

A menu is re-queried on every keystroke under the UI lock, so `Menu` is a
promise that the source answers promptly and never re-enters the UI. A source
that cannot must use `Async` instead.

### Tab rules

- `Tab` fills in the candidates' longest common prefix and stops there, so it
  never guesses past an ambiguity — bash's behaviour. A lone candidate therefore
  completes whole.
- A `Tab` that cannot advance lists the remaining candidates as passive dim rows,
  packed into columns: no highlight, no selection, and every other key clears
  them. Narrowing the list means typing more.
- A `Tab` with nothing to add and nothing new to list leaves the buffer alone and
  flashes the rule above the prompt.
- A source free to return anything — a command's own `Complete` need not extend
  the typed text — has no meaningful common prefix, so its best candidate is
  taken whole.
- `Esc` drops a listing before it clears the buffer. Everything else, `Enter` and
  the arrows included, reaches the editor untouched.

### `command.Completer`

The aggregating completer sources commands from the registry and paths from
`refs.Index`. `/` at line start offers command names; past the first space it
delegates to the matched command's `Complete`. `@` anywhere offers workspace
paths. A nil path index disables path completion (plain mode).

One `classify` call decides which of those the cursor is in, and both `Complete`
and `Style` dispatch on its result — so how a context presents can never disagree
with the query that then runs. It returns a `completeCtx` carrying the grapheme
cells, the clamped cursor and where the context begins, which every branch slices
rather than rescanning the buffer.

### Shell completion (`command/shellcomplete.go`)

A leading `!` routes the whole line to shell completion, so an `@` inside it
stays the literal text bash will receive rather than a workspace ref.

- **Command names.** The first word of the line, and of each `|`, `||`, `&&`,
  `;`, `(` or `{` segment, completes against `bash -c 'compgen -abck'` —
  builtins, keywords and everything on `PATH`. `!` lines execute through
  `bash -c` anyway, so compgen under the same non-interactive shell reports
  exactly the names those runs can invoke. The list is resolved once per process
  and filtered in Go; an unavailable bash simply yields no command names. A bare
  `!` offers nothing: every command on the system is not a useful list.
- **Paths.** Everything else, plus any first word containing `/`, completes
  through `refs.Index.ShellCandidates` — the same one-`ReadDir`-per-step listing
  `@` uses, except VCS and dependency directories are offered too (`!ls
  node_modules/`).

Quoting, escapes, `VAR=path` splitting and variable expansion are not handled:
the token under the cursor completes as-is.

## `@` file references

### Parse (`refs/parse.go`)

`Parse(text)` returns `Ref{Path, Start, End, Note}`. `Note` carries any existing
`(800 lines, 64kb)` measurement absorbed into the token — that absorption is
what makes re-expansion idempotent.

`@` matches only at a word boundary (start of line, or after whitespace, `(`,
`[`, a backtick or a quote) so `email@example.com` is prose. The path token
stops at whitespace or trailing punctuation. The annotation shape is matched
strictly: a trailing `(...)` is absorbed only when it contains a digit or a
known kind word (`binary`, `image`, `dir`, `lines`) and is made of the allowed
tokens (digits, `b`/`kb`/`mb`, the kind words), so ordinary parenthetical prose
after a path is never eaten.

### Expand (`refs/expand.go`)

`Expander.Expand(text)` **plans**; it does not read. It returns the rewritten
text, an `Est` sizing what the plan will add, notices, and a `Run` closure for
`Input.After`. Per distinct resolved path (via the tool `PathPolicy`):

| Case | Outcome |
|---|---|
| wildcard pattern (contains `*`, `?` or `[`) | `ls` pair planned via `Registry.Lookup`, listing the matching files so the model sees which paths matched before choosing what to read; the pattern stays literal in prose |
| missing | literal, warning notice |
| directory | `ls` pair planned via `Registry.Lookup` (ignores enabled state) |
| already read, unchanged (`Tracker.Unchanged`) | nothing planned, literal |
| a path the same message already named | planned once; the repeat stays literal |
| text file within `RefInject` and under the running `RefTotal` cap | `read` pair planned, stale annotation stripped |
| large, binary, image, or over the cap | annotation replaced in place; cap trim adds a notice |

Text is rebuilt by splicing spans back to front so earlier offsets stay valid;
the plan is reversed once so the reads land in document order.

Because planning no longer reads, the tracker cannot dedupe within one expansion
— nothing has been observed yet — so **the plan dedupes by resolved path itself**,
and `Run` re-checks `Unchanged` per injection before executing it. The first
guard keeps one message from naming a path twice (which would duplicate its
content *and* its call id); the second keeps a joined batch from re-reading what
an earlier item in the same batch just read. `Run` also stops on a cancelled
context rather than appending error pairs for the rest of the batch.

### Reads land behind the message

`Run` is `Input.After`, so the pairs are appended **after** the user message that
named them, not ahead of it. `session.RewindTarget` rewinds a user prompt to its
parent, so this is what makes rewinding onto an `@` message drop its reads with
it — edit the file, press Enter on the refilled text, and the model sees the new
contents instead of the old inclusion still sitting in context. It also fixes the
display: the tools run when the message lands, after its echo, so what is on
screen is the order replay renders.

That makes re-injection routine, so ids must not repeat. Each `Expand` takes a
run number and mints `ref-<n>-<path>` / `ref-<n>-ls-<path>`; `Expander.Seed(msgs)`
raises that counter above every `ref-<n>-` id already present, and is called
whenever the context is rebuilt (resume, rewind, fork) so a reopened transcript's
ids are never minted twice.

The same rebuild calls `tools.Tracker.Reset`. The tracker is process-wide and
knows nothing of branches, so without it a rewound-away read would still count as
"already in context" and the resubmit would inject nothing at all. Both hang off
`sessRec.onSwitch`, which **every** path that replaces `State.Messages` calls:
`switchState` (manual rewind, and the plan workflow's `forkTo`) and the
compactor's rebuild. Compaction counts because a cut drops reads outright and
`truncate` elides their results to a byte budget, either of which takes file
content out of context that the tracker still vouches for. Headless seeds its own
expander from the rebuilt state, since `--continue` reopens a transcript that
already holds ref ids.

Injected tools run through the same sink as the stager, so the display order
matches the transcript order and therefore matches what replay renders.

### Idempotence

Annotation is idempotent. The annotated text is what lands in history, so a
rewind that refills the editor, or any other resubmission of an earlier
message, sends text that already carries `(800 lines, 64kb)`. `Parse` absorbs
the existing annotation into `Note`, and `Expand` *replaces* it with the fresh
measurement rather than appending a second one.

### Index (`refs/index.go`)

`Index` never walks the workspace tree: every `@` query lists only the single
directory under the cursor, so completion is one cheap `ReadDir` however large or
slow the filesystem. A bare `@` (or a partial name like `@ma`) offers the root's
immediate children; drilling through a trailing `/` (`@src/`, `@pkg/r…`) descends
one level at a time — never a recursive walk, which is what made `@` block input
after typing in a large repo. The skip list (`node_modules`, `.venv`, VCS dirs)
still applies per directory. A `~` or `~/…` query completes within the user's
home directory instead, offered back with the leading `~` kept in each candidate
(so accepting a home path inserts one that expands); a `./` query keeps its prefix,
and an absolute `/…` path completes anywhere on the filesystem — all three keep
their leading form so accepting inserts a usable path. Because a listing may still
take time on a slow mount, `@` queries run off the key loop (see Completion): a
Tab never stalls the editor and a stale result is dropped. Ranking
is (a) already in the conversation (`Tracker.Records()`), (b) recent mtime,
(c) fuzzy score (reusing `tui.MatchScore` rather than a second implementation).
Directories complete with a trailing `/` and keep completing.

### Measure (`tools/measure.go`)

`Measure(path)` stats first and, when the file is a small enough text file,
counts its lines. A directory reports `Dir` with zero bytes. A file above
`MeasureCeiling` reports its bytes and kind but never reads the whole content,
so annotating a giant file is itself bounded. `HumanSize` abbreviates the way
annotations show it: `64kb`, `1.2mb`.

## Pasted content

Large pastes (> ~2 kB) are stored on the UI and the editor receives a
placeholder (`[pasted 412 lines]`). The placeholder is what `editor.Submit`
records in history (that is the point), and `applyKey` expands placeholders
back to their content just before sending. The map is kept for the session so
recalling a pasted line from history still expands.

## Editor history

The editor's line history is no longer seeded or read back through `pkg/tui`.
Every submitted message — prompt, `/cmd` and `!shell`, multi-line pastes included as
one unit — is appended to the workspace's `editor-history.lines` store at submit time
by main (before `ParseLine` dispatch), so it persists regardless of whether a
transcript/recorder is active. The store lives in the sessions tree
(`pkg/session.EditorHistory`, JSONL: one message per row) and excludes messages
prefixed by the host-owned secret marker on every path, so a pasted secret never
reaches disk. Recall (↑/↓ and Ctrl+R) walks `session.RecallIndex` — typed messages
merged with recorded prompts.

## File map

`pkg/command`:

```
parse.go        ParseLine, SplitCommand — line classification
command.go      Command, Registry, Register/Get/List/Names
console.go      Console interface (+ Select/Confirm/Input/Settings/SaveSetting)
builtin.go      /help, /model, /reasoning, /tools, /agents, /settings, /exit + RegisterBuiltins
model.go        /model, /reasoning (moved from main.go)
tools.go        /tools — free-select before, widen-only after first prompt
agents.go       /agents — list and stop sub-agents via the narrow Agents interface
settings.go     /settings menu and per-row editors (enumRow/modelRow/intRow)
shell.go        Stager — staged ! execution and flush
complete.go     Completer — command + path completion source
shellcomplete.go  shell command-name (compgen) and path completion for ! lines
```

`pkg/refs`:

```
parse.go        Parse — @ references with annotation absorption
expand.go       Expander — inject-or-annotate
index.go        Index — one-ReadDir-per-step path source (Candidates, ShellCandidates)
```

`pkg/tui` additions:

```
complete.go        Completion, Completer, StyledCompleter, CompleteStyle,
                   MatchScore, SetCompleter, refreshMenu/startCompletion
complete_impl.go   completionOverlay — the menu and the Tab-driven listing
prompt.go          MultiPick, multiPickState (grouped, Tab-toggle multi-select)
input.go           keyTab / keyBackTab decoding
ui.go              paste placeholders
```


