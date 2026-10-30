# Session design

How `pkg/session` persists an agent turn stream as an append-only JSONL
transcript, resume and retirement: `--resume`, `--resume <id|name>`,
`--session <name>` and `--continue`, and how it retires one with `--delete` and
`--delete-old`. The transcript is the source of truth: every state rebuild,
replay and rewind reads it back through a branch rooted at a head id. It builds
on the agent loop (`pkg/agent`) and feeds the TUI.

## What it is

A session is one conversation, persisted as an append-only JSONL file: one
`Entry` per line, nothing ever deleted. The file lives in a per-workspace
directory so sessions survive renames deterministically without an index, keyed
by `<slug>-<hash>` of the absolute workspace path (the slug encodes the whole
path, the hash pins it).

A session belongs to one agent, and nothing requires every agent to have one. A
sub-agent runs on an **in-memory session** (no transcript file, recorder, or
resume/rewind), so its only lasting trace is child spend rolled into the parent
ledger.

Goals, in priority order:

1. The transcript is the source of truth and survives any crash.
2. A session can fork: rewinding onto an earlier message starts a new branch
   while keeping every prior one reachable.

Goal 2 is what drove most of the shape below: branches, the per-writer head
slots, and the rewind picker all exist for it.

## The transcript format

One line per entry. An entry names its own id and its parent's id (except on the
root), a type tag drawn from a small set (session | message | compaction |
model_change | setting_change | notice | custom), a unix-millisecond timestamp,
and an opaque payload whose shape follows from the type.

`ParentID` is what makes forking possible: every entry names the one it extends,
so a transcript is not a linear log but a tree of branches.
`Branch(entries, id)` walks that chain back to its root and returns it in order;
it is the *only* read path anything else uses, never raw file order.

The first line of every file is always a `session` entry carrying `SessionData`:
format version, cwd/workspace, the starting model key, and git branch/commit.
The version gate (`Read`) refuses a transcript newer than this build understands
with a classifiable error (`ErrNewerFormat`). Older or equal files are fine. The
refusal still returns the parsed entries, so identity survives: selection
classifies such a file as `Info.Blocked` and fills only the fields that identify
the file (id, name, timestamps), never branch-derived ones. Blocked rows stay
visible to every selection surface (`List`, `Find`, `FindNamed`, `NameConflict`)
so they can be named, deleted or explained, but no path may hand one to `Open`:
an append-open would repair a torn tail before it could refuse, and appending
must never happen at all. Selection returns the fact (`Info.Blocked`), and each
entry point owns its own surfacing policy (detailed in the resume table below).

### Entry types

| Type | Payload | Meaning |
|---|---|---|
| `session` | `SessionData` | first line of the file: version, workspace, starting model, git state, optional name |
| `session_name` | `NameData` | a name set or changed after creation (`/session <name>`); the newest one wins |
| `message` | `MessageData` | one appended message plus its stop reason and usage (assistant only). An `injected` flag marks system-provided user text that must not surface as a recallable prompt, and an `origin` string names a non-typed submitter (`controller`, see `control-design.md`) without changing how the message behaves |
| `compaction` | `CompactionData` | a context reduction recorded without deleting anything (see `compaction-design.md`) |
| `model_change` | `ModelData` | a `/model` switch, by canonical key and reason |
| `setting_change` | `SettingData` | one setting change; the key is a config dotted path (e.g. `reasoning`, `tools.enabled`) and the value its JSON. A JSON null marks a reset: the override was cleared, so replay returns that domain to its unrecorded state instead of applying it |
| `notice` | `NoticeData` | a user-visible notice worth replaying on resume |
| `custom` | `CustomData` | opaque extension state that must survive a resume; `LatestCustom(branch, type, v)` reads the newest one back and `CustomLanded(branch, type)` reports whether a message follows it, so a feature stores latest-wins state rather than writing its own reverse scan |

Unknown types and unknown payload fields round-trip untouched, so older binaries
can read files written by newer ones.

### IDs

IDs are Crockford ULIDs: a millisecond timestamp plus a random tail. They sort
lexically by creation time (so `Store.List` can order sessions newest first),
and the random tail makes them unique without a central counter. The counter's
timestamp only moves forward: on an advancing clock it is adopted fresh
(re-seeding entropy so prefixes spread across real time). Otherwise
(same-millisecond bursts, tests pinning `clock`, an OS stepping back) the
current timestamp is held and the random suffix incremented. IDs stay strictly
increasing within a process even when the wall clock ticks backward, while
remaining unique across processes.

## The writer

The writer owns the transcript file, its path and the current head id, under a
mutex so concurrent appends form one linear chain.

`Append(typ, data)` stamps `ParentID` from the current head and writes exactly
one line under the mutex. That write is fenced by an exclusive lock on the
transcript (see writer slots below), so concurrent *processes* interleave whole
lines rather than bytes. The entry timestamp shares the id's monotonic
millisecond: ts-derived ordering (`Store.List`, `Stale`) always agrees with append
order even when the wall clock steps back. The new id becomes the head only
*after* a successful write, so an append that fails to hit disk never advances it.

- **Create** makes a fresh file, registers the writer's slot (see below), then
  writes its `session` entry first.
- **Open** reopens an existing file for append and recovers the head through the
  slot order (see writer slots below). **OpenEntries** additionally returns what it
  parsed, so a resume can rebuild state without reading the file a second time. The
  implicit form behind `--continue` is the one shape allowed to adopt a live peer's
  head.
- **Sync** flushes at a turn boundary *and* rewrites the writer's head slot, data
  before slot, and it is never called by `Append`. A slot that could not be written
  surfaces as an error rather than silently continuing. The turn boundary is also
  where a writer notices peers arriving and leaving.
- **SetHead(id)** rewinds to an earlier id so later appends fork from it. The
  transcript keeps both histories (nothing is deleted) and the new tip becomes
  this writer's head. An empty id starts a new root, which names no branch at all
  until its first entry.
- **Close** retires the slot immediately, so a clean exit never blocks `--delete`
  for the freshness window.

## Durability

Two boundaries matter, and they are deliberately different:

1. **Per message.** The recorder wires `agent.Options.OnMessage` to append one
   `message` entry as soon as the loop produces it (`Recorder.Message`), so every
   completed step is already on disk before the next begins. Durability here is
   crash-consistent, not fsync-strong: the writes land in the file, but a crash
   before the turn's `Sync` leaves the slot behind, and reopen recovers the
   appended entries from the file tail instead.
2. **Per turn.** Each `TurnEnd` fsyncs and rewrites the head slot, so resume
   continues from exactly where work left off.

Write failures never end a conversation: persistence errors surface as an
error-level notice through the sink rather than failing the turn. A broken disk
should degrade to "not recorded", not kill the agent.

## Writer slots

The one mutable piece of an otherwise append-only design is the branch head: the
entry id where work continues after a fork, which is what a rewind updates. It is
persisted beside its transcript as one **head slot per writer**, keyed by a *slot
token* minted when the file opens for append and rewritten atomically on every
`SetHead`, at turn boundaries, and on a periodic touch.

**Per-writer, never shared.** A slot exists before this writer's first entry can land,
so a peer is visible from its earliest possible write, which is what closes the deletion
races. Two ajent processes on one session each own a slot, so neither can overwrite or
steer the other. A slot counts as live while its writer claims it and keeps touching it,
and a clean exit retires it at once. A writer suspended past the freshness window reads
as stale until its next touch brings it back.

**Head resolution on open** is a total order:

1. An explicit target (`--resume <id|name>`, rewind, plan fork) wins.
2. The newest live slot head. A second `--continue` beside a running session resumes
   from that writer's tip and then diverges on its own branch, which is the collaborative
   behavior rather than an error. A peer with no head yet names nothing to adopt, so
   adoption falls to the next live writer.
3. The newest retired slot head, extended to the file tail when that writer's own
   unsynced appends still form one unbroken chain from its head. Crash recovery is
   per writer: entries interleaved from a peer break the chain and are skipped rather
   than adopted.
4. The file tail.

Ordering is by the head entry's time with slot write time as tie-breaker, so a writer
mid new-root fork still sorts. Tails that older writers left unsynced do not
auto-continue, they stay reachable from the rewind picker. Slots are sidecars rather
than entries, so the frozen corpus and its payload rules are untouched. A session
recorded before slots existed resumes where it left off by seeding one retired slot
from the legacy cursor on first open.

**Byte-level safety stays inside the append.** Complete-line appends plus an exclusive
lock on the transcript make concurrent processes interleave entries instead of corrupting
each other, and a torn tail is quarantined under that same lock before anything else can
append to it. Readers neither lock nor truncate: they tolerate a partial last line the way
they always have. The design assumes a local filesystem, which is also what the control
surface assumes.

**Deletion meets liveness.** `--delete` and `--delete-old` consult live slots first.
Deleting a session another writer holds refuses with the number of active instances,
`--force` overrides that for one named target but never for the sweep, and the sweep lists
live sessions as skipped. Exit-time empty-session cleanup fires only when no peer holds
the file. A writer whose transcript is deleted under it anyway notices at its next append
and stops recording with a persistence error rather than writing where nothing could read
it back.

## Concurrent access

Nothing in the design locks a transcript to one process. Two ajent instances
may share a workspace directory or even one session, kept safe without
coordination by four rules:

1. **Appends interleave whole lines, never bytes.** Each append is one fenced write
   of a complete line, so concurrent writers land interleaved entries in one tree and
   nothing is lost or corrupt.
2. **Heads are per writer.** Each instance's slot carries its own branch, and resume
   resolves through the slot order above. The other instance's entries stay intact in
   the tree, one rewind away, whichever branch wins the resume.
3. **Reads are uncoordinated and tolerant.** A scan racing an append sees a consistent
   prefix or freshly landed lines, exactly as a serial scan would, and a torn tail
   quarantines itself on the next open.
4. **A parsed snapshot is trusted only while the file is unchanged.** A process holding
   one (an open's parse, a cached prompt scan) re-reads before building on it whenever
   the file has grown, so a concurrent writer's entries never land unseen.

**Peer visibility** rides the turn boundary: a writer diffs the slot directory against
what it saw last time and surfaces joins and leaves as transient notices rather than
entries, published through the ephemeral notice family so attached terminals see them too
(see `control-design.md`). **Name races** between two `--session <name>` creators settle
deterministically: the older session id keeps the name and the younger takes a numeric
suffix through a recorded `session_name` entry before its first prompt.

## The store

The store maps workspaces to directories under `<config dir>/sessions`: a
workspace is one `<root>/<slug>-<hash>` directory, so renaming a project does
not orphan its sessions. The slug flattens the whole workspace path so a
directory is recognisable at a glance, and it is lossy, so the hash pins identity.
Names are never parsed back into a workspace. The root itself is created and pinned to
owner-only permissions by the shared home-directory helper (see `config-design.md`), so
a mode that slipped wider is tightened with a startup warning rather than silently kept.
The store:

- **Create** starts a new session file named by UTC timestamp + id.
- Timestamps are stored in UTC and rendered with `.Local()`.
- **List** returns every session for a workspace, most recently used first
  (start time only breaks ties), and each row carries the summary a picker shows:
  model, message count and started time. Newest-first is what lands on the work
  actually left in progress rather than an abandoned fork. A newer-format
  transcript appears too, marked `Blocked`, so it never reads as absent. A
  transcript that holds no session entry is not a session and never lists, so
  an id-less file can never surface as a resumable row. Nor does one whose
  every `session` line fails to decode: that marks the file `Damaged`, and
  listing drops the row.
- **Find** resolves `--resume`/`--delete` targets: exact id first, then a unique
  id prefix, and names match exactly only. Ambiguity at any step fails with
  a clear error naming the candidates (see "Resume modes"). A target matching
  no row falls back to resolving it as a file path inside the workspace's
  session directory, which is how a damaged transcript stays reachable:
  deletion needs no decode, and a miss names any damaged files present so
  hidden never reads as absent.
- **NameConflict** is naming's one rule: a name is usable iff `Find` reaches
  nothing, or reaches the session being named. Names are kept narrower than the
  printable set so the exit hint's resume command is never refused at the door.
- **NameOf** resolves the current name: the newest `session_name` entry, else
  `SessionData.Name`. See invariant 2 for why it reads raw file order;
  `Info.ID/Started/Model` share that rationale (they identify the file's session
  entry, not a branch).
- **Stale** judges by `Info.Updated`: the newest entry's timestamp, or the file
  mtime when that is later. Taking the later of the two is conservative, like
  `NameOf`: work on an abandoned fork was still work. A newer-format session is
  never swept: this build cannot read what it would be deleting.
- **DeleteOld** sweeps unnamed sessions before a caller-supplied cutoff (default
  28 days), most recently used first. Its order and selection key are `List`'s
  already. Sessions another writer holds live list as skipped and are re-checked at
  removal time. Removing a session drops its sidecars too (the slot directory and any
  `.torn` quarantine), leaving siblings and editor history alone.

**Scans are concurrent and deterministic.** Each transcript parses
independently, so `List` and `Prompts` walk files through a bounded worker
pool while results keep the serial walk's order and a per-file failure still
degrades to a skipped row. `Prompts` scans newest files first and stops at its
quota, so files beyond the quota are never parsed.

Ctrl+R history search reads each transcript's entries in raw append order,
deduplicated to each distinct text's most recent occurrence across a bounded
number of files rather than the whole workspace. `PromptIndex` caches that list
on a short TTL behind a mutex so it never rescans every transcript per
keystroke. It is best effort: a scan failure yields an empty slice rather than
an error.

Plain ↑/↓ recall is scoped tighter: `TranscriptIndex` serves only the open
session's own prompts (all branches of its tree, cached on size+mtime so live
appends show up without rescans). Both prompt indexes drop secret-prefixed
prompts at read time. A fresh session with no prompts of its own falls back to
the Ctrl+R scope until it has some.

Sessions are scoped to the workspace they started in, so resuming from elsewhere
sees nothing and starts fresh (see "Resume modes").

## Editor history

The sessions directory also holds each workspace's message record:
`<slug>-<hash>/editor-history.lines`. It is JSONL (one submitted editor
*message* per row, oldest first), the durable record of *messages typed*,
distinct from the transcripts that record *turns*. Each message is
`json.Marshal`ed onto a single physical line, so a multi-line turn (a paste or
wrapped prompt) round-trips whole: embedded newlines are escaped and never
fragment one submission into several recall entries. `/cmd` lines never write a
transcript entry, and neither do `!!` runs (excluded from context), though both still
land as recall rows for ↑/↓ + Ctrl+R. An included `!` run lands as one
**injected** user-message entry when flushed, never a typed-prompt entry.
Prompts appear in both.

The store is append-only at submit time. Whether a submission counts as
secret-bearing is decided **once, on the form that reaches the transcript**: the
driver tests the classified prompt line (`line.Rest`) before appending, and
`EditorHistory.Append` re-tests every raw line after stripping its editor
escape, so an escaped `" secret:…"` cannot reach the durable file in either
layer. Append then trims trailing CRs, drops blank and secret-bearing
messages (the pasted-secret invariant), and makes one atomic `O_APPEND` write of
the JSON row. A single short write never interleaves bytes, so concurrent agents
on one workspace cannot corrupt it. Different workspaces are different files.
Every append is durable before recall, so a line lives only on disk, and an
append that fails to persist is held back in a queue capped at the same line
budget as recall, and retried by the next compaction. A retry that fails keeps
the queue, so the lines stay recallable this session rather than being dropped
by the failed rewrite.

Most rows are bare JSON strings (visible). Argv bootstrap prompts (`ajent
"prompt"`) and the `-p` one-shot record like typed lines: visible in ↑/↓ and
Ctrl+R, their turns carrying no `Input.Injected` mark so transcript-derived
recall includes them too. Only slash commands ride `AppendHidden`, durable yet
never recalled. A text is excluded from recall only when **every** occurrence
was hidden, so a slash command reusing an earlier typed prompt's text never
hides the user's own input.

Recall (`EditorHistory.Recent`) reads the file plus this process's unflushed
(failed) appends, dedups to each text's most recent occurrence, drops texts
whose every copy is hidden, and caps at a bounded line count, newest kept. When
the raw file grows well past the cap it kicks a background compaction,
self-healing after a crash off the recall path. Compaction
(`EditorHistory.Compact`) rewrites via `config.WriteFileAtomic`: read-current,
merge local appends, dedup + cap, replace. It takes **no lock**, so last writer
wins. Losing at most a few messages in the read→rename window is accepted over
flocking the every-message append path. Rows that are not valid JSON
(hand-edited or plain-text leftovers) decode literally, so the file stays
human-edit friendly. Compaction also runs once on exit and writes nothing when
there are no rows at all, so an idle workspace never gains a phantom empty file,
while a file whose every row was dropped is emptied instead of left holding
excluded values.

`RecallIndex` unifies recall onto one source: every typed message first, then
any recorded prompt not already present (backfilling older transcripts). It
reuses the `Prompt` type, and a typed-only line carries no timestamp. The
pasted-secret invariant spans both sources: editor history drops secret-bearing
input at append time and both prompt indexes drop it at read time, so one
surface cannot resurface what the other hides.
Normalization re-applies the stripped-form test to every stored row, so rows an
older build wrote raw heal out of recall on first read and out of the file at
the next compaction.

## Reading and rebuilding

`Read(path)` parses every complete line tolerantly: a trailing partial write is
skipped silently, an unparseable middle line becomes a warning that never makes
the session unopenable. Only a newer major format version is a hard error.

Two consumers read the transcript back:

- **State** (`session.State`) rebuilds `agent.State` from a branch: messages in
  order, model switches resolved through a resolver (a failure to resolve is
  a setting change left unapplied), and setting overrides applied per key
  (`applySetting` accepts both `tools.enabled`, the dotted form of any config
  key). The last value per setting_change comes from
  `session.SettingOverrides(branch)`, where a reset surfaces as null and the
  resume seed treats that as removal. Reads go through a branch, never
  raw file order: a transcript with forks restores only that branch's chain,
  not a sibling's.
- **Replay** re-submits a restored branch to a sink (`session.Replay`), so a
  rewound or resumed session shows its history as fresh committed lines rather
  than erasing scrollback. User prompts open their own turn through `UserPrompt`
  and assistant content carries stop reason and usage, with injected messages marked
  `replayed` redraw as their own block (first line the label) without opening a
  turn. Thinking is off by default because it reads as noise on resume (see
  `tui-design.md`, "Rewind and resume" for the full rendering rules).

Both share one invariant carried over from the agent loop: the rebuilt context
must stay well formed (every `ToolCallBlock` matched by a `ToolResultBlock`) or
the next request would be invalid. Rebuild checks this and warns if not.

**A resume reads its transcript once.** The open's parse feeds the startup
rebuild, so head recovery, hint and state rebuild share one read. The parse is
a snapshot under the concurrent-access rules: trusted only while the file is
unchanged, and every later path (rewind, compaction, exit) re-reads live
state.

**A resume restores the recorded tool set.** The branch's last `tools.enabled`
lands in `State.Tools`, and every later registration replays it: the driver
re-applies after its own registrations, and the MCP manager receives the same
set as its connect-time restore (see `mcp-design.md`), so any registration
order ends at the resumed frame. A recorded empty set means everything off,
while a branch that never recorded one falls back to the resolved config like
every other unrecorded setting. Until this run dispatches its first prompt
`/tools` may reshape what was restored (see `command-design.md`).

An explicit target (`--resume <id|name>`, `--session <name>`) resolves once,
before the TUI opens, so the run never scans the workspace twice. The
resolution is a locator for the open, which still parses the transcript it
names. One deleted in between surfaces as that open's failure rather than a
silent fresh start.

## The recorder

The `Recorder` bridges an agent turn stream onto a writer without coupling the
agent to sessions: it appends one message entry per message (the durability
path), wraps a sink so notices persist and each `TurnEnd` fsyncs, records model
changes by canonical key, setting changes against a dotted config key, and
opaque extension state.

The wrapped sink forwards every event to the real one while persisting what must
survive. Tool results are folded into their message entries (the loop appends a
user `message` carrying them), so replay can collapse each call to its result.

## Branches, tips and rewinding

Because nothing is ever deleted, an abandoned fork stays reachable. The rewind
picker exposes that through one tree view:

- **TreeRows** walks the whole transcript as a tree for the rewind picker,
  indenting by depth with box-drawing guides (every guide cell fixed width, so
  sibling branches align at one level) and shading to separate the live chain
  from abandoned forks. Newest work sits near the bottom where the picker opens.
  Each row carries a kind for the reserved tag column (`tui-design.md`) plus the
  clipboard payload `ctrl+x` copies (see `clipboard-copy-feature.md`), rendered
  from the entry's structured blocks rather than its display label.
**Multiple roots.** A tree normally has one root, the `session` entry. Appending
after `SetHead("")` stamps an empty `ParentID` and starts a second, which
`Branch` stops at: state rebuilt from that head contains only that root's own
chain. The plan workflow uses this to give an implementation round a genuinely
empty context (see `plan-design.md`), and `TreeRows` already renders every root,
so an extra one stays visible in the rewind picker. A new root must lead with a
`model_change` entry, or `State` has no model to resolve for that branch.

**Rewinding** is how you start a new branch: picking an earlier message moves
the writer's `SetHead` to that message's *parent* (so the picked text becomes
the start of the new branch), rebuilds agent state from that head, redraws the
UI, replays the restored context, and pre-fills the editor with the full original
prompt, ready to edit or re-send. The writer's slot now names the fork's tip, and both
it and every earlier branch remain in the file.

Every branch switch, rewind and the plan workflow's forks alike, goes through
one guarded state swap (`WithState`). The swap is refused while a turn owns the
loop, and a refusal is detected before the writer head moves or any switch
callback fires, so a refused switch leaves the run on the branch it already had.

For a **large session** (at least `rewindDeferThreshold` tree rows), navigating
the picker does not repaint committed history: alt mode keeps the retained lines
on screen while only the live block moves. The restored context still replays
once, when a message is chosen (`tui-design.md`, "Rewind and resume").

Rewinding to the parent is why an `@` reference's injected read is appended
*behind* its message rather than ahead of it (`agent-loop-design.md`,
`Input.After`): the read is dropped along with the message that asked for it, so
re-sending the refilled prompt re-reads the file instead of leaving a stale
inclusion in context. The rebuild also tells the host its context changed, which
is what resets read tracking and reseeds `@` call ids.

## Resume modes

How a run decides which transcript to write is one of five modes: always a
brand-new transcript, auto-resume the most recent one (`--continue`), pick from
the session roots then resume its leaf (bare `--resume`), reopen an exact saved
transcript by name or id (`--resume <id|name>`), or create-or-resume by name
(`--session <name>`).

| Invocation | Behaviour |
|---|---|
| *(no flag)* | always a brand-new transcript; the previous one is untouched and stays resumable by id or picker |
| `--continue` | auto-resumes the most recent session this build can open, no prompt, and starts fresh if none exists. A newer-format session is skipped with a loud notice naming it, never silently traded for an older conversation. The "just get back to work" path |
| `--resume` | lists saved sessions (newest first) in a picker showing the first user prompt, started time, model and message count, then resumes the chosen root's leaf. Newer-format rows render disabled as "newer session format". Cancelling or an empty list falls back to fresh |
| `--resume <id\|name>` | reopens that exact saved transcript directly by name, full id or unique id prefix. Fails fast with a clear error if nothing matches, and with the version hint when the match is blocked behind a newer format |
| `--session <name>` | resumes the session with that name, or creates one carrying it when the name is new. The repeatable path: the same command starts the work and returns to it. A held name on a newer-format transcript refuses instead of re-minting it, which would make a future upgrade's `--resume` ambiguous |

`--resume` overrides `--continue`. It is parsed out of `argv` before the
standard flag parser so its optional trailing id is not greedily consumed as a
positional argument. `--session` is an ordinary flag by contrast: combining it
with either `--resume` or `--continue` is a usage error, since it already means
create-or-resume. A requested id or name resolves *before* the TUI opens, so a
bad one fails with a clear message instead of silently starting fresh. For
`--session` that check also rejects a name reaching a session by *id*, which
would resume one the user never named.

Every resume path reopens the file with `session.Open` (head recovery as above),
rebuilds state and replays history. `(*sessRec).restoreState` is the UI-free
half of that (rebuilt state and setting overrides, no replay), so a headless run
resumes the same way without a front end. A failed transcript read during that
rebuild surfaces as a resume warning in every front end instead of resuming
silently empty. Later turns still fork from the writer's intact head.
A rebuilt model is pushed back out too (`syncModelUI`): the registry's active
entry (which preselects in `/model`) and the status line must name the model
the session runs, not the config default. Without that, `/model` on the
resumed model hits `SetModel`'s unchanged-key no-op while the bar keeps
labelling another model.

A one-shot run (`-p`) records its turn like any other, which is what makes
`ajent -p "…"` and a follow-up `ajent -p "…" --continue` share one transcript.
Startup routing weighs each prompt source in precedence order before it may
refuse: the `-p` flag first, then positional arguments (`ajent "why did this
fail"`), then piped stdin. A terminal pair still opens the TUI with any
positional arguments seeded in as its first prompt. Without one, an explicit
prompt runs headless like the flag and stdin beside it is never drained.
Explicit but blank input (`-p "   "`, an all-blank pipe) refuses there as usage
instead of spending a turn on whitespace. `--continue` and `--resume <id>` both
compose with `-p`, but a bare `--resume` does not, because its picker needs a
terminal, and the combination is a usage error rather than a hang. The same
holds for a piped invocation without `-p`: the pipe becomes the prompt and the
run goes headless, where no picker can open. A bare
`--resume` there refuses with a usage error (exit code 1) instead of silently
opening a fresh transcript beside the one it meant to resume. An empty workspace
still starts one, matching the picker's own empty-list rule, since there is
nothing to choose among. On exit the matching resume command (`--resume` plus the
session's name, or its id when unnamed) is printed, so a conversation is never
more than one command away.

An **empty session** (abandoned before its first prompt) is deleted on exit: it
has nothing to resume and would only be a dead picker row. A **named** session
is exempt, since `--session` resumes by name. Deleting it would make the same
command start over instead of returning to the work, and the picker labels it
rather than showing an empty row. The cleanup fires only when no live peer still holds
the file, and it drops the slot directory with it. Siblings are left untouched.

## Deleting sessions

Two flags retire a saved session, both terminal actions: they run before the TUI
opens and exit, like `--version` and `--update`.

| Invocation | Behaviour |
|---|---|
| `--delete <id\|name>` | removes that one transcript, resolved through the same `Find` as `--resume` (exact name, full id, unique id prefix). No prompt: the user named one session. A newer-format transcript resolves too, since deletion needs no decode and it is the only way to free its name from here. A path inside the session directory also resolves, so a `Damaged` transcript stays removable even though it never lists |
| `--delete-old [days]` | lists every unnamed session unused for 28 days (or the given window), asks to confirm, then removes them, excluding newer-format sessions, while damaged transcripts never list and so are never swept |

A name is the signal that a session is worth keeping, so `--delete-old` never
sweeps one. That is the same argument that exempts a named session from the
empty-session cleanup above: `--session <name>` resumes by that name, and a
swept session would silently become a new one. Unnamed sessions are the tail
nothing resumes by hand, which is exactly what the sweep is for.

Only the sweep confirms. `--delete` names one session explicitly and stays
scriptable, and `--delete-old` can remove many at once, so it prints the list first
and reads a `y/N` answer. An unreadable answer (no terminal, closed stdin)
declines rather than deleting blind.

## Invariants

These are load bearing. Each exists because breaking it produced a real bug.

**1. The transcript is append-only: nothing is ever deleted.** Forks, rewinds
and compaction only add entries and move the head. Deleting would orphan
branches that other tips still point at.

This governs entries *within* a transcript. Retiring a whole session
(empty-session cleanup, `--delete`, `--delete-old`) is a separate lifecycle
operation, gated on live writer slots: `Remove` takes the file with its
sidecars (the slot directory and any torn-tail quarantine) so nothing of it
lingers behind.

**2. Reads go through the branch, never raw file order.** Every consumer (state
rebuild, replay, info counts) walks from a head id along `ParentID`.
Raw-file-order reads break the moment two forks coexist in one file.

History search is one deliberate exception: it scans every entry of each file in
raw append order (newest first) rather than only the persisted head branch, so a
prompt on an abandoned rewind fork stays findable.

`NameOf` is another: a name identifies the transcript *file* that `--resume`
opens, not a branch inside it, so it reads raw file order. Branch-scoped
resolution would let a rewind past a rename silently un-name the session while
its entry stays on disk, freeing the old name to create a duplicate.
`Info.ID/Started/Model` share that rationale: they identify the file (the
`session` entry in raw order), not any branch.

**3. The live head wins over the file tail, except after a crash between
Syncs.** They agree only until the first fork. After a rewind, or a plan
workflow that leaves the head on the review branch while the tail is an
implementation entry, the tail belongs to a different branch. Resume and rebuild
follow the slot order (live writer, newest retired slot, tail), and an explicit
target always wins. One exception: a file holding one unbroken chain of appends past
a retired writer's last sync point was written after that sync, so the tail wins and
no unsynced turn is skipped. Slots are per writer, so a sibling session can never
answer this question for another.

**4. The head advances only on success.** An append updates the head after a
successful write, and an fsync rewrites the slot at turn boundaries. A lost or stale
slot falls back through the slot order to tail recovery rather than losing the branch
entirely.

**5. Rebuilt context stays well formed.** Every `ToolCallBlock` is matched by a
`ToolResultBlock`, exactly as in the agent loop, or the next request would be
invalid. This is checked on rebuild and warned about when violated.

**6. Persistence failures never end the conversation.** A broken disk degrades
to "not recorded", surfaced as an error-level notice, rather than failing a
turn.

## Conventions

Package-specific rules beyond the house style in `AGENTS.md`:

- No em dashes and no non-ASCII in comments. ASCII-only throughout identifiers.
- Tests use a deterministic clock (`var clock = func() time.Time ...`) so IDs
  and timestamps are stable, plus an end-to-end fork test that rewinds onto an
  earlier message.

## The frozen corpus

`testdata/branches/` holds three transcripts: a long tool-heavy branch, a fork
with two tips, and one carrying a compaction entry with a full `Reduce` plan.
They were written by an earlier build and are never regenerated.

That is the whole point. Every other test in this package writes a transcript to
a `t.TempDir()` and reads it back in the same run, so renaming a persisted JSON
field moves both sides together and the suite stays green while every session
already on disk stops loading. Renaming `Stub.callId` and `Reduce.stripThinking`
passes the entire rest of the repository's tests and fails only
`TestFixtureSchema`.

So:
**a change to any `json:` tag under `pkg/session` is a migration, not a rename.**
If the corpus fails, the question is what happens to users' saved sessions, not
how to update the fixture. Regenerating it to make a test pass throws away the
only thing that was guarding the format.

Assertions on assembled context use a `digest` helper, one line per message
summarising role, block kinds and a text prefix, compared against an inline
`[]string`. Whole shape, so a silently added or dropped message fails, but the
expectation lives in the test where a reviewer can judge it. Deliberately not
golden files: a rendered blob makes `-update` a rubber stamp, because nobody can
tell a safe regeneration from a regression.

`pkg/compact` reads the same corpus across the directory boundary (both loaders
are test-only, so the fixtures cannot be shared through an exported helper). Its
`plan_replays_to_the_measured_size` case is the one that matters: `Before` and
`After` are measured through the same `ContextMessages` the next request is
built from, so a plan recorded differently from how it was measured fails.

## Extending

- **New entry type**: add a `Type` constant, its payload struct in `entry.go`,
  and handle it where the transcript is consumed (state rebuild, replay,
  resume).
- **Persist more of a session**: add a method on `Recorder` that appends an
  entry, keeping writes best-effort so recording failure cannot break a turn.

Each new surface also needs its read path wired into `openSession`, and the
schema change reflected in this doc's entry-type table.
