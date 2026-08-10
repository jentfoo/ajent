# Session design

How `pkg/session` persists an agent turn stream as an append-only JSONL
transcript, and how `cmd/ajent` resumes it with `--resume`, `--resume <id>` and
`--continue`. The transcript is the source of truth: every state rebuild,
replay and rewind reads it back through a branch rooted at a head id. It builds
on the agent loop (`pkg/agent`) and feeds the TUI.

## What it is

A session is one conversation, persisted as an append-only JSONL file — one
`Entry` per line, nothing ever deleted. The file lives in a per-workspace
directory so sessions survive renames deterministically without an index, keyed
by `<slug>-<hash>` of the absolute workspace path.

Goals, in priority order:

1. The transcript is the source of truth and survives any crash.
2. A session can fork: rewinding onto an earlier message starts a new branch
   while keeping every prior one reachable.
3. Resuming reconstructs agent state and replays history exactly as it was.

Goal 2 is what drove most of the shape below — branches, the `HEAD` cursor, and
the rewind picker all exist for it.

## The transcript format

One line per entry:

```go
type Entry struct {
	ID       string          // ULID (Crockford base32), lexically sortable by time
	ParentID string          // empty on the root; else this entry's predecessor
	Type     Type            // session | message | compaction | model_change |
	                        // setting_change | notice | custom
	TS       int64           // unix milliseconds (UTC)
	Data     json.RawMessage // opaque payload whose shape follows from Type
}
```

`ParentID` is what makes forking possible: every entry names the one it extends,
so a transcript is not a linear log but a tree of branches. `Branch(entries, id)`
walks that chain back to its root and returns it in order — it is the *only* read
path anything else uses, never raw file order.

The first line of every file is always a `session` entry carrying
`SessionData`: format version, cwd/workspace, the starting model key, and git
branch/commit. The version gate (`Version`) refuses to open a transcript newer
than this build understands; older or equal files are fine.

### Entry types

| Type | Payload | Meaning |
|---|---|---|
| `session` | `SessionData` | first line of the file: version, workspace, starting model, git state |
| `message` | `MessageData` | one appended message plus its stop reason and usage (assistant only) |
| `compaction` | `CompactionData` | a context reduction recorded without deleting anything |
| `model_change` | `ModelData` | a `/model` switch, by canonical key and reason |
| `setting_change` | `SettingData` | one setting change (`reasoning`, `tools`) as key + raw value |
| `notice` | `NoticeData` | a user-visible notice worth replaying on resume |
| `custom` | `CustomData` | opaque extension state that must survive a resume |

Unknown types and unknown payload fields round-trip untouched, so older binaries
can read files written by newer ones.

### IDs

IDs are 26-character Crockford ULIDs: a 48-bit millisecond timestamp plus 80
random bits. They sort lexically by creation time (so `Store.List` can order
sessions newest first), and the random tail makes them unique without a central
counter. Within one process, IDs sharing a millisecond are made monotonic by
incrementing the previous random suffix rather than re-rolling entropy.

## The writer

```go
type Writer struct {
	f    *os.File // nil for Discard
	path string   // transcript file; empty for Discard, drives HEAD persistence
	head string   // last appended entry id (or the rewind target after SetHead)
	mu   sync.Mutex
}
```

`Append(typ, data)` stamps `ParentID` from the current head and writes exactly
one line under the mutex, so concurrent appends form one linear chain. The new
id becomes the head only *after* a successful write — an append that fails to hit
disk never advances the cursor.

- **Create** makes a fresh file and writes its `session` entry first.
- **Open** reopens an existing file for append and recovers the head from its tail.
- **Discard** returns a writer with no backing file, so callers stay branch-free.
- **Sync** flushes at a turn boundary *and* persists the current head; it is
  never called by `Append`.
- **SetHead(id)** rewinds to an earlier id so later appends fork from it. The
  transcript keeps both histories — nothing is deleted — and the new tip becomes
  the persisted `HEAD`.

## Durability

Two boundaries matter, and they are deliberately different:

1. **Per message.** The recorder wires `agent.Options.OnMessage` to append one
   `message` entry as soon as the loop produces it (`Recorder.Message`). This is
   the crash path: a process killed mid-turn resumes with its tool results intact,
   because every completed step was already on disk before the next began.
2. **Per turn.** The wrapped sink calls `Writer.Sync()` at each `TurnEnd`, which
   fsyncs and records the head cursor so resume continues from exactly where work
   stopped, not just wherever a crash happened to land.

Write failures never end a conversation: persistence errors surface as an
error-level notice through the sink rather than failing the turn. A broken disk
should degrade to "not recorded", not kill the agent.

## The HEAD cursor

The one mutable piece of an otherwise append-only design is `HEAD`, persisted at
`<session dir>/HEAD`. It points where work continues after a fork: which
transcript file in the directory and which entry id inside it.

```go
type HeadCursor struct {
	File string // base name of one .jsonl session in the directory
	ID   string // active branch head inside that file
}
```

`WriteHead` is atomic (temp-file rename) and runs on every `SetHead` and at turn
boundaries. `headFor(path, entries)` resolves a transcript's effective head: the
persisted `HEAD` when it points into this file *and* its id still exists,
otherwise tail recovery. That fallback matters — if `HEAD` is ever lost or
corrupt, resume degrades to "continue from the end" instead of losing the branch.

## The store

```go
type Store struct { root string } // <config dir>/sessions
```

A workspace maps to one directory `<root>/<slug>-<hash>`, so renaming a project
does not orphan its sessions (the slug is cosmetic; the hash pins it). The store:

- **Create** starts a new session file named by UTC timestamp + id.
- **List** returns every session for a workspace, newest first (`Info`: path,
  id, started time, model, message count, first user prompt).
- **Latest** is `--continue`'s target: the most recent session.
- **Find** matches an exact id or a unique prefix — what makes `--resume <id>`
  accept a few characters instead of the full ULID. An ambiguous prefix errors
  rather than guessing.

Sessions are scoped to the workspace they started in, so resuming from elsewhere
sees nothing and starts fresh (see "Resume modes").

## Reading and rebuilding

`Read(path)` parses every complete line tolerantly: a trailing partial write is
skipped silently, an unparseable middle line becomes a warning that never makes
the session unopenable. Only a newer major format version is a hard error.

Two consumers read the transcript back:

- **State** (`session.State`) rebuilds `agent.State` from a branch: messages in
  order, model switches resolved through a resolver (a failure to resolve is a
  warning, never an error — the caller falls back to its active model), and
  setting changes applied. A compaction collapses everything before its first
  kept entry into one summary system message while later entries stay verbatim.
- **Replay** (`session.Replay`) condenses the same branch onto a sink so a
  reopened session shows its history: user prompts open turns, assistant content
  and tool calls stream through, notices replay, and each turn closes with its
  stop reason and usage. Thinking is off by default — it reads as noise on resume.

Both share one invariant carried over from the agent loop: the rebuilt context
must stay well formed (every `ToolCallBlock` matched by a `ToolResultBlock`) or
the next request would be invalid. Rebuild checks this and warns if not.

## The recorder

The `Recorder` bridges an agent turn stream onto a writer without coupling the
agent to sessions:

```go
rec.Message(info)          // append one message entry (durability path)
rec.Sink(next)             // wrap: persist notices, fsync at TurnEnd
rec.ModelChange(m, reason) // append model_change by canonical key
rec.SettingChange(k, v)    // append setting_change as raw value
rec.Custom(type, v)        // append opaque extension state
```

The wrapped sink forwards every event to the real one while persisting what must
survive. Tool results are folded into their message entries (the loop appends a
user `message` carrying them), so replay can collapse each call to its result.

## Branches, tips and rewinding

Because nothing is ever deleted, an abandoned fork stays reachable. Two views
make that usable:

- **Tips** lists every chain tip in file order — entries whose id is not another
  entry's parent. The persisted head and each fork appear exactly once, so no
  branch becomes unreachable after switching away.
- **TreeRows** walks the whole transcript as a tree for the rewind picker,
  indenting by depth with box-drawing guides (`├──`, `└──`). Sibling branches
  align at one level; nodes on the current head's path are marked active. Newest
  work sits at the bottom, near where the picker opens.

**Rewinding** is how you start a new branch: picking an earlier message moves
the writer's `SetHead` to that message's *parent* (so the picked text becomes the
start of the new branch), rebuilds agent state from that head, redraws the UI,
replays the restored context, and pre-fills the editor with the full original
prompt — ready to edit or re-send. `HEAD` now points at the fork's tip; both it
and every earlier branch remain in the file.

## Resume modes

How a run decides which transcript to write is one of four modes:

```go
const (
	modeNewSession // no flag: always a brand-new transcript
	modeContinue   // --continue: auto-resume the most recent one
	modeResumePick // --resume: picker over session roots, then resume its leaf
	modeResumeID   // --resume <id>: reopen that exact saved transcript directly
)
```

| Invocation | Behaviour |
|---|---|
| *(no flag)* | always a brand-new transcript; the previous one is untouched and stays resumable by id or picker |
| `--continue` | auto-resumes the most recent session's leaf, no prompt; starts fresh if none exists. The "just get back to work" path |
| `--resume` | lists saved sessions (newest first) in a picker — first user prompt, started time, model, message count — and resumes the chosen root's leaf; cancelling or an empty list falls back to fresh |
| `--resume <id>` | reopens that exact saved transcript directly by full id or unique prefix; fails fast with a clear error if nothing matches |

`--resume` overrides `--continue`. It is parsed out of `argv` before the standard
flag parser so its optional trailing id is not greedily consumed as a positional
argument. A requested id resolves *before* the TUI opens, so a bad id fails with a
clear message instead of silently starting a fresh transcript.

Every resume path reopens the file (`session.Open`) — which recovers the head from
`HEAD`, falling back to tail recovery — then rebuilds state and replays history.
On exit, `cmd/ajent` prints:

```
Run `ajent --resume <id>` to resume this session.
```

so a conversation is never more than one command away.

## Invariants

These are load bearing. Each exists because breaking it produced a real bug.

**1. The transcript is append-only; nothing is ever deleted.** Forks, rewinds
and compaction only add entries and move `HEAD`. Deleting would orphan branches
that other tips still point at.

**2. Reads go through the branch, never raw file order.** Every consumer —
state rebuild, replay, info counts — walks from a head id along `ParentID`.
Raw-file-order reads break the moment two forks coexist in one file.

**3. The head advances only on success.** An append updates the cursor after a
successful write; an fsync records it at turn boundaries. A lost or corrupt
`HEAD` falls back to tail recovery rather than losing the branch entirely.

**4. Rebuilt context stays well formed.** Every `ToolCallBlock` is matched by a
`ToolResultBlock`, exactly as in the agent loop, or the next request would be
invalid. This is checked on rebuild and warned about when violated.

**5. Persistence failures never end the conversation.** A broken disk degrades to
"not recorded", surfaced as an error-level notice, rather than failing a turn.

## Conventions

Repository style this package follows (shared with `pkg/tui` and `pkg/agent`):

- Godocs describe inputs and outputs, not mechanism. Comments are short phrases,
  one line, only where the context is non-obvious.
- No em dashes and no non-ASCII in comments; ASCII-only throughout identifiers.
- Prefer stdlib `slices`, `maps` and `strings` over hand written loops.
- One `_test.go` per implementation file, table driven or `t.Run` subtests,
  case names three to five words in lower snake case. `require` for setup,
  `assert` for assertions; never `time.Sleep`.
- Tests use a deterministic clock (`var clock = func() time.Time ...`) so IDs
  and timestamps are stable, plus an end-to-end fork test that rewinds onto an
  earlier message and asserts both branches survive.

## Extending

- **New entry type**: add a `Type` constant, its payload struct in `entry.go`,
  and handle it where the transcript is consumed (state rebuild, replay,
  picker). Unknown types already round-trip safely.
- **Persist more of a session**: add a method on `Recorder` that appends an
  entry; keep writes best-effort so recording failure cannot break a turn.
- **New resume mode**: extend the `resumeMode` enum in `cmd/ajent`, wire it into
  `openSession`, and document it here.

## Known limits

- Sessions are scoped to the workspace directory they started in; resuming from
  a different path sees nothing. There is no cross-workspace search yet.
- The transcript keeps every branch uncapped, so heavy forking grows the file.
  Compaction reduces what is rebuilt into context but never shrinks the file.
- Replay intentionally drops thinking (off by default) and collapses tool
  results to one-line summaries, so a resumed session is condensed history, not
  a pixel-perfect restore of scrollback.
