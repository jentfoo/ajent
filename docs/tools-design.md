# Tool design

`pkg/tools` implements the agent's toolset: the `Tool` interface and registry,
the built-in tools, and the shared infrastructure (path policy, read tracking,
output limits, guard chain) they build on. It implements `pkg/agent.Tool`
directly and never imports `pkg/tui`, so the front end stays interchangeable.

## Core concepts

### Tool interface (`pkg/agent/tool.go`)

```go
type Tool interface {
    Name() string
    Label(call ToolCall) string   // short header line for the UI
    Description() string          // shown to the model
    Schema() llm.ToolSchema       // JSON Schema for the parameters
    Mode() ExecutionMode          // ModeSerial | ModeParallel
    Execute(ctx context.Context, call ToolCall, out Output) (ToolResult, error)
}
```

`Output` is the tool's display channel: writes stream to the UI as they arrive,
and `Diff(path, before, after)` commits a rendered file change. `ToolResult`
splits what the model sees (`Content`) from what history shows (`Display`) and
carries structured `Details` for extensions and the transcript. A failing tool
returns an error *result* (`IsError: true`), not a Go error — the turn
continues and the model adapts.

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
- `SetEnabled(names)` — session-scoped; `/tools` edits it and the session file
  persists it across resume. Changing the set changes the prompt's tool block,
  so the cached schema list is invalidated.
- `Get(name)` — returns the tool wrapped in the guard chain; unknown or
  disabled tools are invisible to the model.
- `All() []agent.Tool` — every declared tool **unwrapped** (no guard chain). A
  sub-agent's tool set is built from this via a narrow `ToolSource`, so a child
  runs no parent guards or approval dialogs.
- `ReadOnly(name)` — whether a tool may auto-run as read-only, derived from MCP
  `annotations.readOnlyHint` or config globs. The permission barrier uses this
  for non-built-in (MCP/extension) tools; core writers never consult it.

### Sub-agent tool set (`toolset.go`, phase 13)

A child agent's tools are a fixed, structural subset of `Registry.All()`,
independent of the parent's enabled set — so `find`/`grep`/`ls`, registered
*disabled* by default in the parent, still reach a child. The filter includes
the read-only built-ins (`read`, `grep`, `find`, `ls`) plus any tool for which
`ReadOnly(name)` is true (MCP hints / config globs). Two exclusions are
structural, not advisory:

- **`agent_*` is barred unconditionally**, applied last so nothing can configure
a child into spawning grandchildren.
- **`bash` is never included** even when enabled in the parent — by policy, not
by classification. A child with a small, obviously read-only set knows exactly
what it has; there is no user at its permission prompt to approve anything else.
- `DryRun(call agent.ToolCall) error` — dispatches to the tool's optional
  `DryRunner` implementation (`editTool.DryRun`) so a doomed call can be detected
  before prompting; returns nil for tools that cannot predict.
- `Preview(call)` → `(path, before, after, ok)` — dispatches to a tool's optional
  `Previewer` (`editTool`, `writeTool`), returning what a call would change so an
  approval dialog shows content or a diff instead of raw JSON arguments.

### Guards (`guard.go`, `asker.go`)

```go
type Guard func(ctx context.Context, call agent.ToolCall) Decision // Allow | Deny | Ask
type Asker func(ctx context.Context, call agent.ToolCall, d Decision) Decision
func (r *Registry) SetAsker(a Asker)
func WithUserInitiated(ctx context.Context) context.Context  // mark a user's own shell line
func IsUserInitiated(ctx context.Context) bool
```

Guards run in registration order before a tool executes; first non-allow wins.
Core registers none by default — the agent runs unguarded unless configured with
a guard. A denial becomes an error result carrying
the reason, and nothing touches disk.

`Ask` consults the registered asker (set via `SetAsker`) when one exists; the
asker turns it into a final allow or deny, and returning `Ask` again is treated
as a denial. With no asker registered an `Ask` still refuses — nothing changes
for callers that do not opt in.

The permission layer (phase 12) registers both: one guard from `permit.Barrier`
runs static classification, and its asker resolves prompts into allow/deny with
session memory. Config's `permissions.safeCommands` lets a user declare extra
tools or exact bash lines to auto-allow as read-only (see phase 12); it can never
name `write`/`edit`. Core never does. The `WithUserInitiated` marker rides the context
so a user's own staged `!` shell line is exempt in every permission mode — it is
the human's shell, not the model's.

## Built-in tools

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
positions. Defaults to 2000 lines with per-line truncation at 2000 chars; the
truncation marker names the next `offset`. Binary files (NUL in the first 8 kB)
are refused with a useful message; images are refused for now. Every successful
read is recorded in the tracker.

### write (`write.go`)

Writes a whole file atomically (temp file + rename) and creates parent
directories. Refuses to overwrite a file the session has not read, or one that
changed since it was read — the error tells the model to read first. Emits a
diff (empty → content for new files) through `Output.Diff`.

### edit (`edit.go`)

Exact-string replacement against an in-memory buffer, written once at the end:
a multi-edit batch is all-or-nothing. The validation loop lives in a shared
`applyEdits(buf string, ops []editOp) (string, error)` used by both `Execute`
and `DryRun`, preceded by an order-independent `validateEdits` pass: an empty
or duplicated `oldText`, two edits overlapping the same region and a no-op
(`oldText == newText`) all fail before any write. Zero matches returns a
near-match suggestion (most token-overlapping line); multiple matches without
`replace_all` returns the occurrence count and locations — both still reported
through the per-op loop. Both errors are designed to be actionable, since they
are the model's main feedback loop. Stale files are refused via the tracker.
Renders through `Output.Diff`. The edit tool implements `DryRunner`, so the
permission barrier can skip prompting for a call that cannot succeed and let
the real apply path surface its natural error.

### bash (`bash.go`)

One `bash -lc` process per call — no persistent shell, so `cd` and state cannot
confuse later calls. Streams stdout and stderr interleaved to the UI while
teeing a bounded copy for the model; output past the limit spills to a file
under `os.TempDir()/ajent-<session>` and the model gets a pointer to it. ANSI
escapes are stripped from captured output. The child runs in its own process
group (`setpgid`); on timeout (default 120 s, max 600 s) or cancellation the
whole group is killed so grandchildren cannot leak, and the model is told it
was a timeout. The environment forces non-interactive settings (`PAGER=cat`,
`GIT_PAGER=cat`, `TERM=dumb`, `GIT_TERMINAL_PROMPT=0`, `AJENT=1`, …).

### find / grep / ls (`find.go`, `grep.go`, `ls.go`)

Off-by-default extras for no-shell agents.

- `find`: glob matching with `**` support; a bare pattern (`*.go`) matches at
  any depth. Uses `git ls-files` inside a repo for `.gitignore` semantics,
  walks otherwise (skipping VCS/dependency subtrees). Results sorted by mtime,
  newest first, capped by `limit`.
- `grep`: shells out to `rg` when present (exit 1 = no matches, exit ≥ 2
  surfaces stderr as an error), falling back to a bounded Go `regexp` walk.
  Modes: `content` (line numbers, optional context lines), `files`, `count`.
  Invalid patterns are actionable errors on both paths.
- `ls`: one directory's entries, sorted alphabetically, `/` suffix on
  directories, named truncation marker at the limit.

These off-by-default extras are exactly what a read-only sub-agent needs (phase
13): they are always available to a child regardless of parent enable state, so
a delegated investigation can `find`/`grep`/`ls` without ever reaching for shell.

## Shared infrastructure

```
builtins.go     Builtins(Options) — wires the shared tracker/policy into all tools
registry.go     Registry, guardedTool wrapper, denied result helper
asker.go        Asker type and SetAsker registration
guard.go        Guard, Decision, Allow/Deny helpers
schema.go       SchemaOf[T] reflection helper
path.go         PathPolicy — resolves relative paths against Cwd, folds symlinks
track.go        Tracker — read tracking for stale-write detection
limits.go       Limit, Elide, bounded Writer, per-tool budgets
spill.go        lazy per-session spill file for oversized bash output
fileutil.go     file probing (text/binary/image), line numbering
walk.go         bounded file walk, runQuiet/runCaptured helpers
internal.go     decode, result helpers, discard Output
```

### Path policy (`path.go`)

All file tools resolve arguments through one `PathPolicy`: relative paths join
the session cwd, symlinks in the longest existing prefix are folded so every
tool agrees on one canonical path (which is also the tracker key). There is no
containment check by default — extensions layer their own limits via guards.

### Read tracking (`track.go`)

The tracker records `path → (mtime, size, sha256)` on every read. `write` and
`edit` consult it before touching an existing file: never-read → "read it
first"; changed since read → "re-read it". Shared by `read`/`write`/`edit` and
exported so compaction can drop superseded reads using it. Safe for
concurrent use.

### Output limits (`limits.go`, `spill.go`)

Each tool has a line/byte budget (`BashOutput` ~30 kB for the model,
`ReadFile` 2000 lines, `GrepResult`, `FindResult`, `LsResult`). `Elide` keeps
head and tail with a marker for in-memory text; the bounded `Writer` forwards
whole lines until a bound is hit, then diverts the remainder to a spill file
created lazily on first overflow — a normal command leaves nothing behind.

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
