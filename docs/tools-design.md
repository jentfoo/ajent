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
streams to `agent.Output` or sets `ToolResult.Display`, never both**, otherwise
its head renders twice (see the output-head rule in `tui-design.md`).
A failing tool returns an error *result* (`IsError: true`), not a Go error.
The turn continues and the model adapts.

### Schemas (`schema.go`)

`SchemaOf[T]()` derives a JSON Schema from a params struct via reflection over
`json`, `desc` and `enum` tags, with no external dependency. A field without
`omitempty` is required. Unsupported kinds panic at registration time, not at
call time.

### Registry (`registry.go`)

Holds the declared tools in registration order plus their enabled state, and
satisfies `agent.ToolSet` so the loop reads tools straight off it.

- `Register(t, defaultEnabled)` — built-ins, extensions and MCP servers all
  register the same way.
- `RegisterGroup(ToolGroup)` — presents several already-registered tools as one
  toggleable `/tools` row (label + source) that always shares enable state. The
  sub-agent trio is registered this way (see "Built-in tools").
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
`pkg/subagent/toolset.go`. The optional-tool seams the guard chain relies on are
methods on `Registry`:
- `DryRun(call agent.ToolCall) error` — dispatches to the tool's optional
  `DryRunner` implementation (`editTool.DryRun`) so a doomed call can be detected
  before prompting; returns nil for tools that cannot predict.
- `Preview(call)` → `(Change, ok)` — dispatches to a tool's optional `Previewer`
  (`editTool`, `writeTool`). `Change{Path, Before, After}` is what the call would
  do to the file; see "Rendering a change before it runs".
- `MustSerialize(calls)` — reports whether any call would prompt, so dispatch
  runs the batch serially and approval dialogs open in submission order.

### Guards (`guard.go`, `asker.go`)

A guard decides a call (allow, deny or ask) and an optional registered asker
turns any `Ask` verdict into a final allow/deny. A user's own staged shell line is
marked on the context so it stays exempt from every permission mode.

Guards run in registration order before a tool executes; first non-allow wins.
Core registers none by default. The agent runs unguarded unless configured with
a guard. A denial becomes an error result carrying
the reason, and nothing touches disk.

`Ask` consults the registered asker (set via `SetAsker`) when one exists; the
asker turns it into a final allow or deny, and returning `Ask` again is treated
as a denial. With no asker registered an `Ask` still refuses. Nothing changes
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
  installs, because that is reached only when a dialog is about to open. So `allow-all`,
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
as a user message immediately after the tool call it governs, never as tool
output. The model therefore treats it as operator intent (see `prompt-design.md`,
provenance). Config's `permissions.safeCommands` lets a user declare extra
tools, whole MCP server namespaces (`sectool` covers every `sectool__*` tool), or
bash command lines to auto-allow as read-only; it can never name
`write`/`edit`. Core never does. `auto+write` is the one mode that runs
`write`/`edit` without a prompt, and only when the call's path resolves under its
roots (cwd and the temp dir); resolution goes through the tool's own `PathPolicy`,
so a symlink pointing outside lands out of scope and still prompts. Three carve-outs
are load-bearing. A bash call's own `cwd` argument rebases every relative path, so
it is resolved and scope-checked before the command is. A `cd` segment does the
same mid-line: its target is checked like any other path, and both the old and new
directory stay candidates, because a control operator may skip the `cd` and a later
path must be in scope either way. That is why `cd sub && mkdir ../x` is refused.
VCS metadata (`.git`, `.hg`, `.svn`) is excluded from the roots, since a hook or
`core.hooksPath` written there is code that executes on the next git invocation.
`rmdir -p` is permitted because it walks only the operand's own components, so its
shallowest one is scope-checked and everything it can reach sits under that. The
`WithUserInitiated` marker rides the context so a user's own staged `!` shell line
is exempt in every permission mode; it is the human's shell, not the model's.

### Headless: the tool set is the gate

A one-shot run (`-p`) has no dialog to
open, so it never lets an ask arise. The barrier runs at `allow-all` and the
**offered tool set** carries the policy instead: the model is only ever handed
tools it is allowed to call, so it never spends a step discovering a refusal.
`tools.ReadOnlyBuiltins` names what survives `--read-only`; `ask_user` is
excluded from every headless scope because nobody can answer it.

The one exception is `permissions.deniedCommands`, which is still installed and
still refuses before the allow-all short circuit. It is the only headless refusal
path, it only fires when an operator configured it, and it covers what a
tool-name flag cannot, such as a bash command line rather than a tool. Such a refusal is
an error result like any other, so the turn adapts and continues.

The scope decides the built-in names outright, ignoring `tools.enabled`, because
the config default omits `grep`/`ls`/`find`. It does **not** re-enable a tool its
source registered disabled, so an MCP server switched off in `mcp.json` stays off.

## Built-in tools

The sub-agent trio (`agent_start`, `agent_poll`, `agent_list`) is also registered
under the builtin source, so `/tools` sorts it up front with the
core tools, ahead of any MCP group. The three are presented and toggled as one
row. A single `subagents` entry (`RegisterGroup`) appears instead of three individual
tools.

| Tool   | Default  | Mode     |
|--------|----------|----------|
| `read` | enabled  | parallel |
| `write`| enabled  | serial   |
| `edit` | enabled  | serial   |
| `bash` | enabled  | serial   |
| `find` | disabled | parallel |
| `grep` | disabled | parallel |
| `ls`   | disabled | parallel |

`find`/`grep`/`ls` are off by default: with `bash` available the model can use
`rg`/`find`/`ls` directly. They exist for the sub-agent (which has no shell)
and for configurations that run without `bash`.

### read (`read.go`)

Line-numbered (`cat -n` style) output so `edit` and the model agree on
positions. Output is bounded by a line count and a per-line rune cap, each with a
marker; the truncation marker names the next `offset`. Binary
files are refused with a useful message; images are refused for now. Every successful
read is recorded in the tracker.

The model always sees the full line-numbered content (`Content`); `Display` is
that same block, which the TUI elides to a head plus a collapse count via the
shared output-head rule in `tui-design.md`. The path already rides on the tool
header that `ToolStart` commits, so no bespoke summary string is produced here.

### write (`write.go`)

Writes a whole file atomically (temp file + rename) and creates parent
directories. Emits a `Change` (empty → content for new files) through
`Previewer`, rendered before the call is vetted rather than after it applies.
Overwriting an existing file returns a unified diff of what was displaced, the
one part of the change the model did not itself supply. A new file reports its
line count alone.

### edit (`edit.go`)

String replacement against an in-memory buffer, written once at the end so a multi-edit
batch is all-or-nothing. One shared apply path serves both `Execute` and `DryRun`, preceded
by an order-independent validation pass (empty or duplicated old text) so nothing
fails after any write. Every op's span resolves against the **original** buffer, never another
edit's output — edits cannot cascade, and overlapping spans across ops are rejected.

A match ladder resolves each `oldText` through four tiers: exact byte-for-byte, then canonical
(unicode lookalikes folded to ASCII), a whole-block indent shift, then a fuzzy per-line match. Escalation
happens only on *zero* matches; a tier that finds several leaves the ambiguity for the caller to reject rather than
guessing which was meant. Canon and indent prove their span equal to the `oldText` it claims before
writing, so a mapping bug degrades into a no-match error instead of corrupting the file; fuzzy
cannot, and earns its span through its own guards below. An `oldText` that folds to bare newlines is
refused above exact, since it would match every line boundary. The indent tier applies only to a
uniform whole-block shift, where both texts' non-blank lines share one base indent, in either
direction including onto a block the file holds flush left; a mixed or nested tab/space conversion
matches nothing and falls through rather than being applied wrongly. Any match above exact reports in
one line what differed, so the next edit is written correctly. Each differing run is widened to whole
identifier and number tokens before it is quoted, since a value and the file's often share an edge
digit (`4096` against `65536`) and a byte-level cut would quote back halves of a number; a non-exact
match names every line that drifted, capped at `maxDriftQuotes`. The canon tier tells a trailing-whitespace
difference from a lookalike by trailing-trim equality rather than by stripping all whitespace, which
folds an nbsp away and misreports it, and names the characters that differed.

The canon tier is skipped when `oldText` and `newText` canonicalize to the same text, since matching
would rewrite already-correct text with itself. Indent still runs, as a block quoted at the wrong
depth remains provable. A byte-identical pair is refused above exact, since with no rewritten span a
duplicated `newText` and a mis-transcribed `oldText` are the same bytes with opposite intent.

That principle also governs what a non-exact match writes. Only the run between the site's and the
replacement's common affixes comes from `newText`; text the edit merely quoted keeps the file's own
bytes, so a lookalike the model flattened survives outside the change. A site that folds to the
replacement is written whole, since there the fold is the edit.

The fuzzy tier heals a mis-transcribed value — `queueDepth = 256` where the file says `1024`. It
works line by line: a line the region differs on must be one `newText` rewrites, and must differ
only inside the part of that line `newText` rewrites. That is what makes writing `newText` the
requested edit. A line the edit only quotes is refused, because applying would rewrite text the
model was not changing and nothing in the call says whether it meant to; the same holds for a
drift outside the rewritten part of a line it is changing. Healing needs `oldText` and `newText`
to hold the same number of lines, since only then does each line pair up and the drift stay
checkable; an edit that adds or removes lines applies through the tiers above or not at all. Five
further guards apply: at most eight lines of one edit may differ, each differing line by one run
of at most four characters with four characters of exact context anchoring it; the region must be
the clear best match, where a second region under the limit is a guess outright and one just past
it separates a match already thin; no line may overlap one an earlier edit wrote, since healing
there could revert that edit; and the punctuation ending a matched line must survive into the
replacement, since every other guard is measured on `oldText` while the apply writes over the
file's line. Those line numbers hang off the tracker's observation of the file, so
a later edit remaps them past its own insertions and any change the tool did not make drops them
rather than leaving them pointing at unrelated lines; a re-read of unchanged content keeps them,
since the numbers still name that text. A line differing only in indentation is left to the indent tier,
which proves a uniform shift or refuses; healing it here would reformat the file.

Diagnostics run only after every tier failed and reason in canonical space, so with each lookalike
difference already rejected they name the real cause (whitespace or casing, genuinely-absent
content, an earlier edit in the same batch) rather than blaming a stray smart quote. A spacing
mismatch is one comparison of the two lines' whitespace runs, reported once per distinct difference
with the lines it covers.
The message ends with the closest text verbatim for copying: chosen by whole-block agreement over
per-line token overlap (which previously landed hints outside the intended block), rendered untrimmed
without a line gutter since position lives in the header and it must be reproduced byte for byte. Below
a similarity floor nothing is offered at all — admitting no close match beats naming a decoy.

One further signal rides on that message: when the closest text differs in one quotable run, it is
named rather than left as two strings to be compared by eye, under the same token widening.

Multiple matches without `replace_all` return the occurrence count and each match's line; messages
tell the model it **must provide text exactly** rather than asking it to copy. Argument decoding
tolerates what models emit in place of the declared schema (a double-encoded object, a
JSON-stringified `edits`, a single edit where an array is declared), since rejecting those costs a round
trip without saying anything new. Singleton wrapping now lives in `strutil.DecodeArgs` for every tool:
a lone value whose shape fits a declared array field heals into a one-element array; leaf values are
never reinterpreted. Decode errors surface via `DecodeArgs` in model-facing shape (field plus
expected/actual JSON kind), never leaking Go type names.

Feedback returns a unified diff via `go-udiff` directly (`pkg/tools` never imports `pkg/tui`), bounded
by `Elide`. The added side is the model's own text and the removed side names what the file actually
held, so it makes a non-exact match explain itself. It rides only when there is reason to check the
result: a match tier fired, one edit landed on several sites, or the apply duplicated text — its
written text (as reindented, when a tier shifted it) now occurring elsewhere in the file, or a
written line repeating its neighbour. An edit matched byte-exactly and landed where aimed returns
the summary alone: the diff would only restate arguments the model just sent, and extra tokens become
text for the next `oldText` to be modelled on.

Line endings follow one package-wide convention: model-visible output is always LF,
while a write copies untouched regions verbatim and gives each replacement its
neighbouring lines' ending. `write` overwrites with the existing file's majority
ending, and a new file gets LF.

### bash (`bash.go`)

One non-login `bash -c` process per call (a login shell would reset PATH to the
system default and hide user dirs like `~/.local/bin`, Homebrew or nvm). There is no
persistent shell, so `cd` and state cannot
confuse later calls. Streams stdout and stderr interleaved to the UI while
teeing a bounded copy for the model; every kept line is capped, and output past the limit (or carrying one overlong line) spills **the complete
stream** (head + overflow) to a file under `os.TempDir()/ajent-<session>` and the
model gets a pointer to it. Because that spill is an ordinary readable text file,
the model can open it with `read` and page through it, exactly as it does for
grep's spilled results. Bash differs from read only in that its output has no
pre-existing source file to re-read, so the footer names a written one instead of
a next offset. ANSI
escapes are stripped from captured output. The child runs in its own process
group; on timeout or cancellation the
whole group is killed so grandchildren cannot leak, and the model is told it
was a timeout.

The cancellation contract: each run owns its process group; when the parent
context is cancelled (a turn interrupt, or `Stager.Cancel` on a `!` line), the
whole group is SIGKILLed, whatever partial stdout/stderr arrived rides in the
result, and the result is an **error result** marked as interrupted with the shared
interruption text, so the transcript reads as an interruption.
A timeout stays a distinct non-error result. The
environment forces non-interactive settings (no pagers, no terminal prompts,
no colour).

### find / grep / ls (`find.go`, `grep.go`, `ls.go`)

Off-by-default extras for no-shell agents.

- `find`: glob matching with `**` support; a bare pattern (`*.go`) matches at
  any depth. Uses `git ls-files -z` (quoting disabled, so non-ASCII filenames
  stay usable) inside a repo for `.gitignore` semantics, walking otherwise.
  Results sorted by mtime (stat once per file), newest first, capped by `limit`.
- `grep`: shells out to `rg` when present (exit 1 = no matches, exit ≥ 2
  surfaces stderr as an error), falling back to a bounded Go `regexp` walk. Both
  paths respect `.gitignore`. The fallback enumerates through the same
  `repoFiles`. Modes: `content` (line numbers, optional context lines),
  `files`, `count`. Invalid patterns are actionable errors on both paths.
- `ls`: one directory's entries (or the files a wildcard pattern matches via
  `filepath.Glob`), sorted alphabetically, `/` suffix on directories,
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
`pkg/app` supplies an adapter over `(*tui.UI).Ask`, which already queues behind
permission dialogs, reports Esc as declined, and reports `ErrNoUI` in plain mode.

No option list is closed: the TUI offers a free-text row and returns the typed
reply with a **negative index**, reported as a non-choice rather than as a
selection. The distinction matters. A reply dressed as
an option would have the model act on a decision the user never made.

`ModeSerial`: a question owns the terminal until answered. Every outcome is a
**normal** result, never an error: a declined question, a missing terminal and an
asker failure all come back as text telling the model to decide for itself and
state its assumption, so a headless run never blocks and never fails a turn. It
touches nothing on disk, so it is marked read-only and needs no approval. Its
description tells the model to ask only when the decision is genuinely the
user's, and never to ask permission to act. That is the barrier's job.

### Plan control tools (`pkg/plan`, source `plan`)

`dev_implement`, `dev_review`, `dev_revise` and `dev_complete` are registered by
`pkg/plan` under source `"plan"` as one `/tools` group, **lazily on `/plan` and
unregistered on every exit path**, so they never exist outside a workflow. They
are marked read-only, so the barrier runs them free without being widened for
anything else. They are the only tools that set `ToolResult.EndTurn`, and only on
their success path. See `plan-design.md` and `agent-loop-design.md`.

Unregistering a source drops its tools but leaves the registry's group bookkeeping
in place, and group rows hide themselves when their members are gone, so a
second `/plan` re-registers cleanly.

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
containment check by default. Extensions layer their own limits via guards.

### Read tracking (`track.go`)

The tracker records, per observed path, enough to detect that the file is unchanged
on a later read (its modification time, size and a content hash).
`@ref` expansion uses those records (via `Unchanged`) to dedupe against an
unchanged in-context read, at plan time and again as each injection runs, so a
batch of messages naming one path reads it once. It is shared by
`read`/`write`/`edit`, which each observe the content they produce so a later
`@file` reflects current state, and exported for reuse outside the package. Safe
for concurrent use.

Records describe the *process*, not the context, so a rewind, fork or compaction
can leave them claiming a file is in context when its read was dropped or elided.
The host calls `Reset` from every rebuild path, which makes the next `@file`
re-inject rather than dedupe against a read the model can no longer see.

### Output limits (`limits.go`, `spill.go`)

Each tool has a line/byte budget (`BashOutput` for the model, `ReadFile`,
`GrepResult`, `FindResult`, `LsResult`). Truncation is
**head-only at whole-line boundaries**: `Bound` keeps the leading lines that fit
either bound, capping every kept line, and cuts a single
overlong first line when no whole line fits. One overlong
line alone still counts as truncated, so grep spills the full text and the
footer can name it. The footer names shown/total lines and total bytes plus a
spill path. The bounded `Writer` forwards whole lines until a bound is hit then
diverts to a spill file created lazily on first overflow. It writes the kept head
there too (in stream order), so the spill file holds the **complete** stream;
bash caps every kept line, and treats an in-budget overlong
line as truncation too, spilling the complete stream so nothing is lost. A normal command
leaves nothing behind. (`Elide`, keeping rune-capped head and tail with a marker,
survives for compaction's structural reduction only.)

## Agent integration

`pkg/app` builds the set with `tools.Builtins(Options{Cwd, SessionID})` and
hands the registry to the agent loop as its `ToolSet`. Per turn the loop:

1. Mirrors the enabled names into state (the system prompt derives its search
   hint from them. `ls`/`grep`/`find` via `bash` is suggested only when no
   dedicated exploration tool is enabled).
2. Sends the registry's cached schemas with each request.
3. Dispatches tool calls: parallel when the model supports it and every call is
   `ModeParallel`, serial otherwise; results are appended in call order.
4. Streams each tool's `Output` to the sink (`ToolOutput`, `Diff`) and commits
   `Display`/`Details` onto the recorded `ToolResultBlock`, so the transcript
   preserves what history showed.
