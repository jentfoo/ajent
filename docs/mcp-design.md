# MCP client and server registration

How `pkg/mcp` turns servers declared in `mcp.json` into ordinary agent tools,
why the protocol layer is delegated to `github.com/mark3labs/mcp-go`, and the
invariants that keep MCP from leaking past its boundary. It is the substrate the
extension protocol rides on, since an ajent extension *is* a registered MCP
server.

## What it is

`pkg/mcp` connects to stdio and network (Streamable HTTP, legacy SSE) MCP
servers, bridges each exposed tool into `agent.Tool`, and supervises every
configured server's lifecycle. It delivers three things the rest of ajent
consumes as plain facts:

- **Configuration** — merged from `~/.ajent/mcp.json` then
  `<workspace>/.ajent/mcp.json`.
- **Tools** — remote tools appear in the ordinary registry, namespaced to avoid
  collisions, subject to per-server filtering and enable state. first message;
  re-discovery on `tools/list_changed`, disconnect/reload through `/mcp`.
## Boundary rules

The dependency edge is load-bearing. The protocol layer (transports, JSON-RPC,
version negotiation, OAuth) is delegated to mcp-go; what ajent owns is the
boundary.

- **`pkg/mcp ↛ pkg/tools`, `pkg/tui`, `pkg/command`, `pkg/refs`.** It imports
  only `agent`, `config`, `llm` and mcp-go. Everything it needs from the
  registry or front / `console.go`. This keeps the dependency isolated so the
  library can be replaced
- **mcp-go wire types never escape `pkg/mcp`.** The bridge emits our own
  `agent.Tool`; discovery returns our own `ToolDef`/`Resource`/`PromptDef`. The
  extension host sees a
- **A tool of unknown effect does not run in an unobserved agent.** Read-only
  marking (from `annotations.readOnlyHint` OR config globs) defaults to *not*
  read-only; only a registry metadata, and the sub-agent tool set reads it: a
  marked MCP tool joins a read-only tools too.
## Configuration (`config.go`)

A server's config carries its stdio command plus args and env overrides, or a
network base URL with headers and an optional transport selection (`http`
default, legacy `sse`); enable state (nil means enabled), allow/deny tool globs,
exact exclusions from registration entirely, extra tools marked safe for
sub-agents, and a per-call timeout.

`LoadConfig(workspace)` reads user then project files and merges
**by server name with whole-entry replacement**. A project entry replaces the
user's for that server in full, never field-by-field. It deliberately does not
reuse `config.Merge`, whose deep fold would merge entries key by key against
this spec.

- The read path follows `pkg/llm/config.go`'s `LoadFile`: `RelaxJSON` →
  unmarshal → unknown-key and duplicate-key warnings returned as strings for the
  caller to surface. unset variable is a **clear error naming it**, never an
  empty header or value. `transport: sse` on a stdio server. There are no
  startup modes: every configured

## Client wrapper (`client.go`)

A `Client` is obtained by connecting to a named server; it lists the remote
tools, calls one with raw JSON arguments and an output writer, pings, and
closes.

- **stdio** — runs the server as a child process in its own process group, the
  child env built from the parent plus config overrides. `Close` tears down
  mcp-go's client not outlive a server torn down mid-session.
- `Initialize` errors wrap the library's version-mismatch error naming both
  versions, rather than failing obscurely. mcp-go 1.0 probes `server/discover`
  first and falls

### Schema fidelity

`mcp.ToolInputSchema` is lossy, keeping only a subset of keywords on round-trip.
So `Tools()` issues raw `tools/list` through the transport and decodes each
entry's `inputSchema` into a `json.RawMessage`, preserving the server's schema
**byte for byte** (proof that the raw request seam works). Pagination follows
`nextCursor`. Names are validated as composed: server key, bare tool name and
the composed `server__tool` string all fit the tightest provider tool-name
charset and cap (64; Anthropic allows 128). A bad server key is skipped with
a warning at config load, so one non-conforming entry never disables the
other servers.
A tool without a usable name or whose schema is structurally invalid is
dropped with one warning naming it rather than failing the whole list, a
server listing one tool twice yields a single registration, and a repeated
warning for the same defect on reconnect or `list_changed` stays in `/mcp`
logs instead of re-entering history (a config edit resets the dedupe, since
the owner may have fixed or re-broken the tool). Validation happens at
discovery, the single point where untrusted schemas enter: one broken schema
would otherwise fail serialization on every request carrying it, far from its
cause, so the tool is rejected there and the rest of the server's surface
stays usable. The rule is deliberately best-effort, not a JSON-Schema
interpreter: a single bounded walk over every schema-valued keyword,
rejecting only what providers police on the wire. That is a non-object schema
position, an unknown type name, a property key outside the providers'
parameter-key pattern `^[a-zA-Z0-9_.-]{1,64}$` (dots legal, unlike tool
names), or a non-array `required`/combinator. Everything else passes through
byte-identical: untyped or boolean-schema nodes, tuple `items`, empty arrays,
explicit nulls, combinators, `$ref` (never resolved, and `$defs` bodies
unchecked since nothing reads them), draft-03 boolean `required`, and unknown
keywords. Only remote schemas travel untrusted, and shrinking a working
tool's surface is worse than a tolerable schema.

### Raw seams

`Request(ctx, method, params)` sends any JSON-RPC request and returns the raw
result. `Handle(method, h)` installs a handler for an incoming server→client
method via the transport's `BidirectionalInterface`, replacing mcp-go's handlers
after `Start` and re-implementing `ping` itself (we set no sampling/elicitation
handlers, so nothing is lost); handlers accumulate, so a second method never
drops the first. Raw sends are bounded per attempt (`rawAttemptTimeout`) and the
idempotent list calls resend on transport failures; a dropped stdio line must
not fail discovery, but an unresponsive server still surfaces as an error, never
a hang. The raw seam sits below mcp-go's own request stamping, so it must carry
the era itself: on a connection negotiated to protocol 2026-07-28 every request
needs per-request `_meta` and mirrored `Mcp-*` headers (added by `applyEra`;
legacy connections stay unstamped, matching the pre-1.0 wire), and `Ping` no-ops
there since the RPC was removed.

Raw request ids are seeded at `rawSeqBase` rather than from one: both the raw
seam and mcp-go's typed calls share the transport's single response map keyed by
request id, so counting up from the same origin would collide mid-session and
strand a waiter until its deadline. The offset keeps the two id spaces disjoint.

### Result mapping (`result.go`)

Text content becomes a text block; image and audio become short placeholders
(naming the media kind) since image processing is separate work; embedded
resources become text references (uri + mime type) and resource links become
text references (uri + description); structured content with empty `content`
falls back to its raw JSON. `isError` maps onto `Result.IsError`.

## Bridging into the registry (`bridge.go`)

A remote tool definition and its owning client are turned into an ordinary
`agent.Tool` for one server.

The isolation seam: a remote tool becomes an ordinary `agent.Tool`, so `/tools`,
permissions, token accounting and the sub-agent treat it like any built-in.

- **Namespacing** — `Name()` namespaces the tool from the server name; this is
  what the model sees and what appears in the transcript, so stability matters.
  `Label()` shows the bare tool
- **Mode** — serial unless read-only, which may run parallel with other reads.
- **Timeout** — per-call cap from config, clamped to a max, mirroring `bash.go`.
  plus a notice so the turn continues; it does not abort. The model adapts.
  notification synchronously on its single stdout-reader goroutine, so any
  handler that response the same blocked reader can never return, deadlocking
  the whole server. goroutine at this boundary, so no current or future ajent
  handler can ever stall mcp-go. stream, so a call's progress shows up where its
  output does. bridged call cannot flood the model. See `tools-design.md`.

## Server manager (`manager.go`)

One supervisor owns every configured server's lifecycle; `pkg/app` passes a
registry adapter and notice/status callbacks. The adapter registers a source's
tool under an explicit state, unregisters a whole source, lists a source's
enabled, disabled or all names (so live enable state survives re-registration),
and marks extra tools read-only.

The local `State` enum mirrors the registry's (Disabled / Enabled) so this
package stays free of `pkg/tools`.

- **First-message load (`LoadOnFirstMessage`)** — there is no startup spawn.
  Every server (including config-disabled ones) is connected in full, exactly
  once, just still connects so its tools stay visible and toggleable in
  `/tools`, but `register()` Loading here rather than at session start means any
  `/tools` or `/mcp` change made up first message. Discovery during a connect is
  bounded by `discoverTimeout`, so an load or `/mcp` reload that awaits it.
  prompt, a pre-first-prompt `LoadOnFirstMessage` would otherwise leave MCP
  tools out of therefore also triggers the (idempotent) load when either command
  is dispatched, so
- **Resume ordering invariant.** The persisted enabled set is applied before MCP
  has registered anything, so those names would be dropped. `Options.Restore`
  (the session's default; the restored subset stays on and the rest are off.
  `list_changed`, the manager captures a source's full enable/disable split so a
  refresh restores exactly what was exposed, including tools the user turned
- **`tools/list_changed`** triggers re-discovery: unregister source, register
  fresh, preserving live enable state. It runs through `rediscan`, which
  serializes per server.
the current tool set anyway. Each pass is bounded with its own timeout so an
unresponsive server surfaces an error rather than leaking a goroutine or
hanging. Resources/prompts changes trigger best-effort capability refresh on
reconnects; both paths are safe to do blocking I/O because notifications arrive
asynchronously from the client (see
*Notifications never block mcp-go's reader*).
- **Disconnect / Reload.** `/mcp disconnect` closes and unregisters without
  removing the config. `Reload` re-reads `mcp.json`, disconnects removed servers
  and connects newly two. Filter fields (`tools.allow/deny`, `excludeTools`,
  `readOnly`, `enabled`, `tools/list_changed` — preserving the live enabled set,
  leaving the process running — Connection fields (`command`, `args`, `env`,
  `url`, `headers`, `transport`) are stored effect on the server's
  **next connect**, whichever comes first — `/mcp disconnect` + re-reads the
  stored config so a death silently adopts the new endpoint. A disconnected

**Lock ownership.** `server.mu` guards every mutable per-server field (client,
failure counters, discovered defs/resources/prompts, config); `Manager.mu` only
the `servers` map and first-load flag — one field, one lock. The notice sink is
immutable: built with the server rather than installed on connect, so it needs
no lock. An unreachable server is expected (offline or not yet started), so a
dial failure stays in `/mcp logs` only rather than surfacing as a notice; the
status ratio still reflects it.

**Single-flight connect.** A server's connection is coalesced: at most one dial
runs for a server at a time, and any path that wants it while another is in
flight shares that after instead of starting its own. Without this the reconnect
backoff, `/mcp reload` eager-connect and a manual `/mcp connect` can all target
the same dead server at once and each spawn its own client, leaking every
loser's stdio process and watcher goroutine (see *Reconnection*). A dial in
flight when `Reload` removes the server closes its fresh client rather than
installing into a stale object.

Network servers have no death supervision: `watchServer` only supervises a stdio
child's stderr, so a dead HTTP or SSE server is noticed on the next call rather
than proactively.

### Reconnection

A server that dies mid-session must produce a clear error result on any call
into it, never a hang, and be reconnected with capped backoff after repeated
failures, at which point its tools are unregistered so the model does not call
into nothing. A failure counter tracks consecutive connect failures for the
status path.

A stdio child's stderr is streamed to `/mcp logs` one line per entry
(`bufio.Reader`, no fixed cap), so a long or newline-less line is never dropped
— only an actual EOF or read error marks the child as exited and triggers
reconnection.


## Registry integration (`pkg/tools/registry.go`)

MCP mutates the registry from notification goroutines while the loop reads it,
so every method takes a lock. The single enabled bool became two states,
known-but-disabled (enabled in the prompt and callable). There is no deferred
state: every configured server is connected in full on first-message load, so
each bridged tool registers as one of these two.

- `Schemas()` includes only `StateEnabled`; `Names()` stays enabled-only (it
  feeds state + transcript); `Get()` answers Enabled tools only. which is what
  `/tools` calls after the first prompt. There is no other promotion method. and
  read-only metadata (`MarkReadOnly`/`ReadOnly`) serve the manager; the
  sub-agent Every mutator nils the schema cache.
## Front end wiring

- `/mcp` lists servers with their state, connects/disconnects, shows recent logs
  from a per-server bounded buffer (stdio stderr plus protocol
- `pkg/command` declares its own small interfaces (`MCPServers`, `MCPGroup`,
  `MCPServerStatus`) so it never imports `pkg/mcp`; `pkg/app`'s adapters back
  them with
- `/tools` groups MCP tools under a per-server header carrying tool count and
  connection state.
## Status

Config, client, bridge and manager lifecycle (first-message load of every
server, disconnect/reload), `/mcp` and read-only marking are implemented and
tested against mcp-go. Deferred loading (`_search`/`_load`), the lazy tool-list
cache and all startup modes have been removed: every configured server is
connected in full on `LoadOnFirstMessage`, so there is no placeholder
registration, schema-drift path, prompt-bloat trade-off or eager/manual
distinction to maintain. Stdio notification handling is deadlock-safe:
`Client.OnNotification` dispatches handlers off mcp-go's reader goroutine and
`list_changed` re-discovery runs serialized with a bounded context (see
*Notifications never block mcp-go's reader*). Reconnection is complete: a stdio
child that exits has its tools unregistered while down, then is reconnected with
capped exponential backoff (`maxReconnectWait`) and its pre-death enable/disable
split restored on the next successful connect.
