# Sessions

Every conversation is one session, persisted as an append-only transcript file
that survives crashes and restarts. This file covers how sessions are named,
found again (resume matching), rewound onto earlier points, deleted, what
happens when several ajent processes touch one workspace, and compaction (the
context-reduction recorded into the transcript). The command-line flags that
select between these behaviors live in `cli.md`, the rewind picker keys in
`tui.md`, and the `compaction.*` config keys in `config.md`.

## Where sessions live

Sessions are scoped to the workspace they started in. Transcripts sit under
`~/.ajent/sessions/<workspace>/` as one `<timestamp>-<id>.jsonl` file per
session, so renaming a project directory never orphans its history and
resuming from a different directory sees nothing (it starts fresh). The same
directory holds each workspace's prompt recall file (see the end of this
page).

Nothing is ever deleted from inside a transcript: rewinds, forks and
compaction only append. An abandoned branch stays reachable from the rewind
picker forever.

## Ids and names

A session has an opaque id assigned at creation, plus an optional
human-readable name that makes it stable across runs.

`/session <name>` names or renames the running session, `/session` alone
reports the current name. Naming rules:

- 1 to 64 characters after trimming, not starting with `-`
- letters, digits, `-`, `_`, `.`, `/` only, so the printed resume command
  pastes back into a shell unquoted and safe
- names match case-insensitively: `Fix-Parser` reaches a session named
  `fix-parser`
- a name is usable when it matches nothing else. It may not reach another
  session by its id either, so a typo cannot silently rename you onto an
  unrelated transcript

A successful `/session <name>` prints the exact command to return with:
`resume it with ajent --resume <name>`. When a named session ends, the exit
hint uses the name rather than the id.

## Resume matching

An explicit target (`--resume <id|name>`, also `--attach` and `--delete`)
resolves in this order:

1. an exact name match (case-insensitive)
2. a full id, then a unique id prefix
3. only for `--delete`: a file path inside the workspace's session directory

Ambiguity at either step is an error naming the target rather than a guess.
A target matching no row names any damaged transcripts present, so nothing
reads as silently absent.

- `--session <name>` resolves by name only: a value that reaches a session by
  id errors instead of resuming something you never named. A new name creates
  the session and carries it for every later run with the same flag
- bare `--resume` opens a picker of saved sessions, newest first, each row
  showing its first prompt (or name), last-used time, model and message count.
  Cancelling or an empty list starts fresh rather than stalling
- `--continue` resumes the most recent session this build can open. A second
  `--continue` adopts the current tip, so it continues where the last writer
  left off rather than replaying an older fork

Every explicit target is resolved once before anything opens, so a bad id or
name fails fast with a clear error instead of silently starting fresh.
Sessions written by a newer ajent are visible but refuse to open (update
ajent first). `--continue` skips one loudly and starts fresh when nothing
else remains.

Resuming restores that branch's messages, its recorded model (the status line
and `/model` follow it), settings changed through commands (`/settings`,
`/reasoning`, the tool set) and nothing of the permission mode: every start
runs at the configured default. On exit every run prints the matching resume
command for where it left off.

## Rewind and fork

Double-tapping `Esc` while idle opens the rewind picker (row visuals and keys
in `tui.md`): a tree of everything ever recorded in this session, abandoned
branches included, so any earlier point can be picked.

Picking a point rebuilds the conversation from there:

- picking a user prompt starts a new branch before it and pre-fills the editor
  with that original text, ready to edit or re-send. Its `@file` reads drop
  with it, so re-sending re-reads fresh content
- picking an assistant reply rewinds onto it directly (it stays in context)
- picking a compaction row undoes that compaction, restoring the full
  pre-compaction context

The swap is refused while a turn runs (`Esc` first). A rewind discards shell
output staged for the branch just left and returns queued prompts to the
editor. Your current model survives the move instead of reverting to whatever
the earlier point used.

Nothing from the abandoned branch is lost: both histories remain in the file,
every entry stays reachable from the picker, and a later resume follows the
branch that was live last. This is also why rewinding is the only "undo":
there is no `/compact undo`, rewind onto the compaction row instead.

## Deleting sessions

`--delete <id|name>` removes one transcript by the same matching rules as
`--resume`, with no confirmation (you named it). It resolves newer-format
transcripts too, since deletion needs no decode and that is the only way to
free a held name. A session another ajent instance serves live refuses the
delete, with `--force` overriding for that one target.

`--delete-old [days]` sweeps unnamed sessions unused for 28 days (or the
given window): it lists what it will remove and reads a `y/N` answer, with an
unreadable answer declining rather than deleting blind. A name marks a
session as worth keeping, so named ones are never swept.

Removing a session also removes files left beside it by interrupted writes,
leaving siblings untouched.

A fresh session abandoned before its first prompt is deleted automatically on
exit: it has nothing to resume and would only be a dead picker row, unless it
is named (`--session` must return to it) or another instance serves it live.

## Concurrent processes

Nothing locks a transcript to one process. Two ajent instances may share a
workspace or even one session:

- appends land as interleaved whole lines in one transcript tree, each writer
  extending its own branch from its own head, so neither steers the other
- two creators racing on `--session <name>` settle deterministically: the
  older id keeps the name and the loser renames itself to `name-2`, then
  `name-3` and onward, with a notice
- live sessions are detected through their control sockets. `--delete` refuses
  a served session unless forced, and the sweep lists served sessions as
  skipped, re-checking each at removal time

You never need attach to share a terminal: two `ajent --continue` processes
on one session work concurrently, their turns interleaving in one tree.
Deleting under an interactive instance with no socket is not guarded, so it
degrades honestly instead of corrupting anything (the writer notices and
stops recording).

## Compaction

A long session eventually outgrows the model's context window. Compaction
keeps the most recent steps byte-exact and folds everything older into one
structured summary recorded in the transcript, then reports the real numbers:
`compacted 184k → 22k (summarised 41 messages)`.

Three triggers produce the same result:

- manual: `/compact` or `/compact <instructions>` (the instructions steer the
  summary). Refused while a turn streams, and reports "nothing to compact"
  when recent work is all there is
- automatic: when context use crosses the compaction point, checked at every
  turn and step boundary so it never fires mid-stream. `compaction.auto:
  false` turns only this trigger off (`/compact` still works)
- emergency: an over-window request from the provider compacts aggressively
  and retries once, instead of leaving the session bricked

The verbatim band is at least `compaction.minSteps` steps (a step is one
assistant reply plus its tool results), extended with older steps while it
stays within `compaction.verbatimFraction` of the context. Nothing inside it
is ever summarized or truncated.

The context bar and `/usage` fill against the same threshold that triggers
the automatic run, so a full bar means a compact is due. Compaction never
deletes anything: rewinding onto its row in the picker restores everything.

## Prompt recall

Each workspace keeps one prompt history beside its transcripts. `↑`/`↓`
recall only the open session's own prompts (all of its branches), while
`Ctrl+R` searches across every transcript in the workspace, so a prompt from
a rewound-away branch stays findable. Lines flagged as containing secrets are
never recorded. The keys themselves live in `tui.md`.

## Control socket

A running session can expose itself on a local unix socket so other programs
or terminals can watch or drive it: `"control": {"enabled": true}` in config
turns it on (off by default, same-user only). Observers mirror what the
session emits while exactly one controller at a time holds the seat and acts
with the authority of the keyboard. You reach that surface through `--serve`
(a session with no terminal at all) or `--attach [name|id|socket]` (a second
full terminal joining a live session, `--force` taking the seat from another
holder); both flags are described in `cli.md`, and the attach surface's keys
in `tui.md`.

## See also

- `cli.md` - the resume/delete/serve/attach flags, exit codes, headless runs
- `tui.md` - rewind picker keys, editor keybindings, rendering
- `commands.md` - slash commands and the input-line grammar (`@path`, `!shell`)
- `config.md` - the `compaction.*` keys, their layers and `AJENT_*` bindings
- `permissions.md` - permission modes carried by a resumed session's tool set
