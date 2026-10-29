# Control socket design

How a live session exposes itself on a per-session unix socket so another program can
watch everything it emits, and how one controller at a time may drive it exactly like
the keyboard. Why remote writes reuse the keyboard's own paths instead of adding a
second dispatch, why bulk history rides the transcript file rather than the socket,
and the invariants (one seat, one event order, kernel-verified same-user peer) that
make admitting a controller safe.

## What it is

A session with the control surface enabled binds a socket named for it under the
ajent home (see `config-design.md`). Any number of connections may observe what the
session emits, and one at a time holds the **control seat** to mutate: submit a
prompt, interrupt, cycle the permission mode, rewind, answer a dialog. Two front
ends are in-tree.

- `--serve` runs the driver with no terminal, where the socket is the only source of
  prompts and answers.
- `--attach <target>` mirrors a live session on this terminal, so two processes
  share one session and either keyboard works.

## Boundary rules

The dependency edges are load-bearing.

- **`pkg/ctl` (leaf).** The wire types, line codec and protocol version both sides
  speak. Nothing internal may appear here, since every shape in it is what an
  external controller compiles against.
- **`pkg/client ↛ pkg/app`, `pkg/tui`, `pkg/agent`.** It imports only `ctl`,
  `paths` and `session`, so a controller gets dial, discovery, typed calls, the
  event stream and transcript backfill with no protocol knowledge of its own. A
  refusal surfaces as a coded wire error rather than a transport failure.
- **The server and both front ends stay in `pkg/app`.** They sit on the seams that
  already decouple the turn loop from its terminal (the session hub and the
  interaction broker), so agent, tools and permit never learn a socket exists.

## One routing path for writes

Each write verb runs the implementation the keyboard already uses: the editor
submission, Esc, Shift+Tab, typing into the editor, the tail of double-Esc, and
answering a dialog. There is deliberately no second dispatch, so remote behavior
shares local behavior and its tests, and a controller cannot reach a state the
operator cannot produce by hand. Two rules follow from that single path:

- **Submitting is not delivering.** A submit during a running turn obeys the pump's
  queue and steer rules, so a controller never believes a prompt reached the model
  while a human gate stands ahead of it.
- **Answers are validated against what was announced.** Each pending interaction
  declares how it may be answered, so resolution needs no rule per kind of user
  input and a new surface costs no protocol change. A malformed or out-of-range
  answer is an error rather than a silently mis-mapped one, which matters because
  the barrier treats an unknown index as denial.

**Provenance.** A controller-submitted prompt records its origin on the transcript
message entry and reaches the model as an ordinary user message: no in-context
marker, and none of the injected flag's behavior changes (`prompt-design.md`).

## The seat

Ownership is single-writer and keyed to the connection rather than the process. EOF
releases it immediately, so a dead controller never wedges a session and a
replacement needs no timeout. Every write refuses an observer while naming the
holder. A forced takeover announces itself to all observers, which lets the
displaced holder downgrade without probing and everyone learn who drives now. A
controller that vanishes mid-dialog leaves the dialog open for whoever answers next,
exactly as a detached terminal does.

The local TUI holds no seat and needs none: local keys keep their existing paths, so
a remote answer races the human's through the broker under the first-resolution rule
already shared by local surfaces. Two controllers can never race one, because only
one may act. Unknown methods and refused writes are legible errors rather than
silent no-ops.

## One order for events and replies

The session hub stamps every observable change with one event number at emit time,
before retention and fanout, so all connections share a single total order. Every
reply carries the number it was taken at, and that is the whole reconciliation rule:
events at or below it are superseded by the result, events above it apply. A
watermark is therefore the same concept as event ordering rather than a second
protocol notion.

Delivery is fail-open and bounded. Each connection stages outbound lines behind its
own queue, and a peer that stops reading loses its connection instead of stalling the
turn loop. Reconnect is the client's job: snapshot for truth, transcript file for
bulk history, replay from the retained window when it still covers the gap. Three
event families share that one number space: stream events mirroring the agent sink
one to one, session events for the rendered surfaces outside the turn loop, and a
single exit event last before EOF.

## Bulk history comes from the file

There is deliberately no transcript method on the socket. The greeting names the
recorded file and every snapshot carries the head to branch by, so catch-up reads
the same append-only transcript at local speed with no size bound crossing the wire
(see `session-design.md`). Only appends after a connection's snapshot announce as
events, so one entry can reach a racing client both ways, which means anything
rendering appends dedupes by entry id.

## Attach mode

Attach is an ordinary consumer of `pkg/client`, never a privileged in-tree protocol:
what the attached terminal can do, a script can do too. Decisions worth keeping:

- **No local classification.** The attached process runs no router, no agent loop
  and no transcript. Model, tools and permission mode belong to the served run and
  only paint settings are local, so flags that configure a local run are refused at
  parse time rather than silently ignored.
- **Parity is structural.** Events replay through the same sink mapping the host
  terminal applies to its own output, and committed history through transcript
  replay. State that would fight the host's stays local: editor drafts, prompt
  recall, and the pre-fill a rewind computes from the branch it already read.
- **The seat degrades honestly.** Attach claims the seat, or attaches as a named
  read-only viewer when another controller holds it. Detaching frees the seat by
  disconnection and never ends the served run, while `/exit` submitted from either
  side does.

## Serve mode

With no terminal the session hub is the only front end, which fixes three defaults:

- **Always recorded.** There is no session to serve without a transcript, so one
  that cannot open fails the start rather than producing an unrecorded run.
- **Interactive permissions.** Controllers answer dialogs, which is what the
  interactive modes assume, so headless permission mode never applies here.
- **No editor truths.** Prefill errors, queued prompts drop on interrupt instead of
  pretending a recall happened, and a rewind redraws nothing because the transcript
  is the client's view.

The run ends as it would at a keyboard: `/exit` over the wire, a signal, or local
EOF. The socket never keeps a run alive, and teardown emits exit before EOF before
unlinking, so every connection sees the end.

## Security model

Same-user and local only, verified rather than assumed. The shared home-directory
helper pins the control directory to owner-only access and the socket itself is
0600 (see `config-design.md`). There is no token or handshake secret because one
would mean nothing on a single machine: what makes a controller trusted is that it
runs as this user, and the kernel attests that rather than the peer asserting it.
Accept checks the peer credentials the operating system keeps for the connection and
closes any other uid with a refusal in place of the greeting. A platform that cannot
attest credentials refuses every connection instead of widening the model silently.

Two corollaries are enforced because they are cheap. Secret values never appear in
events, drafts or dialog outcomes, and transport lifecycle stays out of the event
stream since it is not session state and would consume event numbers and replay to
everyone. A stale socket left by a killed run is rebound while a live one makes a
second ajent naming that session refuse, and a non-socket file at the path is never
removed.

## Invariants

1. Remote writes reuse the keyboard's implementation, so nothing dispatches twice or
   carries its own permission logic.
2. One seat at a time, released by disconnection, observers never mutate, local keys
   always work.
3. One number space orders events and reply watermarks alike, so a snapshot and a
   live stream reconcile by one rule.
4. Delivery is fail-open: a slow peer loses its connection, never the session's turn.
5. Bulk history comes from the transcript file, so only deltas cross the socket.
6. Peer credentials admit same-user connections and nothing else.
