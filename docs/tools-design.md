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
carries structured `Details` for extensions and the transcript. A tool
**either streams to `agent.Output` or sets `ToolResult.Display`, never both**,
otherwise its head renders twice (see the output-head rule in `tui-design.md`).
A failing tool returns an error *result* (`IsError: true`), not a Go error. The
turn continues and the model adapts.

### Schemas (`schema.go`)

`SchemaOf[T]()` derives a JSON Schema from a params struct via reflection over
`json`, `desc` and `enum` tags, with no external dependency. A field without
`omitempty` is required. Unsupported kinds panic at registration time, not at
call time.

### Registry (`registry.go`)

Holds the declared tools in registration order plus their enabled state, and
satisfies `agent.ToolSet` so the loop reads tools straight off it.

- `Register(t, defaultEnabled)` — built-ins, extensions and MCP servers all
  register the same way: one toggleable `/tools` row (label + source) that always
  shares enable state with the tool it names.
- `Units(offered []Tool)` — collapses offered tools into toggleable `/tools`
  rows: a group whose every member is present becomes one row carrying all of
  them, and a partial or non-atomic change falls back to per-member rows. The
  session file persists the resulting enable set across resume.
- Expanding any named tool group changes the prompt's tool block, so the cached
  schema list is invalidated; disabled tools are invisible to the model. The
  sub-agent's tool set is built from this via a narrow `ToolSource`, so a child
  never sees shell or agent_* tools (see `subagents-design.md`).
- **Generic output bound** — every tool that does not bound its own output
  (`SelfBounding`) is wrapped at registration to keep model-visible content
  bound. Read-only marking for MCP comes from `annotations.readOnlyHint` or config
  globs; the permission barrier uses this metadata (see "Read-only" below).

### Sub-agent tool set and preview seams

The child agent's structural filter (which tools a sub-agent may call) is owned
by the registry but specified in `subagents-design.md`; it lives in
`pkg/subagent/toolset.go`. The optional-tool seams the guard chain relies on are
methods on `Registry`:
- `DryRun(call agent.ToolCall) error` — dispatches to the tool's optional
  `DryRunner` implementation (`editTool.DryRun`) so a doomed call can be
  detected
- `Preview(call)` → `(Change, ok)` — dispatches to a tool's optional `Previewer`
  (`editTool`, `writeTool`). `Change{Path, Before, After}` describes what the
  call would write, rendered before execution.
- `MustSerialize(calls)` — reports whether any call would prompt, so dispatch
  runs the batch serially and approval dialogs open in submission order.
### Guards (`guard.go`, `asker.go`)

A guard decides a call (allow, deny or ask) and an optional registered asker
turns any `Ask` verdict into a final allow/deny. A user's own staged shell line
is marked on the context so it stays exempt from every permission mode.

Guards run in registration order before a tool executes; first non-allow wins.
Core registers none by default. The agent runs unguarded unless configured with
a guard. A denial becomes an error result carrying the reason, and nothing
touches disk.

`Ask` consults the registered asker (set via `SetAsker`) when one exists; the
asker turns it into a final allow or deny, and returning `Ask` again is treated
as a denial. With no asker registered an `Ask` still refuses. Nothing changes
for callers that do not opt in.

### Rendering a change before it runs

`guardedTool.Execute` renders a `Previewer`'s `Change` through `Output.Diff`
**before the guard chain**, not after the tool applies it. That ordering is the
point: an approval dialog is a transient live-block region capped at a handful
of rows, so a diff shown *inside* it is necessarily truncated. Committing the
full diff first puts the whole change in the permanent record and lets the
dialog sit below it with a one-line subject that names what is already on
screen.

Invariants:

- **Once per call.** The render sits before the guard loop, so a re-asking asker
  cannot print the change twice. What is rendered once stays in the record,
  followed by the denial summary or the error when the call was refused — a mode
  like `allow-all`, which never prompts, would otherwise show nothing at all.
- A `Preview` error (bad arguments, unreadable file) renders nothing and lets
  `Execute` surface its own error. Tools must therefore not rely on the render
  having succeeded before execution.

Cost: write/edit read the target file twice per call (preview, then execute).

The permission layer registers both: one guard from `permit.Barrier` runs static
classification, and its asker resolves prompts into allow/deny with session
memory. A reason typed for an "allow with note" or a denial is injected as a
user message immediately after the tool call it governs, never as tool output.
The model therefore treats it as operator intent (see `prompt-design.md`,
provenance). Config's `permissions.safeCommands` lets a user declare extra
tools, whole MCP server namespaces (`sectool` covers every `sectool__*` tool),
or bash command lines to auto-allow as read-only; it can never name
`write`/`edit`. Core never does. `auto+write` is the one mode that runs
`write`/`edit` without a prompt, and only when the call's path resolves under
its roots (cwd and the temp dir); resolution goes through the tool's own
`PathPolicy`, so a symlink pointing outside lands out of scope and still
prompts. Three carve-outs are load-bearing. A bash call's own `cwd` argument
rebases every relative path, so it is resolved and scope-checked before the
command is. A `cd` segment does the same mid-line: its target is checked like
any other path, and both the old and new directory stay candidates, because a
control operator may skip the `cd` and a later path must be in scope either way.
That is why `cd sub && mkdir ../x` is refused. VCS metadata (`.git`, `.hg`,
`.svn`) is excluded from the roots, since a hook or `core.hooksPath` written
there is code that executes on the next git invocation. `rmdir -p` is permitted
because it walks only the operand's own components, so its shallowest one is
scope-checked and everything it can reach sits under that. The
`WithUserInitiated` marker rides the context so a user's own staged `!` shell
line is exempt in every permission mode; it is the human's shell, not the
model's. Shell commands are name-trusted only when they have no exec or write
form; `awk`, `rg` and `sort` are verified per invocation like sed. Anything
unverifiable prompts.

### Headless: the tool set is the gate

A one-shot run (`-p`) has no dialog to open, so it never lets an ask arise. The
barrier runs at `allow-all` and the **offered tool set** carries the policy
instead: the model is only ever handed tools it is allowed to call, so it never
spends a step discovering a refusal. `tools.ReadOnlyBuiltins` names what
survives `--read-only`; `ask_user` is excluded from every headless scope because
nobody can answer it.

The one exception is `permissions.deniedCommands`, which is still installed and
still refuses before the allow-all short circuit. It is the only headless
refusal path, it only fires when an operator configured it, and it covers what a
tool-name flag cannot, such as a bash command line rather than a tool. Such a
refusal is an error result like any other, so the turn adapts and continues.

The scope decides the built-in names outright, ignoring `tools.enabled`, because
the config default omits `grep`/`ls`/`find`. It does **not** re-enable a tool
its source registered disabled, so an MCP server switched off in `mcp.json`
stays off.

## Built-in tools

The sub-agent trio (`agent_start`, `agent_poll`, `agent_list`) is also
registered under the builtin source, so `/tools` sorts it up front with the core
tools, ahead of any MCP group. The three are presented and toggled as one row. A
single `subagents` entry (`RegisterGroup`) appears instead of three individual
tools.

| Tool | Default | Mode |
|--------|----------|----------|
| `read` | enabled | parallel |
| `write`| enabled | serial |
| `edit` | enabled | serial |
| `bash` | enabled | serial |
| `find` | disabled | parallel |
| `grep` | disabled | parallel |
| `ls` | disabled | parallel |
| `git_status` | disabled | parallel |
| `git_log` | disabled | parallel |
| `git_show` | disabled | parallel |
| `git_diff` | disabled | parallel |

`find`/`grep`/`ls` are off by default: with `bash` available the model can use
`rg`/`find`/`ls` directly. They exist for the sub-agent (which has no shell) and
for configurations that run without `bash`. The four `git_*` readers share that
default and that role. Their engine choice, safety model and sub-agent repo-context
gate are covered in *Git inspection tools* below.

### read (`read.go`)

Line-numbered (`cat -n` style) output so `edit` and the model agree on
positions. Output is bounded by lines, bytes (each at whole lines) and a
per-line rune cap. read never spills: the source file is its recovery, so the
footer pages with `offset`. A range wider than the line limit is refused. Binary
files are refused. Every successful read is recorded in the tracker.

An image file reads as an image block plus its sizing note on a vision model.
On a text-only model the read is refused with a note naming that, so the model
does not spend a read it cannot be paid back for. Fitting follows *Images*
below. The gate is a pull, not a mirror: `tools.Options.Vision` closes over the
llm registry's active model and is consulted inside `Execute`, so a `/model`
switch, a resume or a plan phase shows up on the very next read with no host
push. A sub-agent on a different `subagent.model` shares the parent's gate and
can therefore be refused where it could see images: single-sourced on purpose,
and the harmless direction.

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

String replacement against an in-memory buffer, written once at the end so a
multi-edit batch is all-or-nothing. One shared apply path serves both `Execute`
and `DryRun`, preceded by an order-independent validation pass (empty or
duplicated old text) so nothing fails after any write. Every op's span resolves
against the **original** buffer, never another edit's output — edits cannot
cascade, and overlapping spans across ops are rejected.

A match ladder resolves each `oldText` through four tiers: exact byte-for-byte,
then canonical (unicode lookalikes folded to ASCII), a whole-block indent shift,
then a fuzzy per-line match. Escalation happens only on *zero* matches; a tier
that finds several leaves the ambiguity for the caller to reject rather than
guessing which was meant. Canon and indent prove their span equal to the
`oldText` it claims before writing, so a mapping bug degrades into a no-match
error instead of corrupting the file; fuzzy cannot, and earns its span through
its own guards below. An `oldText` whose canonical fold is bare whitespace is
refused above exact, since it would match every line boundary; the fold drops
zero-width runes, so one carrying them still refuses. The indent tier applies
only to a uniform whole-block shift, where both texts' non-blank lines share one
base indent, in either direction including onto a block the file holds flush
left; a mixed or nested tab/space conversion matches nothing and falls through
rather than being applied wrongly. Any match above exact reports in one line
what differed, so the next edit is written correctly. Each differing run is
widened to whole identifier and number tokens before it is quoted, since a value
and the file's often share an edge digit (`4096` against `65536`) and a
byte-level cut would quote back halves of a number; a non-exact match names
every line that drifted. The canon tier tells a trailing-whitespace
difference from a lookalike by trailing-trim equality rather
than by stripping all whitespace, which folds an nbsp away and misreports it,
and names the characters that differed.

The canon tier is skipped when `oldText` and `newText` canonicalize to the same
text, since matching would rewrite already-correct text with itself. Indent
still runs, as a block quoted at the wrong depth remains provable. A
byte-identical pair is refused above exact, since with no rewritten span a
duplicated `newText` and a mis-transcribed `oldText` are the same bytes with
opposite intent.

That principle also governs what a non-exact match writes. Only the run between
the site's and the replacement's common affixes comes from `newText`; text the
edit merely quoted keeps the file's own bytes, so a lookalike the model
flattened survives outside the change. A site that folds to the replacement is
written whole, since there the fold is the edit.

The fuzzy tier heals a mis-transcribed value like `queueDepth = 256` where the
file says `1024`. It works line by line. A line the region differs on must be one
`newText` rewrites, and must differ only inside the part of that line `newText`
rewrites. That is what makes writing `newText` the requested edit. A line the
edit only quotes is refused, because applying would rewrite text the model was not
changing and nothing in the call says whether it meant to. The same holds for a
drift outside the rewritten part of a line it is changing.

Healing needs `oldText` and `newText` to hold the same number of lines, since only
then does each line pair up and the drift stay checkable. An edit that adds or
removes lines applies through the tiers above or not at all. Further guards apply:
only a bounded set of lines may differ in one edit, each by one small drift with
exact context anchoring it; the region must be the clear best match (a second close
region is a guess outright, and one just past the limit separates a match already
thin); no line may overlap one an earlier edit wrote, since healing there could
revert that edit; and the punctuation ending a matched line must survive into the
replacement, because every other guard is measured on `oldText` while the apply
writes over the file's line. Those line numbers hang off the tracker's observation
of the file, so a later edit remaps them past its own insertions. Any change the
tool did not make drops them rather than leaving them pointing at unrelated lines,
while a re-read of unchanged content keeps them since the numbers still name that
text. A line differing only in indentation is left to the indent tier, which proves
a uniform shift or refuses, because healing it here would reformat the file.

Diagnostics run only after every tier failed and reason in canonical space, so
with each lookalike difference already rejected they name the real cause
(whitespace or casing, genuinely-absent content, an earlier edit in the same
batch) rather than blaming a stray smart quote. A spacing mismatch is one
comparison of the two lines' whitespace runs, reported once per distinct
difference with the lines it covers. The message ends with the closest text
verbatim for copying: chosen by whole-block agreement over per-line token
overlap (which previously landed hints outside the intended block), rendered
untrimmed without a line gutter since position lives in the header and it must
be reproduced byte for byte. Below a similarity floor nothing is offered at all
— admitting no close match beats naming a decoy.

One further signal rides on that message: when the closest text differs in one
quotable run, it is named rather than left as two strings to be compared by eye,
under the same token widening.

Multiple matches without `replace_all` return the occurrence count and each
match's line; messages tell the model it **must provide text exactly** rather
than asking it to copy. Argument decoding tolerates what models emit in place of
the declared schema (a double-encoded object, a JSON-stringified `edits`, a
single edit where an array is declared), since rejecting those costs a round
trip without saying anything new. Singleton wrapping now lives in
`strutil.DecodeArgs` for every tool: a lone value whose shape fits a declared
array field heals into a one-element array; leaf values are never reinterpreted.
Decode errors surface via `DecodeArgs` in model-facing shape (field plus
expected/actual JSON kind), never leaking Go type names.

Feedback returns a unified diff via `go-udiff` directly (`pkg/tools` never
imports `pkg/tui`), bounded by `Elide`. The added side is the model's own text
and the removed side names what the file actually held, so it makes a non-exact
match explain itself. It rides only when there is reason to check the result: a
match tier fired, one edit landed on several sites, or the apply duplicated text
— its written text (as reindented, when a tier shifted it) now occurring
elsewhere in the file, or a written line repeating its neighbour. An edit
matched byte-exactly and landed where aimed returns the summary alone: the diff
would only restate arguments the model just sent, and extra tokens become text
for the next `oldText` to be modelled on.

Line endings follow one package-wide convention: model-visible output is always
LF, while a write copies untouched regions verbatim and gives each replacement
its neighbouring lines' ending. `write` overwrites with the existing file's
majority ending, and a new file gets LF.

### bash (`bash.go`)

One non-login `bash -c` process per call (a login shell would reset PATH to the
system default and hide user dirs like `~/.local/bin`, Homebrew or nvm). There
is no persistent shell, so `cd` and state cannot confuse later calls. Streams
stdout and stderr interleaved to the UI while teeing a bounded copy for the
model; every kept line is capped, and output past the limit (or carrying one
overlong line) spills **the complete stream** (head + overflow) to a file under
`os.TempDir()/ajent-<session>` and the model gets a pointer to it. Because that
spill is an ordinary readable text file, the model can open it with `read` and
page through it, exactly as it does for grep's spilled results. Bash differs
from read only in that its output has no pre-existing source file to re-read, so
the footer names a written one instead of a next offset. ANSI escapes are
stripped from captured output,
**with the strip position carried between chunks**: `os/exec` splits at pipe
reads, so one sequence can straddle two writes and stdout and stderr share one
filter under the ordering lock. The child runs in its own process group; on
timeout or cancellation the whole group is killed so grandchildren cannot leak,
and the model is told it was a timeout.

The cancellation contract: each run owns its process group; when the parent
context is cancelled (a turn interrupt, or `Stager.Cancel` on a `!` line), the
whole group is SIGKILLed, whatever partial stdout/stderr arrived rides in the
result, and the result is an **error result** marked as interrupted with the
shared interruption text, so the transcript reads as an interruption. A timeout
stays a distinct non-error result. Its status line names only what that note
does not: an exit code, or `signal: <name>` for a death by any other signal. The
kill we send adds nothing beyond its own note. The environment forces
non-interactive settings (no pagers, no terminal prompts, no colour).

### find / grep / ls (`find.go`, `grep.go`, `ls.go`)

Off-by-default extras for no-shell agents.

- `find`: glob matching with `**` support; a bare pattern (`*.go`) matches at
  any depth. It lists files via `git ls-files -z` (quoting disabled, so non-ASCII
  filenames stay usable), falling back to a bounded Go walk when git is
  unavailable or yields nothing. A search never leaves its root: the listing is
  scoped to `Path`, and results are sorted newest first.
- `grep`: searches file contents for a pattern in three modes — `files`, `content`
  (line numbers plus optional context lines) and `count`. Enumeration stops at the
  default match cap with a named note, never silently; surfaces stderr from an
  underlying tool as an error.
- Both spill their complete result when the bound cuts it, so the model can page
  the rest recoverably rather than losing it.
- `ls`: one directory's entries (or the files a wildcard pattern matches via
  `filepath.Glob`), sorted alphabetically with `/` suffix on directories. A glob
  with no matches is an error, so it is never mistaken for an empty listing.

Like `read`, each sets `ToolResult.Display` to the same text as its
model-visible `Content`, so history renders it through the shared output-head
rule instead of a tool header with no body.

These off-by-default extras are exactly what a read-only sub-agent needs: they
are always available to a child regardless of parent enable state, so a
delegated investigation can `find`/`grep`/`ls` without ever reaching for shell.
The plan workflow's planning and review scopes enable them explicitly for the
same reason.

### Git inspection tools (`git.go`, `status.go`, `log.go`, `show.go`, `diff.go`)

The four git readers inspect repository content, and a repository is attacker-shaped
input. The parent already has `bash`, so it can run `git` freely through the
permission barrier. These tools exist (like find/grep/ls) for read-only sub-agents
that have **no shell** and, more importantly, **no permission guard**. A child's
tool set is structural: it gets exactly the named read-only tools and nothing can
widen it. So a git tool offered to a child must be *verifiably* read-only against an
untrusted repository, not merely intended to be.

The four operations:

| Tool | Operation |
|------|-----------|
| `git_status` | branch/HEAD, staged/unstaged changes as short paths + counts |
| `git_log` | recent commits: hash, author date, subject and an optional path filter |
| `git_show` | one commit (or ref): full metadata plus the patch it introduces |
| `git_diff` | diff between two refs, of a single commit, or against the working tree (`to: "worktree"`) |

#### Why exec of system git is not enough

The obvious implementation shells out to the `git` on PATH, exactly as
`walk.go` does for `ls-files` and `rev-parse`. That works there because those two
porcelain commands do not execute repository-controlled code. These tools are
different. A work tree is attacker-shaped: `.gitattributes`, `.git/config`,
`core.hooksPath` and pager/alias configuration all come from whoever owns the
checkout, and several read-only-looking plumbing paths turn that into execution:

- **textconv / clean / smudge filters**: a `*.pdf diff=...` or `filter=` entry
  in `.gitattributes` names an arbitrary command git runs when materialising blob
  content for `show`, `diff` and `log -p`. This is the classic RCE surface: clone
  a malicious repo, run any read-only history inspection, and code executes.
- **hooks**: `core.hooksPath` plus hook scripts can fire on porcelain that looks
  read-only (e.g. index-touching paths). Not all ops hit them, but the blast radius
  is wider than a single command.
- **pagers / aliases / config includes**: `git -c core.pager=...` style defaults
  can pull in environment or subprocesses.

The permission barrier would normally catch this (a git invocation goes through
the shell analyser and VCS metadata is excluded from roots), but a **sub-agent has
no barrier**. Its tool set *is* the gate. Executing system `git` there turns an
attacker's repository into arbitrary local code execution with no approval step.
That defeats structural read-only filtering entirely.

The two viable engines:

1. **System `git` exec.** Matches existing convention (`walk.go`, `bash`),
   battle-tested output, fast on huge histories, zero new dependency. But it runs
filters/hooks and is only safe when the repo is trusted (i.e. inside the main loop
where the barrier vets each call).
2. **go-git (pure Go), module `github.com/go-git/go-git/v5`.** Parses objects,
   trees, refs and diffs in-process with no subprocess: it never reads a
filter/hook into an executable form, so a malicious work tree cannot run code.
Trade-offs: slower on large histories (notably patches over deep logs), a sizable
dependency, and its diff output needs formatting to match what the model expects
from git. (v6 existed only as alphas at implementation time, so the tools pin the
latest stable v5.)

**Decision: go-git is the engine for these tools.** The deciding factor is not
convenience but the sub-agent safety invariant that structural read-only must hold
against an untrusted repository. System `git` cannot provide that guarantee, while
go-git can.
The cost (a dependency and slower history walking) is acceptable because these are
opt-in, off-by-default tools used by no-shell children on focused investigations.

Where the two differ in output shape or correctness, a tool degrades to a clear
error rather than silently emitting something git-shaped but wrong. go-git's diff
has known edge cases. A `git_diff` that cannot faithfully represent a rename,
binary delta or octopus merge says so and offers what it can (e.g. "N files
changed, binary deltas omitted") instead of emitting a misleading unified diff.
Unified hunks are rendered by the same go-udiff engine the `edit` tool's change
preview uses (`unidiff.go`), not go-git's encoder, so both surfaces produce one
diff dialect. go-git supplies the object model (trees, changes and rename
detection). `git.go` shapes git-style per-file blocks around the shared hunks.

**Why not both, switchable.** A runtime fallback from go-git to system `git`
sounds pragmatic ("go-git in a sub-agent, git otherwise") but is rejected: the
engine that runs *inside* a child is the one with the safety requirement. A config
flag letting an operator flip a child's tool onto system `git` would reintroduce
exactly the RCE surface this design removes and split behaviour between two code
paths for no user-visible gain. The tools are go-git only. Operators who want full
git power already have `bash` in the parent.

#### Package shape and conventions

The four follow the find/grep/ls conventions exactly:

- a params struct with `json`/`desc` tags, decoded via `decode`
  (`strutil.DecodeArgs`)
- `Name()` returning a `Tool*` constant, while `Label` returns the bare tool name
  (the argument already rides on the committed header)
- `Mode() agent.ModeParallel`. These are pure reads and may run alongside siblings
- implement `selfBounding()` so the registry's generic bound does not double-wrap,
  then spill via `truncateOutput(t.sessionID, ...)` like find/grep/ls

Files under `pkg/tools/`, mirroring the existing built-ins:

```
git.go        # shared: repo open, object lookup, output shaping, engine errors
status.go     # git_status
log.go        # git_log
show.go       # git_show
diff.go       # git_diff
```

Every tool sets `ToolResult.Display` to the same text as model-visible `Content`,
so history renders it through the shared output-head rule (`tui-design.md`) instead
of a bare header. Model-facing output is LF-only (the package-wide convention) and
bounded by `GitResultLimit()`.

New bound in `pkg/tools/limits.go`: one output limit covers all four
readers (like find/grep/ls) and also caps the `git_log` default walk, while an
explicit `limit` walks up to a far larger commit count before stopping with an
explicit note.

The plan workflow enables find/grep/ls explicitly for its planning and review
scopes. The git tools join that same explicit enable so a planner/reviewer can
inspect history without shell. See `plan-design.md`, which needs no new seam since
it names read-only built-ins by name already.

#### Repo resolution

Each call takes an optional `path` (default session cwd, resolved through the shared
`PathPolicy`). The repo root is found from that path, so every object read is scoped
to **that** repository. A `git_show <sha>` never resolves against some other
checkout reachable via `.git` alternates or submodules unless the caller's path
lands inside it. This mirrors how `repoFiles` keeps `ls-files` scoped with a `.`
pathspec and drops `../` entries.

#### Registration, defaults and headless scope

In `builtins.go`, registered **disabled** like find/grep/ls under source `builtin`:

```go
reg.Register(&gitStatusTool{policy: policy}, false)
reg.Register(&gitLogTool{policy: policy}, false)
reg.Register(&gitShowTool{policy: policy}, false)
reg.Register(&gitDiffTool{policy: policy}, false)
```

The names join `ReadOnlyBuiltins` in `builtins.go`, so both the headless
(`--read-only`) scope and, via its own copy, the sub-agent filter pick them up:

```go
const (
    ToolGitStatus = "git_status"
    ToolGitLog    = "git_log"
    ToolGitShow   = "git_show"
    ToolGitDiff   = "git_diff"
)

var ReadOnlyBuiltins = []string{
    ToolRead, ToolGrep, ToolFind, ToolLs,
    ToolGitStatus, ToolGitLog, ToolGitShow, ToolGitDiff,
}
```

The permission barrier's own set (`pkg/permit/classify.go`) is derived from
`ReadOnlyBuiltins`, so all four names are also allow-listed there in one place.
Because they are read-only built-ins by name, the barrier runs them free in
`allow-read` mode, which is correct since go-git cannot mutate anything. Under the
default headless scope they stay off unless configured via `tools.enabled` or
listed in `--allow-tools`. No special-case code is needed beyond the constant list.

The sub-agent repo-context gate (a child gets git tools only when its own cwd is
inside a work tree) lives with the rest of that filter in
`pkg/subagent/toolset.go`, described in `subagents-design.md`. The parent's enable state
stays ignored, and the `agent_*` bar stays applied last.

#### Work-tree diffs

`git_diff {to: "worktree"}` covers uncommitted changes (a user refinement). Base
defaults to HEAD and `from` names any commit-ish, so one call answers both "what
changed since I last committed" and "what changed since release-X". Work-tree mode
diffs **everything since the base**, committed and uncommitted together, as `git diff
<ref>` does. It walks the union of the base tree's paths and the status entries,
never just the HEAD-relative status. Untracked files are named in a trailing note,
never diffed, matching git. `worktree` is only valid as `to` (it is the newer
state), so `from: "worktree"` is a usage error.

#### Prompt surface

Descriptions must state what each tool reads, because a child learns them from the
schema channel alone (no "Available tools" list in its system block). Each should:

- say it operates on the repository at `path` and is **read-only**
- bound its scope ("the repo containing path", never an arbitrary ref outside it)
- tell the model when output was truncated or a delta could not be rendered
  faithfully, so a partial answer is never mistaken for complete.

Provenance rules in `prompt-design.md` apply: injected git content carries no
special marker (it is ordinary tool output), but a summary that folds several calls
must name what it came from. There is no new prompt block, just tools like any other.

### ask_user (`ask.go`)

Also off by default. `ask_user(question, options?)` puts a decision back to the
user and waits: a closed choice when `options` are given, free text otherwise.
`pkg/tools` must not import `pkg/tui`, so it takes an injected `Options.Ask`;
`pkg/app` supplies an adapter over `(*tui.UI).Ask`, which already queues behind
permission dialogs, reports Esc as declined, and reports `ErrNoUI` in plain
mode.

No option list is closed: the TUI offers a free-text row and returns the typed
reply with a **negative index**, reported as a non-choice rather than as a
selection. The distinction matters. A reply dressed as an option would have the
model act on a decision the user never made.

`ModeSerial`: a question owns the terminal until answered. Every outcome is a
**normal** result, never an error: a declined question, a missing terminal and
an asker failure all come back as text telling the model to decide for itself
and state its assumption, so a headless run never blocks and never fails a turn.
It touches nothing on disk, so it is marked read-only and needs no approval. Its
description tells the model to ask only when the decision is genuinely the
user's, and never to ask permission to act. That is the barrier's job.

### Plan control tools (`pkg/plan`, source `plan`)

`dev_implement`, `dev_review`, `dev_revise` and `dev_complete` are registered by
`pkg/plan` under source `"plan"` as one `/tools` group,
**lazily on `/plan` and unregistered on every exit path**, so they never exist
outside a workflow. They are marked read-only, so the barrier runs them free
without being widened for anything else. They are the only tools that set
`ToolResult.EndTurn`, and only on their success path. See `plan-design.md` and
`agent-loop-design.md`.

Unregistering a source drops its tools but leaves the registry's group
bookkeeping in place, and group rows hide themselves when their members are
gone, so a second `/plan` re-registers cleanly.

## Shared infrastructure

### Path policy (`path.go`)

All file tools resolve arguments through one `PathPolicy`: a leading `~` or
`~/…` expands to the user's home directory, other relative paths join the
session cwd, and symlinks in the longest existing prefix are folded so every
tool agrees on one canonical path (which is also the tracker key). There is no
containment check by default. Extensions layer their own limits via guards.

### Read tracking (`track.go`)

The tracker records, per observed path, enough to detect that the file is
unchanged on a later read (its modification time, size and a content hash).
`@ref` expansion uses those records (via `Unchanged`) to dedupe against an
unchanged in-context read, at plan time and again as each injection runs, so a
batch of messages naming one path reads it once. It is shared by
`read`/`write`/`edit`, which each observe the content they produce so a later
`@file` reflects current state, and exported for reuse outside the package. Safe
for concurrent use.

The tracker also observes directory listings (`ObserveDir`) via an entry-name
fingerprint; `ls` records every real dir it lists (agent calls and injected `@`
refs alike), and `@ref` dedupes a repeat listing through `UnchangedDir`. Unlike
files there is no stable hash, so only the entry set can be compared.

Records describe the *process*, not the context, so a rewind, fork or compaction
can leave them claiming a file is in context when its read was dropped or
elided. The host calls `Reset` from every rebuild path, which makes the next
`@file` re-inject rather than dedupe against a read the model can no longer see.

### Output limits (`limits.go`, `spill.go`)

Each tool has a line/byte budget: bash and other tools 200 lines/64 kB, read
1000 lines/128 kB, and the find/grep/ls readers share 100 lines/16 kB with the
four `git_*` readers (exposed as `GitResultLimit()`). Whichever bound is reached
first truncates.

Recovery is one shared path with a single exception:

- **Spill** (bash, grep, find, ls, the `git_*` readers and the generic registry
  bound). Truncation is head-only at whole-line boundaries: keep the leading
  lines that fit within the budget, writing the complete remainder to a fresh
  file under `os.TempDir()/ajent-<session>`.

  Every spill is its own exclusive fresh file in that session dir, so concurrent
  or sequential spills never share a path and each footer pointer names exactly
  one call's output.
- **Native paging** (read). The source file is the recovery: the footer names
  the next offset and nothing is written to disk. A range wider than the limit
  spills instead of truncating.

`Elide`, rune-capped head and tail with a marker, survives for compaction's
structural reduction and edit feedback only.

### Images

Image decode, resize and re-encode lives in `pkg/img` behind a bytes-in,
blocks-out seam: the only package that touches an imaging codec. The rules:

- **Fit, don't reject.** User-supplied images are transformed to fit the inline
  pixel and encoded-size ceilings rather than refused, converting foreign
  formats to png. Anything whose pixels change (a re-encode, EXIF orientation,
  flattening an animated png to its first frame) gets a coordinate-mapping note
  beside it so spatial answers stay correct against the source. Refusal is left
  for what no transform can rescue and for headers claiming more pixels than
  decoding may allocate, checked on the header alone so a small file cannot
  promise gigabytes.
- **Normalize once, at history's door.** The registry guard wrapper is the one
  path every tool result takes, so tool-produced images are fitted there before
  session history persists them. The transcript replays blocks verbatim forever,
  and an oversized image let into history would poison every later request. A
  failure keeps the original block with a note beside it rather than dropping
  output. Bytes that neither decode nor resemble recoverable content become the
  note alone.
- **One gate.** The block-images setting replaces every image with a text note
  at that same seam, so mid-session changes take effect without restarts.
- **Classification before bytes.** The text/binary/image sniffer trusts image
  headers over the NUL scan, because most image headers contain NULs and would
  otherwise read as binary.

## Agent integration

`pkg/app` builds the set with `tools.Builtins(Options{Cwd, SessionID,
ShellCommands})` and hands the registry to the agent loop as its `ToolSet`.
Per turn the loop:

1. Mirrors the enabled names into state, so the transcript records what the
   turn could call and a later resume can restore the same set.
2. Sends the registry's cached schemas with each request.
3. Dispatches tool calls: parallel when the model supports it and every call is
   `ModeParallel`, serial otherwise. Results are appended in call order.
4. Runs each call through the guard chain and any asker inside `Execute`, and
   surfaces permission outcomes as `Display`/`Details` onto the recorded
   `ToolResultBlock`, so the transcript shows why a call ran or was refused.
