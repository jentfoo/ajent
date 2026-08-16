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

A session belongs to one agent; nothing requires every agent to have one. A
phase-13 sub-agent runs on an **in-memory session** — no transcript file, recorder,
or resume/rewind — so its only lasting trace is child spend rolled into the parent
ledger.

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
| `message` | `MessageData` | one appended message plus its stop reason and usage (assistant only); an `injected` flag marks system-provided user text that must not surface as a recallable prompt |
| `compaction` | `CompactionData` | a context reduction recorded without deleting anything (see `compaction-design.md`) |
| `model_change` | `ModelData` | a `/model` switch, by canonical key and reason |
| `setting_change` | `SettingData` | one setting change; the key is a config dotted path (e.g. `reasoning`, `tools.enabled`) and the value its JSON |
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
  id, started time, model, message count, first user prompt). It scans only
  non-directory `*.jsonl` entries so side files never surface as phantom rows.
- **Latest** is `--continue`'s target: the most recent session.
- **Find** matches an exact id or a unique prefix — what makes `--resume <id>`
  accept a few characters instead of the full ULID. An ambiguous prefix errors
  rather than guessing.
- **Prompts** returns the workspace's recorded user prompts, newest first and
  deduplicated to each distinct text's most recent occurrence. It scans up to
  100 transcript files (500 prompts), walking each append-only file in reverse
  so the newest prompt is read first.

`PromptIndex` caches that list on a short TTL (`promptTTL`) behind a mutex, so
Ctrl+R history search never rescans every transcript per keystroke. It is best
effort: a scan failure yields an empty slice rather than an error.

Sessions are scoped to the workspace they started in, so resuming from elsewhere
sees nothing and starts fresh (see "Resume modes").

## Editor history

The sessions directory also holds each workspace's message record:
`<slug>-<hash>/editor-history.lines`. It is JSONL — one submitted editor *message*
per row, oldest first — the durable record of *messages typed*, distinct from the
transcripts that record *turns*. Each message is `json.Marshal`ed onto a single
physical line, so a multi-line turn (a paste or wrapped prompt) round-trips whole:
embedded newlines are escaped and never fragment one submission into several recall
entries. Non-prompt lines (`/cmd`, `!shell`) never touch a transcript and live only
here; prompts appear in both.

The store is append-only at submit time: `EditorHistory.Append` trims trailing CRs,
drops blank and secret-prefixed messages (the pasted-secret invariant), then one
atomic `O_APPEND` write of the JSON row. A single short write never interleaves
bytes, so concurrent agents on one workspace cannot corrupt it; different workspaces
are different files.

Most rows are bare JSON strings (visible). Non-editor input that must still be
durable — an `ajent "prompt"` argv bootstrap line — is written via `AppendHidden` as a
JSON object (`{"msg":…,"hidden":true}`) so it survives restart and compaction yet
never surfaces in ↑/↓ or Ctrl+R; the corresponding turn also carries `Input.Injected`
so transcripts exclude it from prompt recall. Hidden rows are otherwise treated like
any other line for dedup, cap and secret filtering.

Recall (`EditorHistory.Recent`) reads the file plus this process's unflushed
appends, dedups to each text's most recent occurrence, and caps at `maxHistoryLines`
(1000), newest kept. When the raw file exceeds twice that it kicks a background
compaction — self-healing after a crash off the recall path. Compaction
(`EditorHistory.Compact`) rewrites via `config.WriteFileAtomic`: read-current,
merge local appends, dedup + cap, replace. It takes **no lock**; last writer wins.
Losing at most a few messages in the read→rename window is accepted over flocking the
every-message append path. Rows that are not valid JSON (hand-edited or plain-text
leftovers) decode literally, so the file stays human-edit friendly. Compaction also
runs once on exit via `defer hist.Compact()`; it writes nothing when there is no
message to persist, so an idle workspace never gains a phantom empty file.

`RecallIndex` unifies recall onto one source: every typed message first, then any
recorded prompt not already present (backfilling transcripts written before this
phase). It reuses the `Prompt` type; a typed-only line carries a zero `At`. The old
global `~/.ajent/history` is abandoned in place — never read, migrated or deleted.

## Reading and rebuilding

`Read(path)` parses every complete line tolerantly: a trailing partial write is
skipped silently, an unparseable middle line becomes a warning that never makes
the session unopenable. Only a newer major format version is a hard error.

Two consumers read the transcript back:

- **State** (`session.State`) rebuilds `agent.State` from a branch: messages in
  order, model switches resolved through a resolver (a failure to resolve is a
  warning, never an error — the caller falls back to its active model), and
  setting changes applied (`applySetting` accepts both `tools.enabled`, the dotted
  config key, and the legacy `tools` alias so old transcripts still replay).
  `session.SettingOverrides(branch)` returns the last value per setting_change
  key for seeding a resumed session's config layer. Message assembly goes through
  one function,
  `session.ContextMessages`, which applies the newest compaction's cut and its
  structural reduction plan (the schema and replay live here; `pkg/compact`
  computes the plan — see `compaction-design.md`). A compaction collapses
  everything before its first kept entry into one summary **user** message (wrapped in `<summary>` provenance
  framing — a user role reaches every provider, unlike a system message), while
  later entries stay verbatim except where the plan stubs or drops them.
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
rec.SettingChange(k, v)    // append setting_change; k is a config dotted key
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

Every resume path reopens the file with `session.Open` (head recovery as
above), rebuilds state and replays history.
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

History search is the deliberate exception: it scans every entry of each file in
raw append order (newest first) rather than only the persisted head branch, so
a prompt on an abandoned rewind fork stays findable.

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

- Recall (↑/↓ and Ctrl+R) is per-workspace: each session dir holds only its own
  typed lines, so history does not follow you across projects.
- The legacy global `~/.ajent/history` file is abandoned in place — never read,
  migrated or deleted. Only newly typed lines reach the sessions tree.
- Sessions are scoped to the workspace directory they started in; resuming from
  a different path sees nothing. There is no cross-workspace search yet.
- The transcript keeps every branch uncapped, so heavy forking grows the file.
  Compaction reduces what is rebuilt into context but never shrinks the file
  (see `compaction-design.md` "Known limits").
- Replay intentionally drops thinking (off by default) and collapses tool
  results to one-line summaries, so a resumed session is condensed history, not
  a pixel-perfect restore of scrollback.
