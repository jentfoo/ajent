# Tool design

`pkg/tools` implements the agent's toolset: the `Tool` interface and registry,
the built-in tools, and the shared infrastructure (path policy, read tracking,
output limits, guard chain) they build on. It implements `pkg/agent.Tool`
directly and never imports `pkg/tui`, so the front end stays interchangeable.

## Core concepts

### Tool interface (`pkg/agent/tool.go`)

A tool exposes its name, a short UI header label, the description shown to the
model, its JSON schema for parameters, whether it may run in parallel with
siblings (`ModeSerial | ModeParallel`), and executes a call against an output
writer.

`Output` is the tool's display channel: writes stream to the UI as they arrive,
and `Diff(path, before, after)` commits a rendered file change. `ToolResult`
splits what the model sees (`Content`) from what history shows (`Display`) and
carries structured `Details` for extensions and the transcript. A tool **either
streams to `agent.Output` or sets `ToolResult.Display`, never both** — otherwise
its head renders twice (see the output-head rule in `tui-design.md`).
A failing tool returns an error *result* (`IsError: true`), not a Go error —
the turn continues and the model adapts.

### Schemas (`schema.go`)

`SchemaOf[T]()` derives a JSON Schema from a params struct via reflection over
`json`, `desc` and `enum` tags — no external dependency. A field without
`omitempty` is required. Unsupported kinds panic at registration time, not at
call time.

### Registry (`registry.go`)

Holds the declared tools in registration order plus their enabled state, and
satisfies `agent.ToolSet` so the loop reads tools straight off it.

- `Register(t, defaultEnabled)` — built-ins, extensions and MCP servers all
  register the same way.
- `RegisterGroup(ToolGroup)` — presents several already-registered tools as one
  toggleable `/tools` row (label + source) that always shares enable state. The
  sub-agent trio (`agent_start`/`agent_poll`/`agent_list`) is registered under
  this, so it shows once as `subagents`, grouped with the builtins.
- `Units(offered []Tool)` — collapses offered tools into toggleable `/tools`
  rows: a group whose every member is present becomes one row carrying all of
  them; otherwise each tool stands alone. A partially-offered group (e.g. widen
  mode after a non-atomic change) falls back to per-member rows.
- `SetEnabled(names)` / `Enable(names)` — session-scoped; `/tools` edits it and
  the session file persists it across resume. Both expand any named tool group
  into its members, so one label flips every member at once. Changing the set
  changes the prompt's tool block, so the cached schema list is invalidated.
- `Get(name)` — returns the tool wrapped in the guard chain; unknown or
  disabled tools are invisible to the model.
- `All() []agent.Tool` — every declared tool **unwrapped** (no guard chain). A
  sub-agent's tool set is built from this via a narrow `ToolSource`, so a child
  runs no parent guards or approval dialogs.
- `ReadOnly(name)` — whether a tool may auto-run as read-only, derived from MCP
  `annotations.readOnlyHint` or config globs. The permission barrier uses this
  for non-built-in (MCP/extension) tools; core writers never consult it.

### Sub-agent tool set and preview seams

The child agent's structural filter (which tools a sub-agent may call) is owned by
the registry but specified in `subagents-design.md`; it lives in
`pkg/subagent/toolset.go`. The two optional-tool seams the guard chain relies on are
methods on `Registry`:
- `DryRun(call agent.ToolCall) error` — dispatches to the tool's optional
  `DryRunner` implementation (`editTool.DryRun`) so a doomed call can be detected
  before prompting; returns nil for tools that cannot predict.
- `Preview(call)` → `(Change, ok)` — dispatches to a tool's optional `Previewer`
  (`editTool`, `writeTool`). `Change{Path, Before, After}` is what the call would
  do to the file; see "Rendering a change before it runs".

### Guards (`guard.go`, `asker.go`)

A guard decides a call — allow, deny or ask — and an optional registered asker
turns any `Ask` verdict into a final allow/deny. A user's own staged shell line is
marked on the context so it stays exempt from every permission mode.

Guards run in registration order before a tool executes; first non-allow wins.
Core registers none by default — the agent runs unguarded unless configured with
a guard. A denial becomes an error result carrying
the reason, and nothing touches disk.

`Ask` consults the registered asker (set via `SetAsker`) when one exists; the
asker turns it into a final allow or deny, and returning `Ask` again is treated
as a denial. With no asker registered an `Ask` still refuses — nothing changes
for callers that do not opt in.

### Rendering a change before it runs

`guardedTool.Execute` renders a `Previewer`'s `Change` through `Output.Diff`
**before the guard chain**, not after the tool applies it. That ordering is the
point: an approval dialog is a transient live-block region capped at a handful of
rows, so a diff shown *inside* it is necessarily truncated. Committing the full
diff first puts the whole change in the permanent record and lets the dialog sit
below it with a one-line subject that names what is already on screen.

Invariants:

- **Once per call.** The render sits before the guard loop, so a re-asking asker
  cannot print the change twice.
- **Every mode.** It does not hang off the preview closure the permission layer
  installs — that is reached only when a dialog is about to open, so `allow-all`,
  a remembered session grant, a `!` user-initiated line and the doomed-edit skip
  would all show nothing.
- **Proposed, not applied.** A denied or failed call still leaves its diff in the
  record, followed by the denial summary or the error. What is rendered is the
  change *requested*.
- A `Preview` error (bad arguments, unreadable file) renders nothing and lets
  `Execute` surface its own error. Tools must therefore not rely on the render
  having happened.

Cost: write/edit read the target file twice per call (preview, then execute).

The permission layer registers both: one guard from `permit.Barrier`
runs static classification, and its asker resolves prompts into allow/deny with
session memory. A reason typed for an "allow with note" or a denial is injected
as a user message immediately after the tool call it governs — never as tool
output — so the model treats it as operator intent (see `prompt-design.md`,
provenance). Config's `permissions.safeCommands` lets a user declare extra
tools, whole MCP server namespaces (`sectool` covers every `sectool__*` tool), or
bash command lines to auto-allow as read-only; it can never name
`write`/`edit`. Core never does. The `WithUserInitiated` marker rides the context
so a user's own staged `!` shell line is exempt in every permission mode — it is
the human's shell, not the model's.

### Headless: the tool set is the gate

A one-shot run (`-p`, see `phases/21-one-shot-noninteractive.md`) has no dialog to
open, so it never lets an ask arise. The barrier runs at `allow-all` and the
**offered tool set** carries the policy instead: the model is only ever handed
tools it is allowed to call, so it never spends a step discovering a refusal.
`tools.ReadOnlyBuiltins` names what survives `--read-only`; `ask_user` is
excluded from every headless scope because nobody can answer it.

The one exception is `permissions.deniedCommands`, which is still installed and
still refuses before the allow-all short circuit. It is the only headless refusal
path, it only fires when an operator configured it, and it covers what a
tool-name flag cannot — a bash command line rather than a tool. Such a refusal is
an error result like any other, so the turn adapts and continues.

The scope decides the built-in names outright, ignoring `tools.enabled`, because
the config default omits `grep`/`ls`/`find`. It does **not** re-enable a tool its
source registered disabled, so an MCP server switched off in `mcp.json` stays off.

## Built-in tools

The sub-agent trio (`agent_start`, `agent_poll`, `agent_list`) is also registered
under the builtin source, so `/tools` sorts it up front with the
core tools, ahead of any MCP group. The three are presented and toggled as one
row — a single `subagents` entry (`RegisterGroup`) rather than three individual
tools.

| Tool   | Default  | Mode     | Parameters |
|--------|----------|----------|------------|
| `read` | enabled  | parallel | `path`, `offset`, `limit` |
| `write`| enabled  | serial   | `path`, `content` |
| `edit` | enabled  | serial   | `path`, `edits[]` (`oldText`/`newText`/`replace_all`) |
| `bash` | enabled  | serial   | `command`, `timeout`, `cwd` |
| `find` | disabled | parallel | `pattern`, `path`, `limit` |
| `grep` | disabled | parallel | `pattern`, `path`, `glob`, `ignoreCase`, `limit`, `literal`, `context`, `mode` |
| `ls`   | disabled | parallel | `path`, `limit` |

`find`/`grep`/`ls` are off by default: with `bash` available the model can use
`rg`/`find`/`ls` directly. They exist for the sub-agent (which has no shell)
and for configurations that run without `bash`.

### read (`read.go`)

Line-numbered (`cat -n` style) output so `edit` and the model agree on
positions. Defaults to 2000 lines with each line capped at `MaxLineRunes`
(1024 runes) and a marker; the truncation marker names the next `offset`. Binary
files (NUL in the first 8 kB)
are refused with a useful message; images are refused for now. Every successful
read is recorded in the tracker.

The model always sees the full line-numbered content (`Content`); `Display` is
that same block, which the TUI elides to a head plus a collapse count via the
shared output-head rule in `tui-design.md`. The path already rides on the tool
header that `ToolStart` commits, so no bespoke summary string is produced here.

### write (`write.go`)

Writes a whole file atomically (temp file + rename) and creates parent
directories. Emits a `Change` (empty → content for new files) through
`Previewer`, rendered before the call is vetted rather than after it applies.

### edit (`edit.go`)

Exact-string replacement against an in-memory buffer, written once at the end:
a multi-edit batch is all-or-nothing. The validation loop lives in a shared
`applyEdits(buf string, ops []editOp) (string, error)` used by both `Execute`
and `DryRun`, preceded by an order-independent `validateEdits` pass: an empty
or duplicated `oldText`, and a no-op (`oldText == newText`) all fail before any
write. Every op's span is resolved against the **original** buffer, never another
edit's output, so edits cannot cascade; overlapping spans across ops are rejected.
Zero matches returns an actionable diagnostic naming each reliably-detected cause
(whitespace or casing differences, genuinely-absent content) plus one closest-line
hint — and when an earlier edit's `newText` would create the missing text it says
so explicitly. Multiple matches without `replace_all` returns the occurrence count
and locations. Messages tell the model it **must provide text exactly** rather than
asking it to copy, and always receive the original buffer so diagnostics stay
actionable. Line endings follow one package-wide convention: model-visible output
is always LF, while a write copies untouched regions verbatim and gives each
replacement its neighbouring lines' ending — `write` overwrites with the existing
file's majority ending, and a new file gets LF.

### bash (`bash.go`)

One non-login `bash -c` process per call (a login shell would reset PATH to the
system default and hide user dirs like `~/.local/bin`, Homebrew or nvm) — no
persistent shell, so `cd` and state cannot
confuse later calls. Streams stdout and stderr interleaved to the UI while
teeing a bounded copy for the model; every kept line is capped at `MaxLineRunes`,
and output past the limit (or carrying one overlong line) spills **the complete
stream** (head + overflow) to a file under `os.TempDir()/ajent-<session>` and the
model gets a pointer to it. Because that spill is an ordinary readable text file,
the model can open it with `read` and page through it, exactly as it does for
grep's spilled results — bash differs from read only in that its output has no
pre-existing source file to re-read, so the footer names a written one instead of
a next offset. ANSI
escapes are stripped from captured output. The child runs in its own process
group (`setpgid`); on timeout (default 120 s, max 600 s) or cancellation the
whole group is killed so grandchildren cannot leak, and the model is told it
was a timeout.

The cancellation contract: each run owns its process group; when the parent
context is cancelled (a turn interrupt, or `Stager.Cancel` on a `!` line), the
whole group is SIGKILLed, whatever partial stdout/stderr arrived rides in the
result, and the result is an **error result** beginning `interrupted by user`
(the shared `agent.InterruptedText`) so the transcript reads as an interruption.
A timeout stays a distinct non-error `"killed after … timeout"` result. The
environment forces non-interactive settings (`PAGER=cat`,
`GIT_PAGER=cat`, `TERM=dumb`, `GIT_TERMINAL_PROMPT=0`, `AJENT=1`, …).

### find / grep / ls (`find.go`, `grep.go`, `ls.go`)

Off-by-default extras for no-shell agents.

- `find`: glob matching with `**` support; a bare pattern (`*.go`) matches at
  any depth. Uses `git ls-files -z` (quoting disabled, so non-ASCII filenames
  stay usable) inside a repo for `.gitignore` semantics, walking otherwise.
  Results sorted by mtime (stat once per file), newest first, capped by `limit`.
- `grep`: shells out to `rg` when present (exit 1 = no matches, exit ≥ 2
  surfaces stderr as an error), falling back to a bounded Go `regexp` walk. Both
  paths respect `.gitignore` — the fallback enumerates through the same
  `repoFiles`. Modes: `content` (line numbers, optional context lines),
  `files`, `count`. Invalid patterns are actionable errors on both paths.
- `ls`: one directory's entries — or the files a wildcard pattern matches
  (via `filepath.Glob`) — sorted alphabetically, `/` suffix on directories,
  named truncation marker at the limit. A glob with no matches is an error so it
  is never mistaken for an empty dir.

Like `read`, each sets `ToolResult.Display` to the same text as its model-visible
`Content`, so history renders it through the shared output-head rule instead of a
tool header with no body.

These off-by-default extras are exactly what a read-only sub-agent needs: they
are always available to a child regardless of parent enable state, so a
delegated investigation can `find`/`grep`/`ls` without ever reaching for shell.
The plan workflow's planning and review scopes enable them explicitly for the
same reason.

### ask_user (`ask.go`)

Also off by default. `ask_user(question, options?)` puts a decision back to the
user and waits: a closed choice when `options` are given, free text otherwise.
`pkg/tools` must not import `pkg/tui`, so it takes an injected `Options.Ask`;
`main.go` supplies an adapter over `(*tui.UI).Ask`, which already queues behind
permission dialogs, reports Esc as declined, and reports `ErrNoUI` in plain mode.

No option list is closed: the TUI offers a "Chat about this" row and returns the
typed reply with a **negative index**, reported as "chose none of the options and
replied: …" rather than as a choice. The distinction matters — a reply dressed as
an option would have the model act on a decision the user never made.

`ModeSerial` — a question owns the terminal until answered. Every outcome is a
**normal** result, never an error: a declined question, a missing terminal and an
asker failure all come back as text telling the model to decide for itself and
state its assumption, so a headless run never blocks and never fails a turn. It
touches nothing on disk, so it is marked read-only and needs no approval. Its
description tells the model to ask only when the decision is genuinely the
user's, and never to ask permission to act — that is the barrier's job.

### Plan control tools (`pkg/plan`, source `plan`)

`dev_implement`, `dev_review`, `dev_revise` and `dev_complete` are registered by
`pkg/plan` under source `"plan"` as one `/tools` group, **lazily on `/plan` and
unregistered on every exit path**, so they never exist outside a workflow. They
are marked read-only, so the barrier runs them free without being widened for
anything else. They are the only tools that set `ToolResult.EndTurn`, and only on
their success path — see `plan-design.md` and `agent-loop-design.md`.

`Unregister(source)` drops the tools but leaves `Registry.groups` in place;
`Units`' all-present check hides a row whose members are gone, and
`RegisterGroup` dedupes by name, so a second `/plan` re-registers cleanly.

## Shared infrastructure

```
builtins.go     Builtins(Options) — wires the shared tracker/policy into all tools
registry.go     Registry, guardedTool wrapper, denied result helper
asker.go        Asker type and SetAsker registration
guard.go        Guard, Decision, Allow/Deny helpers
schema.go       SchemaOf[T] reflection helper
path.go         PathPolicy — resolves relative paths against Cwd, folds symlinks
track.go        Tracker — observed-file records for @ref dedupe; Reset on a context switch
limits.go       Limit, Bound/Bounded truncation, bounded Writer, per-tool budgets
spill.go        lazy per-session spill file for oversized tool output (bash/grep)
fileutil.go     file probing (text/binary/image), line numbering
walk.go         bounded file walk, runQuiet/runCaptured helpers
internal.go     decode, result helpers, discard Output
```

### Path policy (`path.go`)

All file tools resolve arguments through one `PathPolicy`: a leading `~` or
`~/…` expands to the user's home directory, other relative paths join the
session cwd, and symlinks in the longest existing prefix are folded so every
tool agrees on one canonical path (which is also the tracker key). There is no
containment check by default — extensions layer their own limits via guards.

### Read tracking (`track.go`)

The tracker records `path → (mtime, size, sha256)` on every observation.
`@ref` expansion uses those records (via `Unchanged`) to dedupe against an
unchanged in-context read — at plan time, and again as each injection runs, so a
batch of messages naming one path reads it once. It is shared by
`read`/`write`/`edit`, which each observe the content they produce so a later
`@file` reflects current state, and exported for reuse outside the package. Safe
for concurrent use.

Records describe the *process*, not the context, so a rewind, fork or compaction
can leave them claiming a file is in context when its read was dropped or elided.
The host calls `Reset` from every rebuild path, which makes the next `@file`
re-inject rather than dedupe against a read the model can no longer see.

### Output limits (`limits.go`, `spill.go`)

Each tool has a line/byte budget (`BashOutput` ~30 kB for the model,
`ReadFile` 2000 lines, `GrepResult`, `FindResult`, `LsResult`). Truncation is
**head-only at whole-line boundaries**: `Bound` keeps the leading lines that fit
either bound, capping every kept line at `MaxLineRunes`, and cuts a single
overlong first line at `MaxLineRunes` when no whole line fits. One overlong
line alone still counts as truncated, so grep spills the full text and the
footer can name it. The footer names shown/total lines and total bytes plus a
spill path. The bounded `Writer` forwards whole lines until a bound is hit then
diverts to a spill file created lazily on first overflow — writing the kept head
there too (in stream order), so the spill file holds the **complete** stream;
bash caps every kept line at `MaxLineRunes`, and treats an in-budget overlong
line as truncation too, spilling the complete stream so nothing is lost. A normal command
leaves nothing behind. (`Elide`, keeping rune-capped head and tail with a marker,
survives for compaction's structural reduction only.)

## Agent integration

`main.go` builds the set with `tools.Builtins(Options{Cwd, SessionID})` and
hands the registry to the agent loop as its `ToolSet`. Per turn the loop:

1. Mirrors the enabled names into state (the system prompt derives its search
   hint from them — `ls`/`grep`/`find` via `bash` is suggested only when no
   dedicated exploration tool is enabled).
2. Sends the registry's cached schemas with each request.
3. Dispatches tool calls: parallel when the model supports it and every call is
   `ModeParallel`, serial otherwise; results are appended in call order.
4. Streams each tool's `Output` to the sink (`ToolOutput`, `Diff`) and commits
   `Display`/`Details` onto the recorded `ToolResultBlock`, so the transcript
   preserves what history showed.
