# Control socket design

How a live session exposes itself on per-instance unix sockets so another program can
watch everything it emits, and how one controller at a time may drive it exactly like
the keyboard. Why remote writes reuse the keyboard's own paths instead of adding a
second dispatch, why bulk history rides the transcript file rather than the socket,
and the invariants (one seat, one event order, kernel-verified same-user peer) that make
admitting a controller safe.

## What it is

A serving session binds a socket under the ajent home, named for the session and for this
instance's writer slot token (`session-design.md`), so two instances of one session never
share a path (see `config-design.md` for the control directory). Any number of
connections may observe what the session emits, and one at a time holds the **control
seat** to mutate: submit a prompt, interrupt, cycle the permission mode, rewind, answer a
dialog. Two front ends are in-tree.

- `--serve` runs the driver with no terminal, where the socket is the only source of
  prompts and answers. Serve implies control whatever `control.enabled` says, because a
  serve run without a socket has no prompt source at all. Interactive runs keep the
  default off.
- `--attach <name|id|prefix>` mirrors a live session on this terminal, so two processes
  share one session and either keyboard works. Bare `--attach` opens a live-session
  picker.

Per-instance sockets are what detached sharing needs (see `session-design.md`): a second
starter never probes or refuses, both bind, and each instance removes only paths bearing
its own token. A discovery record beside each socket carries the session identity,
workspace, model and seat state, so pickers and `--attach` resolve a target from files
alone.

## Boundary rules

The dependency edges are load-bearing.

- **`pkg/ctl` (leaf).** The wire types, line codec and protocol version both sides
  speak. Nothing internal may appear here, since every shape in it is what an external
  controller compiles against.
- **`pkg/client ↛ pkg/app`, `pkg/tui`, `pkg/agent`.** It imports only `ctl`, `paths` and
  `session`, so a controller gets dial, discovery, typed calls, the event stream and
  transcript backfill with no protocol knowledge of its own. A refusal surfaces as a
  coded wire error rather than a transport failure.
- **The server and both front ends stay in `pkg/app`.** They sit on the seams that
  already decouple the turn loop from its terminal (the session hub and the interaction
  broker), so agent, tools and permit never learn a socket exists.
- **`pkg/session` never imports control code.** Writer slots are a file mechanism in
  their own right. The socket path derives from the writer's token across that existing
  seam, never the reverse.

## One routing path for writes

Each write verb runs the implementation the keyboard already uses: editor submission, Esc,
Shift+Tab, typing into the editor, the tail of double-Esc, answering a dialog. There is
deliberately no second dispatch, so remote behavior shares local behavior and its tests,
and a controller cannot reach a state the operator cannot produce by hand. Four rules
keep that single path honest under concurrent controllers.

- **Mutating verbs run on one lane.** Submit, interrupt, recall, rewind and mode
  cycle are serialized, so guards revalidate at execution time rather than at
  request time and a reply follows the effects it acknowledges. Interrupts jump
  the queue, so Esc never waits behind a queued rewind. The lane only orders and
  validates, then calls the keyboard's implementation, and the prompt pump stays
  the only turn starter. A submit that would block on a busy pump refuses with a
  retryable code instead of parking the lane behind it, which is what keeps
  interrupt latency alive.
- **Branch switches share one state lane with dispatch.** Rewind, fork and the
  compactor's append-then-swap hold the state lane across their whole
  transaction, while turn dispatch holds it only long enough to re-check
  conditions and commit the queue's draining mark, never across the turn
  itself. That is what closes the swap-then-SetHead window: a prompt dispatched
  before a rewind lands before it or ages out, never onto the branch the rewind
  forked away from, and a compaction checkpoint can never record against a head
  a rewind moved mid-rebuild. A rewind refuses while a drain owns the loop — a
  running turn or the between-turns window over queued prompts — and interrupt
  covers that same window: Esc stops the drain, releasing the staged-command
  wait it sits in and recovering its queued prompts, exactly as it stops a
  running turn. Pump lines carry the lane's dispatch generation; a rewind bumps
  it and the pump returns aged lines to the editor with a notice naming what
  happened, so a buffered prompt is decided once, never landed by accident.
- **Rewind is one transaction.** Swapping agent state, moving the writer head and
  publishing the branch change form one ordered operation shared by double-Esc and the
  wire, so live state and the writer head cannot diverge and the last rewind wins no
  matter which surface sent it. Queued prompts clear with a recorded notice naming them,
  and their drop publishes inside the queue's own critical section beside the emptied
  rows, so pending rows resolve instead of vanishing and a prompt re-offered after the
  rewind keeps its fresh row. The clear and the staged-output discard run before any
  step that can refuse, so a model restore hitting a running turn never leaves prompts
  queued for the abandoned branch. A rewind never waits on a dialog or picker holding
  another surface, which closes with a transient notice once the head really moves.
- **Dialog answers stay off the lane.** The broker's first-resolution rule already
  linearizes them, and answer latency matters more than ordering against other verbs. A
  controller that dies mid-dialog leaves the dialog open for whoever answers next, exactly
  as a detached terminal does.

Two further rules follow from the single path:

- **Submitting is not delivering.** A submit during a running turn obeys the pump's
  queue and steer rules, and a dialog holding the editor refuses it the same way it
  refuses any write. A controller never believes a prompt reached the model while a
  human gate stands ahead of it.
- **Answers are validated against what was announced.** Each pending interaction declares
  how it may be answered, so resolution needs no rule per kind of user input and a new
  surface costs no protocol change. A malformed or out-of-range answer is an error rather
  than a silently mis-mapped one, which matters because the barrier treats an unknown
  index as denial.

**Provenance.** A controller-submitted prompt records its origin on the transcript
message entry and reaches the model as an ordinary user message: no in-context marker, and
none of the injected flag's behavior changes (`prompt-design.md`).

## The seat

Ownership is single-writer and keyed to the connection rather than the process. EOF
releases it immediately, so a dead controller never wedges a session and a replacement
needs no timeout, and an explicit yield hands it back. Every write refuses an observer
while naming the holder in a coded field, so a script can choose between takeover,
waiting and exiting without parsing prose. A forced takeover announces itself to all
observers, which lets the displaced holder downgrade without probing and everyone learn
who drives now.

The local TUI holds no seat and needs none: local keys keep their existing paths, so a
remote answer races the human's through the broker under the first-resolution rule already
shared by local surfaces. Two controllers can never race one another, because only one may
act. Unknown methods and refused writes are legible errors rather than silent no-ops.

Admission runs off the server lock, so one peer's credential check and handshake can
never delay the session, the seat or another accept.

## One order for events

The session hub stamps every observable change with one event number at commit, before
fanout, so commit order equals event order for every connection. Every reply carries the
number its result was taken at, a snapshot's own number for reads and the post-mutation
watermark for writes. That is the whole reconciliation rule: events at or below it are
superseded by the result, events above it apply. The hub owns the ephemeral view model
(status segments, activity and queue rows, idle, running, spinning, model labels, context
numbers, permission mode, tool rows, draft), and every mutation updates that struct and
fans out its event in one critical section, so a snapshot's number and its hub-held
fields — plus the writer head, read through the writer's own head func — share that one
section. The two sources that cannot join the hub lock (the dialog broker's open set,
whose begin holds its own lock across its publish, and the usage totals behind the
agent's state lock) are read immediately after, so a snapshot may describe a moment
slightly newer than its own number. That direction is safe: the newer state already
supersedes the at-or-below events, and an above-number event that re-describes it is
idempotent by entry id, dialog id or full replacement.

**Committed versus ephemeral.** Two families share the number space. The committed family
is entry appends and branch changes, and everything scrollback-visible renders from it plus
transcript backfill, so dedupe by entry id is the only rule it needs. Echo has no wire
concept at all: a prompt renders when its entry commits, never when it is submitted, and
a live append paints exactly what replay paints for the same entry, so one echo rule (kept
beside replay in `pkg/session`) feeds both. No event outside that family may move a head.
The ephemeral family is everything else (stream deltas, dialogs, queue rows, transient
notices) and is governed by the watermark alone, never reconciled against the file — with
one exception below.

**Open dialogs are state, not paint.** A dialog holding the run cannot be an event a late
arrival misses, so every snapshot lists the interactions still open, exactly as their
pending events announced them. A controller that attaches mid-dialog learns what holds
input and can answer it by id; a reconnecting one re-races locally what it still lists and
cancels racers whose dialogs settled while it was away. Reconciliation stays the one
watermark rule, by dialog id. Open dialogs ride the socket only: never the transcript
(they are runtime state, not history) and never the discovery record (which stays static
facts for pickers).

**Poll-only facts.** Not every snapshot field has a change event. The ephemeral view
model is fully event-covered (the permission mode, tool rows, queue count and idle
gained their events as the hub took ownership), so the named list holds only the static
facts — session id, workspace, cwd — plus the usage totals: their ledger accumulates
between per-report usage events, so totals are read by resnapshotting, through the one
documented read-after-unlock. The contract for a new snapshot field is to either publish
a change event or join this named list, and the hub test holds the inventory to that, so
a rendering surface can always tell which facts events carry and which only a snapshot
reads. Seat release by disconnect follows the same rule: it rewrites the discovery
record and never spends an event number.

**Reconnect is always a resnapshot.** There is no retained-window replay on the wire. A
reconnecting controller snapshots again and renders only the difference against what it has
already rendered, which removes the whole class of gaps a bounded replay buffer creates.
The difference cuts both ways: the snapshot's applier also clears what the new state no
longer lists (segments, activity rows and queue rows removed while the terminal was away,
and a spinner whose turn ended without its end event surviving the drop), so stale chrome
cannot outlive the resnapshot. The snapshot's open dialogs are part of that difference, so a dialog spanning a drop
re-races instead of stranding the run on a dialog nobody attached can see.

Delivery is fail-open and bounded. Each connection stages outbound lines behind its own
queue, and a peer that stops reading loses its connection instead of stalling the turn
loop. Each stage reserves room for the exit event, so no bound can drop it and an attached
terminal can distinguish "session ended" from "connection lost". Shutdown quiesces an
interrupted turn before emitting exit, under a bounded wait, so a wedged tool cannot hang
exit; the executor lane and the state lane drain under the same bound first, so a rewind
still mid-transaction publishes its branch.changed before the end event, never after it,
and decision dialogs root on the app-owned context so teardown settles them too.

## Bulk history comes from the file

There is deliberately no transcript method on the socket. The greeting names the recorded
file and this instance's slot token, and every snapshot carries the head to branch by, so
catch-up reads the same append-only transcript at local speed with no size bound crossing
the wire (see `session-design.md`). Only appends after a connection's snapshot announce as
events, and an append is idempotent by entry id, so an entry that arrives both as a live
event and through backfill renders once.

A submit paints an ephemeral pending row before any reply, which the committed entry
replaces on success and a drop, recall or failed submit resolves inline. Only prompts
paint one, since slash and shell lines commit no message entry and surface through their
effects. Rows therefore carry stable ids: queued and queue.dropped speak `{id, text}` rows
(a secret's text redacts away, never its id), submit takes a client correlation echoed back
as the row id, and the committed message entry records every correlation it landed for —
so each surface's pending row follows its prompt by id wherever it goes — delivered,
dropped, or back in a draft — with no matching of strings against strings. A recall is
announced like every other queue exit: queue.dropped with cause "recalled" names the row
that left for an editor.

## Attach mode

Attach is an ordinary consumer of `pkg/client`, never a privileged in-tree protocol: what
the attached terminal can do, a script can do too. Decisions worth keeping:

- **No local classification.** The attached process runs no router, no agent loop and no
  transcript. Model, tools and permission mode belong to the served run and only paint
  settings are local, so flags that configure a local run are refused at parse time rather
  than silently ignored.
- **Parity is structural.** One applier projects every control payload onto a terminal:
  stream events ride tui/sink's mapping (one tool-result fallback) and chrome families
  forward through one narrow surface interface, so the host renders its own mutations with
  exactly what an attached terminal applies to the wire: a new keyed row renders on both
  surfaces with three touches (a view-model field, an event payload, one applier case).
  Local-only surfaces are declared by staying off that interface: editor drafts, prompt
  recall from history, and the pre-fill a rewind computes from the branch it already read.
  Recall reads the same
  local stores the host keyboard recalls from (`command-design.md`), keyed by hello's
  workspace: Ctrl+R walks the workspace-wide store, arrows scope to this session's
  transcript. The one editor gesture that reaches across is Alt+Up: queue recall runs
  over the wire and the popped text fills this terminal's editor instead of the host's,
  so editing a queued prompt on either surface replaces it exactly once. The served run
  owns every other write; an attached terminal only reads.
- **The seat degrades honestly.** Attach claims the seat, or attaches as a named read-only
  viewer when another controller holds it. Detaching frees the seat by disconnection and
  never ends the served run, while `/exit` submitted from either side does.
- **Reconnect is a state machine, not an exit.** A drop without an end event keeps
  scrollback and the local draft, shows a reconnecting banner and retries with backoff,
  rediscovering by session id. It prefers the instance it was on, follows a sole surviving
  instance openly, and picks among several rather than silently switching onto a sibling's
  head. Submits while reconnecting fail legibly with the editor text preserved. Past a
  give-up window the terminal exits as a failed turn, so a script can tell a lost session
  from a clean end, and only a clean end shows the `--resume <name>` hint.
- **One goroutine owns the view.** Backfill, live events, redraws and reconnect transitions
  are messages on one queue, so rendering cannot race the reader that feeds it and rendered
  ids reconcile by entry id alone.

## Serve mode

With no terminal the session hub is the only front end, which fixes three defaults:

- **Always recorded.** There is no session to serve without a transcript, so one that
  cannot open fails the start rather than producing an unrecorded run.
- **Interactive permissions.** Controllers answer dialogs, which is what the interactive
  modes assume, so headless permission mode never applies here.
- **No editor truths.** Prefill errors are dropped, queued prompts drop on interrupt
  instead of pretending a recall happened, and a rewind redraws nothing because the
  transcript is the client's view.

The run ends as it would at a keyboard: `/exit` over the wire, a signal, or local EOF. The
socket never keeps a run alive, and teardown quiesces the turn, emits exit before EOF, then
removes only this instance's own socket and record. Every connection sees the end, and no
instance can remove another's files.

## Security model

Same-user and local only, verified rather than assumed. The shared home-directory helper
pins the control directory to owner-only access and the socket itself is 0600 (see
`config-design.md`). There is no token or handshake secret because one would mean nothing on
a single machine: what makes a controller trusted is that it runs as this user, and the
kernel attests that rather than the peer asserting it. Accept checks the peer credentials
the operating system keeps for the connection and closes any other uid with a refusal in
place of the greeting. A platform that cannot attest credentials refuses every connection
instead of widening the model silently.

Two corollaries are enforced because they are cheap. Secret values never appear on the live
surfaces (events, drafts, queue rows, recalled texts, dialog outcomes — a recall of a
secret-prefixed prompt refuses rather than leak), and transport lifecycle stays out of the
event stream since it is not session state and would consume event numbers. One exposure
is stated honestly: controllers receive the transcript path, whose entries contain such
prompts verbatim, so redaction covers the live surfaces only.

## Invariants

1. Remote writes reuse the keyboard's implementation, so nothing dispatches twice or carries
   its own permission logic, and mutating verbs serialize on one lane with guards revalidated
   at execution.
2. Branch switches and turn dispatch serialize on one state lane: a rewind, fork or
   compaction swap holds it across its whole transaction, dispatch only for its decision,
   so live state and the writer head never diverge mid-append and a rewind's cleanups
   all run inside the transaction. Interrupt covers the drain's between-turns window,
   and shutdown drains the lane before the exit event publishes.
3. One seat at a time, released by disconnection or yield. Observers never mutate, and local
   keys always work.
4. One number space orders events and reply watermarks alike. Commit order equals event
   order, so a snapshot and a live stream reconcile by one rule. A snapshot field either
   has a change event or sits on the named poll-only list, and the hub test holds that
   inventory.
5. Committed content is entry appends plus branch changes, reconciled by entry id alone.
   Everything else is ephemeral, governed by the watermark and never reconciled against the
   file, except open dialogs: they hold the run, so every snapshot lists them and every
   surface re-races what it lists.
6. A live append paints exactly what replay paints for the same entry: one echo rule,
   defined beside replay, feeds committed history and live appends alike.
7. Delivery is fail-open: a slow peer loses its connection, never the session's turn, and the
   exit event cannot be dropped.
8. Bulk history comes from the transcript file, so only deltas cross the socket.
9. Peer credentials admit same-user connections and nothing else.
10. Sockets and their records are per instance and token-scoped. A process removes only paths
   carrying its own token, and detached sharing never depends on the socket at all.
