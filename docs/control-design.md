# Control socket design

The control socket is ajent's second front end: a unix socket per live session
over which any program can observe everything the local terminal renders, and —
holding the control seat — operate the session exactly like the keyboard. The
wire contract lives in `pkg/ctl` (leaf, imported by both sides); the server
lives in `pkg/app` (`control*.go`) on top of the phase 33 seams (`sessionHub`,
`hubUI`, the interaction broker). This document records the decisions that must
hold when changing any of them. Transport, events and the read-only methods are
specified here only where the write half depends on them; the phase documents
(`docs/phases/34a-*`, `34b-*`) carry the delivery history.

## The seat

One connection at a time may hold the session's single **control seat**:
`attach {control: true, holder: "name"}` claims it, a second claim errors
naming the holder (`seat-held`), and every mutating method (`submit`,
`interrupt`, `mode.cycle`, `input.set`, `session.rewind`,
`interaction.resolve`) refuses an observer connection (`observer`) with the
holder named. The rules that matter:

- **The seat is keyed to the connection, not the process.** EOF on the socket
  releases it immediately, so a dead controller never wedges the session and a
  replacement attaches at once, no timeout needed. `attach` from the current
  holder succeeds and refreshes its name, and `force: true` takes the seat from
  a live one: the displaced holder's writes refuse naming the new holder from
  then on, and it learns of the takeover on its next write rather than by
  announcement. Death never answers anything: a controller
  going away mid-dialog leaves the dialog open for the local human or the next
  controller, exactly like the 34a hold-on-dialog policy.
- **The local TUI holds no seat and needs none.** Local keys keep working
  through their existing paths. Remote and local answers to the same
  interaction race through the broker and the first wins — this is the phase 33
  first-resolution rule applied across process boundaries. Two controllers can
  never race a dialog, because only one can hold the seat; a controller and the
  local human can, and that is by design.
- **Unknown methods are legible errors**, never silent no-ops. The same applies
  to every refused write: an empty `submit` line, an out-of-range dialog index,
  an `input.set` into a serve run, a rewind onto an id that is not in the tree.

## Write methods, one routing path

Every mutating method delegates to the same implementation the keyboard uses —
there is deliberately no second dispatch:

- `submit` routes through `lineRouter` (`fromEditor=false`), so `@` expansion,
  slash dispatch, shell staging and the excluded `!!` form inherit their
  behavior and tests unchanged. Submitting while a turn runs follows the pump's
  queue/steer rules and surfaces in `queued` events, so a controller never
  believes a prompt was delivered when a human gate stands first in line.
- `interrupt` is the Esc body (`sessionHub.interrupt`): turn, survey, staged
  shell, in priority order. Queued prompts recover to the local editor in an
  interactive run and drop in a serve run, exactly as the keypress would.
- `mode.cycle` is Shift+Tab (`Barrier.Cycle`/`Prev`), re-evaluating open
  approval dialogs under the new mode. A mode-change resolution and a racing
  remote answer settle once, first wins.
- `input.set` prefills the editor; it refuses while a dialog owns the input
  (naming the kind) and errors in a serve run, where no editor exists.
- `session.rewind` is the double-Esc tail minus the picker: the client names
  the head entry id, an active plan workflow ends first, and the `head` event
  fires on the move (local or remote rewinds are indistinguishable to
  observers, who re-read the branch). Unknown heads answer `not-found`; a
  running turn answers "a turn is running" with nothing moved.
- `interaction.resolve` answers any announced dialog by id. The pending event
  announces how a dialog may be answered (its options, multi-select, free-text
  and secret shapes), so resolution validates against what was announced and a
  new kind of user input needs no new rules: `{index}` for choices,
  `{indexes}` for multi pick (an empty list confirms none), `{text}` and
  `{chat}` for free text, `{cancel}` everywhere. Several shapes in one
  request, a bad shape or an out-of-range index is a legible error, never
  a silently mis-mapped answer (the barrier clamps unknown indexes to deny,
  which a typo must not trigger). Resolution raises `interaction.settled` so
  every observer dismisses the dialog; a late resolver reads `settled`, a
  stale id reads `not-found`.

**Provenance.** A controller-submitted prompt is recorded with
`origin: "controller"` on its message entry in the transcript — the same place
message-level provenance already lives (`injected`, `replayed`). It is
deliberately not the `Input.Injected` flag: the steer queue groups runs by that
flag and replay derives echo from it, so controller prompts must keep grouping
and echoing like the user lines they emulate. The model sees no marker — a
controller is an authorized human proxy, and the annotation is for audit and
resumption, not for the prompt. See `prompt-design.md`.

## Delivery: fail-open, ordered, bounded

Event delivery never blocks the session. Each connection stages outbound lines
on its own goroutine behind a bounded queue; a peer that stops reading backs
its writer up until the bounds (item count, bytes, stall window) trip and the
connection is dropped. Overflow never stalls the pump, and reconnect is the
client's job: `state.get` for truth, `transcript.entries` for backfill,
`events.replay {since}` when the retained ring still holds the gap.

Every observable change is assigned one `seq` in the hub at emit time, before
ring insert and fanout, so all connections see one total order and reconnects
are orderable. Every reply carries the `seq` it was taken at: events at or
below it are superseded by the result, events above it apply — one rule
reconciling snapshots and streams, no second protocol concept. Write replies
read the watermark before running their handler, so an event a submit causes
never lands at or below the submit's own reply.

## Serve mode

`--serve` runs the driver with no terminal: the hub is the only front end, and
the socket is where prompts, dialogs and control arrive. Decisions that shape
it:

- **Always recorded.** A transcript that cannot open is a failed start, not an
  unrecorded run. There is no session to serve without one.
- **Interactive permission default.** A controller can answer dialogs, which is
  what the interactive modes assume; `permissions.headlessMode` never applies.
- **No editor truths.** `input.set` errors; queued prompts drop on interrupt
  instead of pretending a recall happened; a rewind redraws nothing (the
  transcript is the client's view).
- **The run ends on `/exit` through `submit`, a signal, or local EOF.** The
  socket never keeps an interactive run alive: local Ctrl+C stays a local
  truth, observed by clients through `session.exit` like anything else.
  Teardown order guarantees every connection receives `session.exit`, then EOF,
  then the socket unlinks.

## Security model

Same-user local only, stated as such: the socket directory is 0700, the socket
0600, and there is no token or handshake secret. That is the entire model —
anything that can connect is already the user. Two corollaries are enforced
anyway, because they are cheap: secret-prefixed prompts and secret dialog
answers never appear in events, drafts or outcomes (`InputSecret` text reaches
its caller and nothing else), and a socket whose peer is dead (a SIGKILL
leftover) is unlinked and rebound while a live one refuses a second ajent
naming the session. A non-socket file at the path is never removed.

## Event families

Three, assigned one seq space together: **stream events** mirroring the agent
sink one to one (the json one-shot's shapes plus `tool.result`), **session
events** for the rendered surfaces outside the turn loop (`echo`, `notice`,
`print`, `entry.append`, `status`, `activity`, `queued`, `editor`, `head`,
`interaction.pending`/`settled`), and the `session.exit` lifecycle event last
before close. Pending interactions are announcements; the seat decides who may
answer them. Raw transcript payloads (including secret-prefixed prompts) are
acceptable on this transport and nowhere else.
