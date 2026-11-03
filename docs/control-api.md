# Control socket API

The wire contract an external controller speaks: transport and framing, discovery,
every method a connection may call, every event it receives, every payload field name,
and how bulk session history is rebuilt from the transcript file. Every shape lives in
`pkg/ctl`, the versioned protocol both sides import. `pkg/client` is the reference Go
consumer (dial, typed calls, event pump, backfill) and is documented in full below. This
document records what a controller must know to speak the protocol from any language.
Design rationale (why one routing path, one seat, one event order) lives in
`control-design.md`.

A session serves its socket when `control.enabled` is set or `--serve` runs it, and never
in a one-shot (`-p`, piped stdio). Serve mode implies the socket: without it there is no
prompt source at all. See the README's control socket section for the operator-side setup.

## Transport and framing

A serving session binds one unix socket per instance at
`<ajent home>/control/<session-id>/<token8>.sock`, with its discovery record
`<token8>.json` beside it (see `config-design.md`). The token is the writer slot token, so
two instances of one session never share a path. The directory is 0700 and the socket 0600,
and accept admits same-user peers only: the kernel's peer credentials are checked, and any
other uid gets a `refused` event then a close. A platform that cannot attest credentials
refuses every connection.

The line protocol is newline-delimited JSON. A controller writes one request per line. The
server answers with replies and pushes events on the same socket, in one total order that
matches its event numbers.

```json
→ {"id":"7","method":"submit","params":{"line":"explain this diff","pendingId":"c1"}}
← {"id":"7","seq":412}
← {"event":"queued","seq":413,"data":{"rows":[{"id":"c1","text":"explain this diff"}]}}
```

Framing objects, one per line in the direction shown:

| object | fields |
|---|---|
| request (controller → server) | `id`, your correlation id, then `method` and optional `params` |
| reply (server → controller) | `id` and `seq`, then either `result` or `error` |
| event (server → controller) | `event` name, with `seq` and optional `data` payload |
| error object (a reply's `error`) | `code`, a human-readable `message`, plus `holder` on seat refusals |

Empty optionals are omitted, so a void method's reply is just `{"id":"7","seq":412}` and an
error reply carries `error` instead of `result`.

Rules that matter to an implementation:

- **Correlate by id only.** Your request ids are opaque strings you choose. Replies do not
  necessarily follow request order: mutating verbs serialize on one shared lane, and
  `interrupt` jumps ahead of everything queued there, so a later request can be answered
  first.
- **The greeting always comes first.** The server flushes it before dispatching any request,
  so the first line you read is `hello` or `refused`, however early you start writing.
- **Replies and events interleave.** One socket carries both, and their `seq` values come
  from the same number space, so interleaving order is meaningful (see Reconciliation).
- **A request line that fails to parse closes the connection**, as does EOF. Blank lines are
  ignored and line length is unbounded, since event payloads carry transcript entries.
- **Every reply carries a `seq`.** On a read it is the number the result was taken at, on a
  write the post-mutation watermark, and on an error the current watermark. One number
  space per session, dense and monotonic for its lifetime.
- **Delivery is fail-open.** Outbound lines stage behind bounded queues under a short write
  deadline, so a peer that stops reading loses its connection rather than stalling the
  session's turn loop. Only `session.exit` bypasses every bound, so an attached controller
  can always tell "session ended" from "connection lost".

### Protocol versioning

`hello.protocol` must equal the controller's expected version (currently 1). An
incompatible shape change bumps it. Additive fields may appear within one version (the
discovery record is additive-only by rule), so ignore unknown event names, method names
you are not offered, and payload fields rather than erroring on them.

### Error codes on replies

| code | meaning |
|---|---|
| `unknown-method` | no such method |
| `bad-request` | malformed or invalid params (empty submit line, empty rewind head, out-of-range dialog index, two answer shapes at once) |
| `unavailable` | valid now-refused call: a dialog holds input, serve mode has no editor, a turn owns the loop, the queue cannot take an item, no gate exists for `mode.set`, or the session is shutting down |
| `seat-held` | another controller owns the seat, with `Error.Holder` naming it |
| `observer` | write from a connection without the seat, with `Error.Holder` naming its holder when one holds it |
| `not-found` | unknown target (rewind head entry id, interaction id, model or settings key) |
| `settled` | a dialog already resolved, first answer won |
| `busy` | the prompt queue is full, retry once it drains |
| `unexpected` | the verb faulted mid-run; its effect may have partially applied, so reconcile from `state.get` before writing again |

A refusal is always a reply with `error`, never a transport failure. Errors carry a
human-readable `message` on top of the code. Scripts should branch on `code` and `holder`.

## Connection lifecycle

1. **Connect.** The first server line is `hello` or `refused`, and nothing else precedes it.
2. **Read the hello.** Its `seq` is the watermark your stream continues from: events above
   it are new to you. Check `protocol` against your own version before parsing anything else.
3. **Claim the seat if you intend to write** (`attach`). An observer stays read-only for the
   life of the connection, and any number of observers may coexist.
4. **Work.** Snapshot, backfill from the transcript file, stream events, call methods.
5. **End.** `session.exit` is always the last event before a socket closes, with a free-form
   `reason` that may be empty. Detaching (closing your socket) frees the seat you hold and
   never ends the served run. Submitting `/exit` does end it.

A `refused` connection reads one `refused` event (`seq` 0) naming what the credential check
saw, then closes. A slow or stalled reader is dropped mid-session (see the fail-open rule
above), which is a disconnect like any other: reconnect and resnapshot. There is no retained
event replay on the wire.

Hello payload:

| field | meaning |
|---|---|
| `protocol` | wire version, must match yours |
| `session.id`, `session.name` | transcript identity, name optional |
| `workspace`, `cwd` | where the session runs |
| `interactive` | a local terminal renders this session too |
| `serve` | the socket is the only front end |
| `pid`, `version` | serving process id and ajent version |
| `transcriptPath` | recorded transcript file, empty when recording is off |
| `token` | this instance's writer token, so a reconnect can prefer the same instance |

## Discovery

The registry file beside each socket is one JSON object per instance, with empty or false
fields omitted. It is written after listen and rewritten on seat claims, releases and renames,
so picking costs one directory read and never dials. Fields are additive only.

| field | meaning |
|---|---|
| `sessionID` | transcript id, the directory name beside the socket |
| `sessionName` | human name when the session has one |
| `workspace`, `model` | where it runs and the model key in effect at that write |
| `startedAt` | instance start time, unix milliseconds |
| `pid` | serving process id, the liveness proof |
| `token` | writer slot token, whose tail names this instance's two files |
| `socket` | absolute socket path to dial |
| `seatHeld` | whether some controller holds the control seat right now |

Liveness is judged without connecting: a record is collected only when it is at least a
minute old and signal 0 on its recorded pid proves no such process. A reused pid reads as
alive, which merely delays that one record's collection. Empty per-session directories go
with their last record. Because records are the only discovery mechanism, a picker never
rings a serving instance's accept log.

Target resolution (what `client.Resolve` implements) is worth matching in other
implementations: an existing socket path wins outright, then an exact session name
(case-insensitive), then a full id or any prefix of one. Several matches all return so the
caller picks among them rather than guessing. An empty target means every live instance.

## Methods

`state.get`, `models.get` and `settings.get` answer any connection. `attach` is callable
from any connection at any time and
is how a connection takes or keeps the seat. Every other method requires the control seat,
so an observer's write refuses with code `observer`. Mutating verbs serialize on one lane,
where guards revalidate at execution time rather than at request time and a reply follows the
effects it acknowledges. `interrupt` jumps that queue. `interaction.resolve` needs the seat
but runs off the lane, because its first resolution wins against local surfaces.

| method | params → result | seat | lane |
|---|---|---|---|
| `state.get` | none → `State` snapshot | no | no |
| `models.get` | none → `{active, models}` | no | no |
| `settings.get` | none → `{rows}` | no | no |
| `attach` | `{control?, holder?, force?}` → `{control}` | claims it | no |
| `submit` | `{line, pendingId?}` → void | yes | yes |
| `interrupt` | none → `{stopped?}` | yes | yes, priority |
| `mode.cycle` | `{back?}` → `{mode?}` | yes | yes |
| `mode.set` | `{mode}` → `{mode}` | yes | yes |
| `input.set` | `{text}` → void | yes | yes |
| `queue.recall` | none → `{id?, text?}` | yes | yes |
| `session.rewind` | `{head}` → `{head}` | yes | yes |
| `interaction.resolve` | `{id, index? \| indexes? \| text? \| cancel, chat?}` → void | yes | no |
| `seat.yield` | none → void | yes | no |
| `tools.set` | `{names}` → `{rows}` | yes | yes |
| `model.set` | `{model}` → `{model}` | yes | yes |
| `settings.set` | `{key, value, persist?}` → `{row}` | yes | yes |

Params and results with their wire keys:

- **`state.get`** → one snapshot object, documented in [Snapshots](#snapshots). The reply's
  `seq` is the snapshot's own number.
- **`models.get`** lists the model registry exactly as the running process resolved it
  (config plus discovery, no compiled entries), so a controller renders a picker without
  riding one. `{active}` names the model now in effect and each entry of `models` is the
  snapshot's `model` shape (`{key, short, window, caps}`). The registry this lists is the
  one `model.set` resolves against.
- **`settings.get`** returns the `/settings` rows, one per editable dotted key, in menu
  order: `{rows: [{label, key, value, source, editable, reason}]}`. `value` is the
  resolved JSON value and `source` the layer it came from (see
  [Vocabularies](#vocabularies)). `reason` says why an uneditable row cannot change right
  now. Theme has no config key and Reset overrides is an action, so neither appears. Rows
  answer any connection.
- **`attach`** `{control, holder, force}`. With `control` false or absent this stays an
  observer attach and answers `{control:false}`, which succeeds even while another controller
  holds the seat. With `control` true it claims the seat: `holder` names this controller in
  refusal messages (default `controller`, whitespace trimmed), re-attaching as the current
  holder just refreshes that name, and `force` takes the seat from a live holder. A held seat
  refuses with `seat-held` plus `Error.Holder`. A forced takeover publishes `seat.lost` to
  every connection, so the displaced holder downgrades without probing. `{control:true}`
  reports success.
- **`submit`** `{line, pendingId}` routes one line exactly as typed in the session's editor:
  prompt, `/command`, `!shell` or `!!`. Remote behavior therefore shares local dispatch and
  its tests. During a running turn the line steers or queues per the pump's rules, and a
  queued submit can come back with `queue.recall`. Submitting never delivers past a human
  gate: while a dialog holds the editor this refuses. Errors: `bad-request` on an empty or
  whitespace-only line, `unavailable` while a dialog holds input,
  `busy` when the prompt queue is full.
  `pendingId` is your own correlation for the pending row. Omit it and the session mints one,
  which you then learn only from `queued` events, so pass your own if you track submissions.
  It becomes the row's id in `queued` and `queue.dropped`, and rides the committed message
  entry as `queueIds`. Prompt lines only: slash and shell lines commit no message entry and
  surface through their effects instead. Several queued prompts may be joined into one user
  message at a step boundary, so one committed entry's `queueIds` can name several of your rows.
- **`interrupt`** runs the Esc body. It aborts a running turn (then an `/init` survey, then a
  staged shell command) in that priority order, covers the between-turns drain window, and
  clears queued prompts as `queue.dropped` with cause `interrupt`. `{stopped}` names what it
  stopped (`turn`, `survey`, `shell`) and is empty when nothing ran.
- **`mode.cycle`** `{back}` steps the permission mode like Shift+Tab, backwards when `back`,
  re-evaluating open approval dialogs under the new mode. `{mode}` names the mode now in
  effect and the same change announces as a `permission` event. An empty result means no gate
  exists at all. See [Vocabularies](#vocabularies) for the mode names and cycle order.
- **`input.set`** `{text}` prefills the host editor's buffer under local typing semantics, so
  text simply sits there during an active turn. Refuses `unavailable` in serve mode (no
  editor) or while a dialog holds input. Your own draft is not a wire concept: this writes the
  session's editor, which is what a keyboard-only driver wants and an attached terminal does
  not need.
- **`queue.recall`** pops the newest queued prompt for your own surface to edit, like Alt+Up.
  `{id,text}` names the row that left and its text, because the reply's watermark supersedes
  that same row's `queue.dropped` event (cause `recalled`). An empty result means nothing was
  queued. A refusal (an item that cannot cross the wire) leaves the item queued.
- **`session.rewind`** `{head}` forks onto an earlier transcript entry through the keyboard's
  rewind transaction: queued prompts clear with a recorded notice, the head moves, and
  `branch.changed` announces it. Head ids come from the transcript tree or prior backfill
  reads. Target rules follow what you name: a user-prompt entry rewinds to its parent so the
  prompt re-sends (an interactive host also pre-fills it locally), an assistant reply keeps
  itself in the restored branch, a `compaction` entry moves to its parent, and any other
  entry refuses with `unavailable`. An id absent from the file is `not-found`. Rewind also
  ends an active `/plan` workflow and refuses while a drain owns the loop (a running turn or
  the between-turns window), both with `unavailable`.
- **`interaction.resolve`** answers one open dialog by its `id`. Exactly one answer shape
  applies, validated against what `interaction.pending` announced: `index` selects from
  `options`, `indexes` when the dialog is `multi` (an empty list confirms no selection),
  `text` when text is accepted (`chat` marks an own-words reply where offered), `cancel` for
  Esc everywhere. Two shapes in one call are `bad-request`, never a precedence to resolve.
  An id of 0 is `bad-request`, out-of-range or wrong-shape answers are `bad-request`, a
  settled id is `settled`, and a never-seen id is `not-found`. Wrong-shape strictness matters
  because the permission barrier reads an unknown index as denial. Secret answers never echo
  back in any event.
- **`seat.yield`** releases the seat without dropping your socket, so another controller can
  take it while you keep watching. A no-op when you do not hold it.
- **`tools.set`** `{names}` reshapes the enabled tool set through the same apply `/tools`
  uses, widen-only by construction: before this run's first dispatched prompt `names` is a
  free replacement (names left out disable, an empty list disables everything). After it
  the call can only enable, and names left out keep their state, since the tool block the
  model has already seen is not retractable. Group names (like `subagents`) expand to
  their members and unknown names are ignored. The reshape records its
  `setting_change` entry, publishes the full-replacement `tools` event and, when the set
  actually changed, notes the prompt cache will miss. A set that changes nothing records
  nothing. The reply's `{rows}` is the post-apply tool rows, the same shape the `tools`
  event and the snapshot carry.
- **`model.set`** `{model}` switches onto one model resolved by key, alias, provider/id,
  bare id or a unique id suffix, exactly the names `models.get` lists. The switch is
  **session-only**: it records a `model_change` entry and a session override, never
  rewriting the user's config the way the `/model` command does. During a running turn the
  switch queues and lands at the turn boundary, announced by its `status.model` event. The
  reply names the model the switch resolved to. An unknown name is `not-found`, an
  ambiguous one `bad-request` listing its candidates.
- **`mode.set`** `{mode}` sets the permission mode by name rather than stepping it, through
  the same hook the `/settings` row drives: the barrier flips live, open approval dialogs
  re-evaluate under the new mode, the `permission` event publishes and no `setting_change`
  entry records, so the mode stays run-scoped. Mode names are the vocabulary `mode.cycle`
  uses. An unknown name is `bad-request`. A session with no gate at all (tools disabled)
  refuses `unavailable`.
- **`settings.set`** `{key, value, persist}` applies one setting from the `settings.get`
  rows as a session override with its live effects, validated server-side against the
  row's shape and bounds (a bad value is `bad-request`, an unknown key `not-found`, an
  uneditable row `unavailable` naming its reason). `value` carries the JSON value in the
  row's own shape: booleans, numbers, strings, or a list of tool names for `tools.enabled`
  (which applies through the same widen-only rule as `tools.set`). Model and reasoning
  rows apply their live effects too, so a switch or level change never records an override
  that does not take effect. Every change records its `setting_change` entry except
  `permissions.mode`, which never does. `persist`, when set, also copies the value to a
  config file layer (`user` or `project`) exactly as the `/settings` save prompt would.
  The reply carries the row after the change.

## Events

Two families share the `seq` space:

- **Committed**: `entry.append` and `branch.changed`. Everything scrollback-visible renders
  from these plus transcript backfill, reconciled by entry id alone. No event outside this
  family may move a head. A prompt renders when its entry commits, never at submit.
- **Ephemeral**: everything else, governed by the watermark rule only and never reconciled
  against the file. The one exception is open dialogs: they are listed in every snapshot too,
  because they hold the run.

Every payload below is the `data` object of that event. Names are exact wire keys, and an
omitted field means absent or false.

| event | data | notes |
|---|---|---|
| `hello` | hello fields above | first line, `seq` is your starting watermark |
| `refused` | `{reason}` | instead of hello, `seq` 0, then close |
| `session.exit` | `{reason?}` | last event before close, cannot be dropped |
| `seat.lost` | `{from?, to}` | a force takeover announcing itself, never on release-by-disconnect |
| `turn.start` | `{model}` | active model key |
| `thinking.delta`, `text.delta` | `{delta}` | one streamed piece |
| `thinking.end`, `text.end` | none | closes that stream |
| `tool.start` | `{id, name, input?, label?, full?}` | opens one call, keyed by id |
| `tool.output` | `{id, delta}` | live output of that call |
| `tool.progress` | `{id, name?, path?, bytes?, lines?, done?}` | arguments still streaming |
| `tool.result` | `{id, name, output?, error?}` | closes the call |
| `diff` | `{path, before, after}` | one rendered file change |
| `context` | `{used, window, reserve, compact, estimated}` | how full the next request is |
| `notice` | `{text, level, key?}` | transient line |
| `turn.end` | `{stop, steps, usage, error?}` | closes the turn with its own usage |
| `print` | `{markdown}` | one rendered block, the form `/help` and `/usage` use |
| `entry.append` | `{entry: {id, parentId?, type, ts, data?}}` | committed transcript line, note the nesting |
| `branch.changed` | `{from?, head, cause?}` | the head moved, causes below |
| `status` | exactly one of `segment`, `model`, `spinning` | status-row family |
| `activity` | `{key, text, rank}` | one keyed activity row |
| `permission` | `{mode}` | mode now in effect |
| `tools` | `{rows: [{name, source, enabled, readOnly}]}` | replaces the toggleable `/tools` rows |
| `idle` | `{idle}` | the prompt's rest state, what local keys mean |
| `queued` | `{rows: [{id, text}]}` | replaces pending-prompt rows wholesale, oldest first |
| `queue.dropped` | `{rows: [{id, text}], cause?}` | names rows a removal cleared |
| `session.renamed` | `{name}` | rename, the committed truth rides its own entry |
| `interaction.pending` | dialog shape below | one open dialog and how to answer it |
| `interaction.settled` | `{id, kind, answered, cancelled, index?, indexes?, text?, chat?}` | closes one dialog with its outcome |

Rendering rules a controller needs:

- **Keyed families replace, empty text removes.** A `status` segment and an `activity` row
  carry a key. Setting non-empty text adds or replaces that row, and empty text reports its
  removal. An unkeyed `notice` prints, while a keyed one (`key` set) replaces in place instead
  of stacking. The same applies to `tool.*`, keyed by call id: a header commits once its result
  or first output arrives.
- **Tolerate events for state you never saw.** `tool.progress` reports arguments that are still
  streaming, so it can name a call before its `tool.start`, and an attach window can lose the
  start entirely: render the header from the `tool.result`. Turn boundaries are likewise
  idempotent, since `turn.end` after a lost `turn.start` is fine.
- **Do not paint a tool result twice.** `tool.result.output` carries the final body in this
  order: what history shows when it differs, then streamed output nobody rendered, then the
  model-facing content. A surface that already painted that call's `tool.output` deltas should
  ignore `output`, since the deltas committed at their own end.
- **Queue rows keep ids even when redacted.** Secret-prefixed prompts arrive with an id and an
  empty text, in `queued`, in `queue.dropped` and in snapshots. Some drops are id-only by design
  (a run reported dropped before it landed), so resolve rows by id rather than by text. Causes
  are `interrupt`, `rewind` and `recalled`.
- **`status.spinning` needs no live paint.** Turn events carry that fact, and only a snapshot
  reconciles it, because a drop can lose a turn's end event. Reconcile a spinner from
  `spinning` on resnapshot.
- **Dialogs are announced, then closed.** Every open dialog appears as `interaction.pending`
  with the shape it accepts (`kind` is a display hint only), and every one ends as
  `interaction.settled`, so no client holds a dialog that never resolves. Free-text answers
  never echo in either event, and a secret answer's text never appears anywhere.

## Snapshots

`state.get` returns one internally consistent read of everything a controller polls. The
reply's `seq` is the snapshot's own number: head and view-model fields were taken under it.

| field | shape | change event |
|---|---|---|
| `session` | `{id, name?}` | poll-only (`name` also announces as `session.renamed`) |
| `workspace`, `cwd` | strings | poll-only |
| `head` | transcript head entry id, the one to branch a file read by | committed family only |
| `model` | `{key, short, window, caps{reasoning, images, parallelToolCalls}}` | `status.model` |
| `permission` | mode name, empty when no gate exists | `permission` |
| `tools` | rows of `{name, source, enabled, readOnly}` | `tools` (full replacement) |
| `usage` | `{turns, total, child, byModel, estimatedTurns}`, all usage objects below | poll-only |
| `context` | `{used, window, reserve, compact, estimated}` | `context` |
| `running` | a turn is in flight | derived from `turn.start` / `turn.end` |
| `idle` | the prompt's rest state | `idle` |
| `spinning` | working indicator | reconciled here only |
| `queued` | count of pending prompts | follows `queued` rows |
| `queuedRows` | `[{id, text}]`, oldest first, secrets redacted to empty text | `queued` (full replacement) |
| `activity` | `[{key, text, rank}]` | `activity` per row |
| `status.segments` | `[{key, text, short, order}]` | `status.segment` per key |
| `seat` | `{held, holder?, yours}` reported from your connection's side | none, see below |
| `interactions` | open dialogs, oldest first, each exactly the pending shape below | `interaction.pending` / `.settled` |

Poll-only facts have no change event by design: session identity, workspace, cwd and the
usage totals, whose ledger accumulates silently because per-turn usage rides `turn.end`.
Refresh those by resnapshotting. Seat release, whether by disconnect or `seat.yield`, spends
no event number. It rewrites the discovery record instead, so a reconnecting or newly attached
controller reads seat state from the next snapshot rather than waiting for an event.

Two fields may describe a moment slightly newer than the reply's `seq`: the open dialog set and
the usage totals are read immediately after the hub's critical section rather than inside it.
That direction is safe, since newer state already supersedes events at or below the watermark,
and an above-number event that re-describes it is idempotent by entry id, dialog id or full
replacement.

Dialog shape (`interaction.pending` and one `interactions` element):

| field | meaning |
|---|---|
| `id` | broker-assigned dialog id, answer with `interaction.resolve` |
| `kind` | display hint: see [Vocabularies](#vocabularies) |
| `prompt`, `subject` | what it asks and what it is about |
| `options` | labels an `index` or `indexes` answer selects from, absent when neither applies |
| `multi` | an `indexes` answer selects many at once |
| `text` | a `text` answer is accepted |
| `secret` | the text answer is a secret and never echoed |
| `chat` | a `text` answer may be marked as an own-words reply (`chat: true`) |

Dialog ids are unique to one open dialog in one process run. Do not carry them across a
reconnect: whatever your snapshot no longer lists has settled, so cancel it locally too.

Usage object: `{input, output, cacheRead, cacheWrite, reasoning}`. `turn.end.usage` covers
that turn alone. The snapshot's totals accumulate for the session, with `child` covering
sub-agent spend and `byModel` keyed by model key.

## Vocabularies

Strings a controller switches on. Unknown values should render generically rather than fail,
since additions are additive changes.

- **Permission modes** (`permission`, `mode.cycle`, `mode.set`): `allow-read`, `auto`,
  `auto+write`, `allow-all`, `block-all`. Cycle order follows that list and wraps, `back: true` walks it in
  reverse. Empty means no gate exists on this session.
- **Setting sources** (`settings.get`): `default`, `user`, `project`, `local`, `session`, plus
  `active` for the model row when no layer sets it (the run still sits on the model it
  started with).
- **Dialog kinds** (`kind`): `pick`, `multipick`, `select`, `confirm`, `input`, `secret`,
  `ask`, `decision`. The answer shape to trust comes from the flags, not the kind: `options`
  for `index`, `multi` for `indexes`, `text` for a text answer.
- **Notice levels** (`notice.level`): `info`, `warn`, `error`.
- **Stop reasons** (`turn.end.stop`): `end_turn`, `tool_use`, `max_tokens`, `incomplete`,
  `aborted`, `error`, and `unknown` when nothing better is known. `turn.end.error` is set on a
  transport or context failure, which is separate from a tool's own error result.
- **What an interrupt stopped** (`interrupt` result): `turn`, `survey`, `shell`, empty for nothing.
- **Queue drop causes**: `interrupt`, `rewind`, `recalled`.
- **Head-move causes** (`branch.changed.cause`): `rewind`, `fork`, `compact`. No other event may
  move a head, and only these plus appended entries change what history means.
- **Message roles** (transcript blocks below): `system`, `user`, `assistant`, `tool`.

## Reconciliation

One rule covers live events against reads: events at or below a reply's `seq` are superseded by
that result, events above it apply on top. Committed entries additionally dedupe by entry id, so
an entry arriving both as a live event and through backfill renders once.

- **Reconnect is always a resnapshot.** There is no retained-window replay. Reconnect means
  `state.get` plus another file read, rendering only the difference against what you already
  rendered. The cut goes both ways: also clear what the new snapshot no longer lists (segments,
  activity rows and queue rows removed while you were away, a spinner whose turn ended without
  its end event surviving the drop), or stale chrome outlives the resnapshot. Open dialogs are
  part of that difference, so a dialog spanning your gap gets re-raced locally instead of
  stranding the run on a dialog nobody can see.
- **Open dialogs are state.** Every snapshot lists them exactly as their pending events announced
  them, which is what lets a controller attaching mid-dialog answer by id. They ride the socket
  only: never the transcript, never the discovery record.
- **A prompt's journey resolves by ids.** A submit paints an ephemeral pending row before any
  reply, and the committed entry replaces it on success while a drop, recall or failed submit
  resolves it inline. Only prompts paint one. Follow your `pendingId` through `queued`,
  `queue.dropped` and the committed entry's `queueIds`.

## Rebuilding session data from the transcript file

There is deliberately no transcript method on the socket: bulk history rides the file at local
speed with no size bound crossing the wire. The hello's `transcriptPath` names it, and every
snapshot carries `head`, the entry id to branch by. Only appends after your snapshot announce as
events.

The backfill sequence (`client.Backfill` does all of this):

1. **Snapshot**: `state.get`, recording the reply's `seq` and its `head`.
2. **Read the file**: parse every complete line, one entry per line, with `session.Read`. The
   transcript is append-only JSONL, so reading while the host appends is safe: a torn final line
   (no trailing newline) is skipped silently, an unparseable middle line becomes a warning and
   never makes the file unreadable, and only a newer format version is a hard error
   (`ErrNewerFormat`, returned together with the parsed entries). Whole lines are fenced by an
   exclusive lock, so concurrent processes interleave lines rather than bytes.
3. **Branch to the head**: select the chain with `session.Branch(entries, head)`, walking parent
   ids from the snapshot's head back to root and returning oldest first, cycle-guarded. File
   order is never a read path, because branches interleave in it. Keep the whole parsed set if
   you want to offer every fork, the way the rewind picker does.
4. **Apply live events**: events past the snapshot's `seq` apply on top, deduping `entry.append`
   against step 2 by entry id, since an append landing between the snapshot and your read may
   arrive both ways.

One entry is one line:

```json
{"id":"01J...","parentId":"01J...","type":"message","ts":1730000000000,"data":{...}}
```

`id` is a Crockford ULID, so ids sort lexically by creation time and `ts` is unix milliseconds.
`parentId` links the entry into the session tree (empty means root), which is what makes
branching, rewind history and abandoned forks reconstructible. Types and payloads:

| type | payload | role |
|---|---|---|
| `session` | `{version, cwd?, workspace?, model?, branch?, commit?, name?}` | first entry of every file, format version 1 today |
| `message` | `MessageData` below | committed message with stop reason and usage |
| `compaction` | `{summary?, firstKeptEntryId?, before?, after?, reduce?, details?}` | context-reduction checkpoint, not replayed to the UI (see `compaction-design.md`) |
| `model_change` | `{model, reason?}` | model switch by canonical key |
| `setting_change` | `{key, value}` | one setting change by config dotted path, where a JSON null `value` marks a reset |
| `session_name` | `{name}` | rename after creation, newest wins |
| `notice` | `{message, level?}` | user-visible notice worth replaying. Note the key is `message` here where the live event uses `text` |
| `custom` | `{customType, data?}` | opaque extension state that survives resume |

Unknown types and unknown payload fields round-trip untouched, so handle the familiar and ignore
the rest rather than failing on a newer build's file.

`MessageData`:

| field | meaning |
|---|---|
| `message` | `{role, content}` with the block envelope below |
| `stop` | stop reason name, assistant messages only |
| `usage` | that message's usage object |
| `injected` | system-provided user text, excluded from prompt recall |
| `replayed` | injected context that still belongs in restored scrollback |
| `origin` | submitter of a non-typed prompt, `controller` for control submits |
| `queueIds` | the pending-row correlations this message landed for, possibly several when queued prompts were joined |

Content blocks are a tagged list: each element is `{"type": ..., "data": {...}}`. Types are
`text` (`{text, signature?}`), `thinking` (provider replay tokens included, so do not drop it),
`tool_call` (`{id, name, input}`), `tool_result` (`{callId, toolName?, content[], display?, isError?, ...}`,
whose `content` is itself a block list) and `image` (`{mediaType, data}` with base64 bytes). A
user message carrying only tool results is plumbing rather than a prompt, which is why one shared
classification rule exists.

To render committed history in Go, `session.Replay(branch, sink, opts)` projects a branch onto an
agent sink exactly as the live turn loop would paint it: user prompts open turns and echo above
their replies, tool results fold under their calls' headers, notices replay, and each turn closes
with its stop reason and accumulated usage (thinking suppressed unless requested). For single
entries, `session.EchoEntry` paints one committed entry's standalone shape. Both share one
classification rule for user messages (typed prompt vs tool-result carrier vs injected context),
so a live append renders exactly what replay would render for the same entry: controllers need no
second rendering path.

After attaching, only appends past your snapshot announce as events. When `branch.changed`
moves the head, later reads branch from the new head. A reconnecting controller repeats the
whole sequence above.

## The Go client (`pkg/client`)

The in-tree reference consumer, and what `ajent --attach` itself drives. It imports only `ctl`,
`paths` and `session`, so a controller gets dial, discovery, typed calls, the event stream and
transcript backfill with no protocol knowledge of its own: a refusal surfaces as a coded wire
error (`*ctl.Error`) rather than a transport failure.

### Dialing and discovery

| symbol | contract |
|---|---|
| `Dir() (string, error)` | the control directory under the ajent home, without creating it |
| `Discover() ([]Instance, error)` | every live instance's registry record, sorted by session id. A corrupt or truncated file reads as absent, and nothing here dials |
| `DiscoverLive() ([]Instance, error)` | Discover without the dead records: every instance whose recorded pid still answers signal zero past its first minute. It never dials and never collects; removing a dead record stays the serving instances' passive sweep |
| `InstanceDead(in Instance) bool` | the record-only liveness test: a record at least one minute old whose recorded pid fails signal zero. Anything undecidable (no usable pid, a permission error, a reused pid) reads as alive, so a caller only drops a record on proof of death |
| `Resolve(target string) (matches, live []Instance, err error)` | Discovery's target-resolution order over the registry. An empty target returns every instance in both slices, a socket-path target wins outright as one record with only `Socket` filled, and a miss is an error naming the target while the second slice carries the full live set for a picker |
| `Followable(live []Instance, sessionID, token string) []Instance` | what a reconnecting controller follows: the same token when it still serves, else a sole surviving instance of that session, else nil so several candidates never become a silent swap onto a sibling's head |
| `SavedSessions() ([]session.Listed, error)` | every saved session under the ajent home across all workspaces through ajent's own store (`session.Store.ListAll`), most recently used first. Each row carries the workspace its transcript recorded for resume targets to derive from; a newer-format transcript still lists marked `Blocked`, one without decodable identity never lists, and opening the store creates the sessions root when missing |
| `Instance` | alias of `ctl.Instance`, with `Label()` (name or id), `Started()` and `String()` for listing rows |

### Connection lifecycle

| symbol | contract |
|---|---|
| `Dial(path string) (*Conn, ctl.Hello, error)` | connect and read the hello under a 2 s deadline, refusing an incompatible protocol version and turning a `refused` event into an error naming its reason. The connection starts silent |
| `(*Conn) Serve(handle func(ctl.Event))` | decode lines until the peer goes away or one turns to garbage: start it in its own goroutine right after `Dial`, before the first request flies, since nothing completes without it. Events arrive in order on that goroutine, and so do your replies, so a handler that blocks stalls your own calls and risks the server's stall rule |
| `(*Conn) Call(ctx, method string, params, out any) (uint64, error)` | send one request and wait for its reply, returning the reply's watermark. Pass a `nil` params or `out` for void calls. A refusal returns `*ctl.Error`, decodable with `errors.As` for `Code` and `Holder`. A transport failure or expired context errors with no seq |
| `(*Conn) Hello() ctl.Hello` | the greeting read at connect |
| `(*Conn) Done() <-chan struct{}` | closed when `Serve` returned, the signal to start reconnecting |
| `(*Conn) Err() error` | the read failure that ended the stream, nil only while `Serve` is still running or after a refused protocol version. A clean peer close reads as `io.EOF`, so decide "session ended" versus "connection lost" from whether you saw `session.exit`, not from this error |
| `(*Conn) Close()` | tear the connection down: `Serve` returns and waiters wake. This frees any seat you hold |
| `CallWait` | 3 s request budget for a caller with none of its own (`context.WithTimeout(context.Background(), client.CallWait)`), which is what every in-tree call uses |

### Typed calls

Each wrapper covers one method, so nothing here needs a protocol constant. Every one takes a
context and surfaces wire refusals unchanged.

| symbol | wraps | returns |
|---|---|---|
| `ClaimSeat(ctx, holder string, force bool) (bool, error)` | `attach` with control | whether this connection holds the seat |
| `State(ctx) (ctl.State, uint64, error)` | `state.get` | snapshot and its watermark |
| `Backfill(ctx) (ctl.State, []session.Entry, uint64, error)` | `state.get` plus the file read | snapshot, branch through its head, watermark. Errors when recording is off |
| `Submit(ctx, ctl.SubmitParams) error` | `submit` | sets `Line` and optionally `PendingID` |
| `Interrupt(ctx) (string, error)` | `interrupt` | what stopped: `turn`, `survey`, `shell`, empty for nothing |
| `ModeCycle(ctx, back bool) (string, error)` | `mode.cycle` | mode now in effect, empty when no gate exists |
| `ModeSet(ctx, mode string) (string, error)` | `mode.set` | mode now in effect, and a session without a gate refuses |
| `ToolsSet(ctx, names []string) (ctl.ToolsSetResult, error)` | `tools.set` | the tool rows after the reshape (widen-only past the first dispatched prompt) |
| `Models(ctx) (ctl.ModelsData, error)` | `models.get` | the registry listing with its active key |
| `ModelSet(ctx, model string) (ctl.ModelSetResult, error)` | `model.set` | the model the session-only switch resolved to |
| `Settings(ctx) (ctl.SettingsData, error)` | `settings.get` | the `/settings` rows with values, sources and editability |
| `SettingsSet(ctx, ctl.SettingsSetParams) (ctl.SettingsSetResult, error)` | `settings.set` | the row after the applied change, with `Persist` optionally copying to `user`/`project` |
| `InputSet(ctx, text string) error` | `input.set` | prefills the served run's editor buffer under local typing semantics; serve mode refuses with `unavailable` |
| `Rewind(ctx, head string) (string, error)` | `session.rewind` | the new head id |
| `QueueRecall(ctx) (ctl.RecallResult, error)` | `queue.recall` | the popped row whole: its `ID` names the queue entry that left, so a controller tracking pending rows resolves them by id. Zero result when nothing was queued |
| `Resolve(ctx, ctl.ResolveParams) error` | `interaction.resolve` | pass exactly one answer shape with the dialog `ID` |
| `YieldSeat(ctx) error` | `seat.yield` | releases the seat without dropping the socket; a no-op when this connection holds none |
| `ReadBranch(path, head string) ([]session.Entry, error)` | package-level | read one transcript file and return its chain under `head`, oldest first, tolerating a torn final line |

Every method in the table above has a typed wrapper. `Call` remains the escape hatch for any
method added later.

### A controller in Go, end to end

```go
insts, _, err := client.Resolve("my-session") // discovery by name, id prefix or socket path
if err != nil {
	return err
}
conn, _, err := client.Dial(insts[0].Socket) // protocol checked here
defer conn.Close()

events := make(chan ctl.Event, 256)
go conn.Serve(func(ev ctl.Event) { events <- ev }) // start before any Call, never block in it

cctx, ccancel := context.WithTimeout(context.Background(), client.CallWait)
defer ccancel()
if _, err := conn.ClaimSeat(cctx, "my-controller", false); err != nil {
	var we *ctl.Error
	if errors.As(err, &we) && we.Code == ctl.CodeSeatHeld {
		return fmt.Errorf("seat held by %s", we.Holder) // or retry with force: true
	}
	return err
}

snap, branch, water, err := conn.Backfill(cctx) // snapshot plus the file read through its head
render(branch) // committed history, or session.Replay to paint it as a turn stream

wm := water // everything at or below one reply's seq is superseded by that result
for {
	select {
	case <-conn.Done(): // Serve returned: after session.exit this is the end, else reconnect
		return conn.Err()
	case ev := <-events:
		if ev.Seq <= wm {
			continue // the snapshot already covers it
		}
		wm = ev.Seq
		if ev.Name == ctl.EventSessionExit {
			return nil
		}
		apply(ev) // dedupe entry.append by id, replace keyed families wholesale
	}
}
```

For dialogs, keep a map from `id` to shape built from `interaction.pending` and the snapshot's
`interactions`, delete on `interaction.settled`, and clear anything a resnapshot no longer lists.

## Writing a controller in another language

The minimum honest implementation:

1. Read `<home>/control/*/<token8>.json` for candidates, or dial one known socket path. Pick by
   name, id prefix or a user's choice. Judge liveness from the recorded pid, never a probe.
2. Connect over unix and read the first line. Require `protocol` to match (1 today), accept
   `refused` as a fatal permission error, and remember hello's `seq`, `transcriptPath` and `token`.
3. Send `attach` with `control: true` if you intend to write. On `seat-held` decide between
   re-attaching with `force`, waiting for `seat.lost` or exit.
4. Call `state.get`, remember its `seq` as your watermark, then read the transcript file and
   branch it by the snapshot's `head`. Render committed entries deduped by entry id.
5. Run one event pump that drops any event whose `seq` is at or below a watermark you have already
   applied, replaces keyed state for ephemeral families, and moves your head only on
   `entry.append` or `branch.changed`.
6. Answer dialogs from the pending set by id with exactly one shape, treating an unknown dialog as
   gone rather than retrying.
7. Handle teardown honestly: `session.exit` means the run ended, any other close means a drop, and
   a drop is recovered by reconnect plus resnapshot (`Followable`'s rule is the fair one), never by
   replaying a gap you cannot see.
